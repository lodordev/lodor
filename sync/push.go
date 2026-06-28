package sync

import (
	"os"
	"path/filepath"
	"strings"

	"lodor/catalog"
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// localSaveFile is one save file on the card matched to a ROM, with the emulator
// folder it was found in (the value POSTed as the `emulator` query param).
type localSaveFile struct {
	path   string
	emuDir string
}

// PushSaveDirect uploads the device's local save file(s) for ONE ROM straight to
// RomM (POST /api/saves) — the cheap targeted write path (PullSaveDirect is the read
// mirror). romPath is a local on-card ROM path; it is reversed to a rom_id via the
// catalog index, then the full ROM record is fetched so save discovery has fs_name /
// fs_name_no_ext to match against.
//
// Unlike grout (which returned only an uploaded count), this returns a structured
// per-save result slice — one PushResult per local save file attempted — each
// carrying WHY it ended where it did. That makes a stuck save name its own cause in
// the --push-pending log instead of disappearing into "pushed=0 total=2 stuck=2".
//
// Outcomes:
//   - ROM doesn't resolve              → a single {OutcomeResolveFail}
//   - ROM resolves but no local save   → a single {OutcomeNoLocalSave}
//   - per save: uploaded               → {OutcomePushed} (Conflicted set if the
//     additive overwrite=true retry was needed)
//   - per save: errored but on server  → {OutcomeAlreadyOnServer}
//   - per save: errored and not on srv → {OutcomeUploadError, Err: reason}
//
// Versioning semantics (BLUEPRINT §2): each upload lands as a NEW datetime-tagged
// row — we never overwrite in place. autocleanup caps the slot at 25 versions. On a
// foreign-device conflict, overwrite=true is ADDITIVE — it inserts our row alongside
// theirs and deletes nothing.
func PushSaveDirect(client *romm.Client, cfg *config.Config, romPath string) []PushResult {
	romID, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || romID == 0 {
		return []PushResult{{Outcome: OutcomeResolveFail}}
	}
	rom, err := client.GetRom(romID)
	if err != nil || rom.ID == 0 {
		return []PushResult{{Outcome: OutcomeResolveFail, Err: cleanErr(orErr(err))}}
	}

	saves := findLocalSavesForRom(cfg, rom)
	if len(saves) == 0 {
		return []PushResult{{Outcome: OutcomeNoLocalSave}}
	}

	results := make([]PushResult, 0, len(saves))
	for _, sf := range saves {
		results = append(results, pushOne(client, cfg, rom, sf))
	}
	return results
}

// pushOne uploads a single local save file and classifies the outcome.
func pushOne(client *romm.Client, cfg *config.Config, rom romm.Rom, sf localSaveFile) PushResult {
	res := PushResult{SaveFile: filepath.Base(sf.path), Emulator: sf.emuDir}

	q := romm.UploadSaveQuery{
		RomID:            rom.ID,
		DeviceID:         deviceID(cfg),
		Emulator:         sf.emuDir,
		Slot:             "autosave",
		Autocleanup:      true,
		AutocleanupLimit: 25,
	}

	uploaded, err := client.UploadSave(q, sf.path)
	if err != nil && isSaveConflict(err) {
		// Foreign-device interlock fired: another device added a save in this slot we
		// never pulled. overwrite=true is additive (RomM inserts our datetime-tagged
		// row, deletes theirs nowhere) — add ours alongside and flag the conflict.
		q.Overwrite = true
		if uploaded, err = client.UploadSave(q, sf.path); err == nil {
			if !verifyUploadedContent(client, cfg, rom.ID, sf.path, uploaded) {
				res.Outcome = OutcomeUploadError
				res.Err = "upload left no retrievable content (ghost) — kept pending"
				return res
			}
			res.Outcome = OutcomePushed
			res.Conflicted = true
			return res
		}
	}
	if err != nil {
		// The upload errored — but the content may ALREADY be on the server (an
		// earlier attempt landed despite a flaky response, or another path uploaded
		// it). If so the save is safe; count it done so the banner clears.
		if AlreadyOnServer(client, rom.ID, sf.path) {
			res.Outcome = OutcomeAlreadyOnServer
			return res
		}
		res.Outcome = OutcomeUploadError
		res.Err = cleanErr(err)
		return res
	}

	// Ghost PREVENTION (#63): the POST returned a record, but verify its CONTENT is
	// actually retrievable before declaring success — otherwise we'd leave a ghost
	// (record present, bytes missing) AND clear the pending banner on a save that can
	// never be restored. If the content isn't there, keep it pending (UploadError) so
	// it retries on the next sync instead of silently rotting.
	if !verifyUploadedContent(client, cfg, rom.ID, sf.path, uploaded) {
		res.Outcome = OutcomeUploadError
		res.Err = "upload left no retrievable content (ghost) — kept pending"
		return res
	}

	res.Outcome = OutcomePushed
	return res
}

// verifyUploadedContent confirms a just-uploaded save's CONTENT is retrievable from the
// server (ghost prevention, #63). A successful POST /api/saves returns a record, but a
// record without retrievable bytes is a "ghost" (content GET = 404 / empty) that fails
// every later restore. We GET the content once; on a ghost we re-upload ONCE and re-check.
// Returns true if the content is retrievable (directly, after the retry, or because the
// SAME bytes are already on the server under another revision — AlreadyOnServer). Failing
// closed (false) keeps the save pending rather than falsely clearing the banner.
func verifyUploadedContent(client *romm.Client, cfg *config.Config, romID int, localPath string, uploaded romm.Save) bool {
	check := func(id int) bool {
		if id == 0 {
			return false
		}
		data, err := client.DownloadSaveContent(id, deviceID(cfg), false)
		return err == nil && len(data) > 0
	}
	if check(uploaded.ID) {
		return true
	}
	// One re-upload attempt, then re-verify (handles a transient server-side write miss).
	q := romm.UploadSaveQuery{
		RomID: romID, DeviceID: deviceID(cfg), Emulator: uploaded.Emulator,
		Slot: "autosave", Overwrite: true, Autocleanup: true, AutocleanupLimit: 25,
	}
	if again, err := client.UploadSave(q, localPath); err == nil && check(again.ID) {
		return true
	}
	// Last resort: the identical bytes may already be retrievable under another revision.
	return AlreadyOnServer(client, romID, localPath)
}

// AlreadyOnServer reports whether the server already holds a save for romID whose
// content matches the local file (RomM's content_hash is the MD5 of the bytes,
// compared case-insensitively). Lets a save that's been backed up by ANY path count
// as done. Exported so the --push-pending mode can clear an already-uploaded save.
func AlreadyOnServer(client *romm.Client, romID int, localPath string) bool {
	local, ok := fileMD5(localPath)
	if !ok {
		return false
	}
	saves, err := client.GetSaves(romm.SaveQuery{RomID: romID})
	if err != nil {
		return false
	}
	for _, s := range saves {
		if s.ContentHash != nil && strings.EqualFold(*s.ContentHash, local) {
			return true
		}
	}
	return false
}

// findLocalSavesForRom returns the save files on the card that belong to this ROM,
// across the platform's save directories (BLUEPRINT §2). A file matches when,
// stripping its save extension, its stem equals the ROM's full on-disk name
// ("Game (USA).gba.sav" — minarch) or its name-without-extension ("Game (USA).srm" —
// RetroArch). Hidden files and directories are skipped; only ValidSaveExtensions
// count.
func findLocalSavesForRom(cfg *config.Config, rom romm.Rom) []localSaveFile {
	var out []localSaveFile
	// The on-disk save is named after the ACTUAL on-disk ROM basename, which carries the
	// mode disambiguator (" (RomM)" in separate/merge mode) and any leading state marker
	// ("[^] "/"[v] "). Match the marker-stripped save stem against every name the same ROM
	// can occupy: the server fs_name (own mode) and the mode-aware LocalBasename
	// (separate/merge), each with and without the ROM extension (minarch ".sav" appends to
	// the full filename; RetroArch ".srm" replaces the extension).
	localBase := ""
	if p := platform.LocalRomPath(cfg, rom); p != "" {
		localBase = filepath.Base(p)
	}
	localNoExt := strings.TrimSuffix(localBase, filepath.Ext(localBase))
	for _, emuDir := range platform.EmulatorFoldersForFSSlug(rom.PlatformFsSlug) {
		dir := filepath.Join(platform.SavesDir(), emuDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if !ValidSaveExtensions[strings.ToLower(filepath.Ext(e.Name()))] {
				continue
			}
			stem := platform.StripLeadingMarker(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
			if stem == rom.FsName || stem == rom.FsNameNoExt ||
				(localBase != "" && (stem == localBase || stem == localNoExt)) {
				out = append(out, localSaveFile{
					path:   filepath.Join(dir, e.Name()),
					emuDir: emuDir,
				})
			}
		}
	}
	return out
}

// deviceID returns the configured device_id of the first host, or "" if none.
func deviceID(cfg *config.Config) string {
	if cfg == nil || len(cfg.Hosts) == 0 {
		return ""
	}
	// MULTI-USER: the device_id is the ACTIVE profile's, so a save pushes under the
	// right user/device (hosts[0]'s own when no profile is selected).
	return cfg.ActiveHost().DeviceID
}

// orErr returns a non-nil error so cleanErr always has something to read; if err is
// nil it returns a sentinel meaning the ROM record came back empty.
func orErr(err error) error {
	if err != nil {
		return err
	}
	return errEmptyRom
}

type syncErr string

func (e syncErr) Error() string { return string(e) }

const errEmptyRom = syncErr("rom record empty")
