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
		return []PushResult{{Outcome: OutcomeResolveFail, Err: cleanErr(orErr(err)), AuthExpired: romm.IsAuthError(err)}}
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

// pushOne uploads a single local save file and classifies the outcome. All the
// integrity work (0-byte guard, upload, conflict retry, server-side verify with
// one full retry) lives in uploadVerified so the staged-file path (PushSaveFile)
// runs the identical gate.
func pushOne(client *romm.Client, cfg *config.Config, rom romm.Rom, sf localSaveFile) PushResult {
	return uploadVerified(client, cfg, rom, sf.path, sf.emuDir)
}

// uploadVerified is the ONE verified save-upload path every push route funnels
// through (session push --push-save, banner --push-pending, manual --sync-save,
// staged flashback snapshots). It:
//
//  1. refuses a zero-byte/unreadable local file (OutcomeEmptyLocalSave — the
//     ghost-proof guard, #63);
//  2. uploads (with the additive overwrite=true retry on RomM's foreign-device
//     409 interlock, unchanged semantics);
//  3. VERIFIES the server actually stored our bytes (verifyUploadedSave: response
//     content_hash → byte re-download → list fallback) BEFORE any outcome that
//     marks the save safe/synced — a bare HTTP 2xx is no longer trusted;
//  4. on a verify failure, retries the WHOLE upload+verify once, then reports
//     OutcomeHashMismatch (the save stays PENDING — never silently synced).
//
// A 401/403-invalid-token anywhere sets AuthExpired for the PAIRING_EXPIRED map.
func uploadVerified(client *romm.Client, cfg *config.Config, rom romm.Rom, savePath, emulator string) PushResult {
	res := PushResult{SaveFile: filepath.Base(savePath), Emulator: emulator}

	if emptyLocalSave(savePath) {
		res.Outcome = OutcomeEmptyLocalSave
		return res
	}

	// PRE-UPLOAD DEDUP (A2): if the server already verifiably holds these exact bytes
	// (non-ghost content_hash match), there is nothing to move — report
	// AlreadyOnServer WITHOUT uploading. Pre-A2 every push route uploaded
	// unconditionally, so each manual "Sync save" of an unchanged save stacked a new
	// identical revision on the timeline (observed: duplicate CIMA rows, Smart Pro
	// 2026-07-02). A failed check (offline) falls through to the upload, whose own
	// error handling stays authoritative.
	if AlreadyOnServer(client, rom.ID, savePath) {
		res.Outcome = OutcomeAlreadyOnServer
		return res
	}

	q := romm.UploadSaveQuery{
		RomID:            rom.ID,
		DeviceID:         deviceID(cfg),
		Emulator:         emulator,
		Slot:             "autosave",
		Autocleanup:      true,
		AutocleanupLimit: 25,
		// CANONICAL NAME (task #126): the local save file is named after the on-card
		// ROM (state marker + coexist disambiguator included) because minarch derives
		// it from exactly that name — but those are DEVICE-LOCAL display artifacts.
		// The server stores the canonical, device-independent name so every device's
		// pushes land in ONE name family (see sync/canonical.go).
		FileName: canonicalSaveUploadName(cfg, rom, savePath),
	}

	uploaded, conflicted, err := uploadWithConflictRetry(client, &q, savePath)
	if err != nil {
		if romm.IsAuthError(err) {
			res.Outcome = OutcomeUploadError
			res.Err = cleanErr(err)
			res.AuthExpired = true
			return res
		}
		// The upload errored — but the content may ALREADY be on the server (an
		// earlier attempt landed despite a flaky response, or another path uploaded
		// it). If so the save is safe; count it done so the banner clears.
		// AlreadyOnServer requires a NON-GHOST content-hash match, so this is a
		// verified outcome too.
		if AlreadyOnServer(client, rom.ID, savePath) {
			res.Outcome = OutcomeAlreadyOnServer
			return res
		}
		res.Outcome = OutcomeUploadError
		res.Err = cleanErr(err)
		return res
	}
	res.Conflicted = conflicted

	// UPLOAD VERIFY: never trust the 2xx alone. One full retry on failure.
	verr := verifyUploadedSave(client, rom.ID, uploaded, savePath)
	if verr != nil && !romm.IsAuthError(verr) {
		uploaded2, conflicted2, err2 := uploadWithConflictRetry(client, &q, savePath)
		if err2 == nil {
			res.Conflicted = res.Conflicted || conflicted2
			verr = verifyUploadedSave(client, rom.ID, uploaded2, savePath)
		}
	}
	if verr != nil {
		if romm.IsAuthError(verr) {
			res.AuthExpired = true
		}
		res.Outcome = OutcomeHashMismatch
		res.Err = firstLine(verr.Error())
		return res
	}

	// HEAL (task #126): with this push verified under the canonical name, delete any
	// marker-named duplicates of this ROM whose bytes verifiably survive under a
	// clean-named record (same non-ghost content hash). Best-effort — never affects
	// the push outcome; a marker-named record with unique bytes is never touched.
	_ = healMarkerTwins(client, rom.ID)

	res.Outcome = OutcomePushed
	return res
}

// uploadWithConflictRetry POSTs the save, and on RomM's foreign-device slot
// interlock (409) retries once with overwrite=true — ADDITIVE: RomM inserts our
// datetime-tagged row alongside the foreign one, deletes nothing. Returns the
// created Save record (for verification), whether the conflict retry was needed,
// and the final error. q is a pointer so the overwrite flag sticks for a
// caller-level verify retry.
func uploadWithConflictRetry(client *romm.Client, q *romm.UploadSaveQuery, savePath string) (romm.Save, bool, error) {
	uploaded, err := client.UploadSave(*q, savePath)
	if err != nil && isSaveConflict(err) {
		q.Overwrite = true
		if uploaded, err = client.UploadSave(*q, savePath); err == nil {
			return uploaded, true, nil
		}
	}
	return uploaded, false, err
}

// AlreadyOnServer reports whether the server already holds a save for romID whose
// content matches the local file (RomM's content_hash is the MD5 of the bytes,
// compared case-insensitively). Lets a save that's been backed up by ANY path count
// as done. Exported so the --push-pending mode can clear an already-uploaded save.
// GHOST-IMMUNE (#63): a record whose bytes are missing/zero-length never counts —
// its hash cannot vouch for content the server doesn't hold.
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
		if IsGhostSave(s) || IsMetaSave(s) {
			continue // neither a ghost's nor a meta record's hash can vouch for save bytes (#63/#146)
		}
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
	// MULTI-USER: the device is the ACTIVE profile device, not Hosts[0]. Using
	// Hosts[0] sent the admin device_id under a viewer token -> 404 on upload.
	if cfg == nil {
		return ""
	}
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
