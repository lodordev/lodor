package sync

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"lodor/catalog"
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// PullOutcome is the result of a pull/restore attempt. Pull is a single-file
// operation so a plain enum (not a slice) suffices; the cmd layer maps it to the
// RESULT pulled=<0|1> line and, on failure, a reason.
type PullOutcome int

const (
	// PullWritten: a newer server save was downloaded and written (local backed up
	// to .bak).
	PullWritten PullOutcome = iota
	// PullNoServerSave: the server has no save for this ROM yet — a normal no-op.
	PullNoServerSave
	// PullLocalNewer: the local save is newer than or equal to the server's — kept,
	// no-op (newest-wins).
	PullLocalNewer
	// PullResolveFail: the ROM couldn't be resolved, or has no save directory.
	PullResolveFail
	// PullError: a download/write failure (Reason carries a short host-free string).
	PullError
	// PullSnapshotFail: the restore was ABORTED by the Flashback lose-proof guard because
	// the device's current save could not be preserved to the timeline first. Nothing
	// was overwritten — distinct from PullError so the UI can say "your current save
	// wasn't safe to overwrite" instead of a generic failure.
	PullSnapshotFail
	// PullGhostSave: the server has a save RECORD for this id but its content GET returned
	// 404 — the metadata row exists, the bytes are missing (a "ghost" save). Distinct from
	// PullError so the UI says "this save is unavailable on the server" instead of the
	// cryptic "download failed" / reason=download. Nothing was overwritten.
	PullGhostSave
)

func (o PullOutcome) String() string {
	switch o {
	case PullWritten:
		return "Written"
	case PullNoServerSave:
		return "NoServerSave"
	case PullLocalNewer:
		return "LocalNewer"
	case PullResolveFail:
		return "ResolveFail"
	case PullError:
		return "Error"
	case PullSnapshotFail:
		return "SnapshotFail"
	case PullGhostSave:
		return "GhostSave"
	default:
		return "Unknown"
	}
}

// PullResult is the structured return of PullSaveDirect / RestoreSave: the outcome,
// the local path written (when one was), and a short host-free reason on failure.
type PullResult struct {
	Outcome   PullOutcome
	LocalPath string
	Reason    string
}

// Pulled reports whether a file was actually written (the RESULT pulled=<0|1> bit).
func (r PullResult) Pulled() bool { return r.Outcome == PullWritten }

// PullSaveDirect downloads the newest server save for ONE ROM and writes it to the
// local save file, bypassing the full-library negotiate (BLUEPRINT §2). It is the
// read mirror of the device's cheap upload.
//
// Non-destructive + newest-wins: it overwrites the local save ONLY when the server's
// copy is strictly newer than the local file's mtime, and it renames the existing
// local save to "<name>.bak" before replacing it (write .tmp → atomic rename), so a
// pull can never lose a save.
func PullSaveDirect(client *romm.Client, cfg *config.Config, romPath string) PullResult {
	rom, localPath, ok := resolveRomAndLocalSavePath(client, cfg, romPath, "")
	if !ok {
		return PullResult{Outcome: PullResolveFail, Reason: "not matched to a RomM game"}
	}

	saves, err := client.GetSaves(romm.SaveQuery{RomID: rom.ID})
	if err != nil {
		return PullResult{Outcome: PullError, Reason: "couldn't reach server"}
	}
	if len(saves) == 0 {
		return PullResult{Outcome: PullNoServerSave}
	}
	newest := newestSave(saves)

	// localPath was computed with the placeholder extension above; recompute with the
	// chosen save's real extension so the on-disk name matches what minarch reads.
	localPath = saveLocalPath(cfg, rom, romPath, newest.FileExtension)
	if localPath == "" {
		return PullResult{Outcome: PullResolveFail, Reason: "no save directory"}
	}

	// newest-wins: never clobber a local save newer than (or equal age to) the server's.
	if info, statErr := os.Stat(localPath); statErr == nil {
		if !newest.UpdatedAt.After(info.ModTime()) {
			return PullResult{Outcome: PullLocalNewer, LocalPath: localPath}
		}
	}

	if res := writeSave(client, cfg, newest.ID, localPath); res.Outcome != PullWritten {
		return res
	}
	return PullResult{Outcome: PullWritten, LocalPath: localPath}
}

// RestoreSave downloads ONE explicit server save by id and writes it to the local
// save file (BLUEPRINT §2). Unlike PullSaveDirect it applies NO age check — the user
// explicitly chose this version — but it is equally non-destructive: the existing
// local save is renamed to .bak, and the new bytes are written .tmp → atomic rename.
//
// save carries the chosen record (its FileExtension picks the local filename and its
// ID names the content endpoint). romPath identifies the ROM for the save directory.
func RestoreSave(client *romm.Client, cfg *config.Config, romPath string, save romm.Save) PullResult {
	rom, _, ok := resolveRomAndLocalSavePath(client, cfg, romPath, save.FileExtension)
	if !ok {
		return PullResult{Outcome: PullResolveFail, Reason: "not matched to a RomM game"}
	}
	localPath := saveLocalPath(cfg, rom, romPath, save.FileExtension)
	if localPath == "" {
		return PullResult{Outcome: PullResolveFail, Reason: "no save directory"}
	}

	// Pure overwrite. Preserving the device's CURRENT save before this lands (Flashback
	// Pillar A, lose-proof) is the CALLER's job (cmd runRestoreSave), because it must
	// work OFFLINE: when the current save can't be pushed to the timeline right now, the
	// caller stages its bytes and queues them for a later upload rather than blocking the
	// flashback. writeSave still renames the existing local save to .bak as a local net.
	if res := writeSave(client, cfg, save.ID, localPath); res.Outcome != PullWritten {
		return res
	}
	return PullResult{Outcome: PullWritten, LocalPath: localPath}
}

// LocalSaveFilesForRom returns the absolute paths of the save file(s) currently on the
// card for the ROM at romPath — the bytes a flashback is about to overwrite. Exported so
// the cmd layer can stage them for deferred upload before calling RestoreSave. Empty when
// the ROM doesn't resolve or has no local save yet.
func LocalSaveFilesForRom(client *romm.Client, cfg *config.Config, romPath string) []string {
	rom, _, ok := resolveRomAndLocalSavePath(client, cfg, romPath, "")
	if !ok {
		return nil
	}
	var out []string
	for _, sf := range findLocalSavesForRom(cfg, rom) {
		out = append(out, sf.path)
	}
	return out
}

// LocalSaveHashesForRom returns the lower-case MD5 content hashes of the save file(s)
// currently on the card for the ROM at romPath — the SAME signal RomM stores as a save's
// content_hash (see AlreadyOnServer). Used by --list-saves to mark which server revision
// matches the bytes currently on the device. Empty when there's no local save or none read.
func LocalSaveHashesForRom(client *romm.Client, cfg *config.Config, romPath string) []string {
	var out []string
	for _, p := range LocalSaveFilesForRom(client, cfg, romPath) {
		if sum, ok := fileMD5(p); ok {
			out = append(out, sum)
		}
	}
	return out
}

// PushSaveFile uploads ONE explicit save file to the timeline for the ROM at romPath,
// independent of what's currently in the save directory. This is how a STAGED pre-
// flashback save (copied aside before the overwrite) reaches the server later via
// --push-pending, even though the live save file now holds the flashed-back bytes.
// emulator labels the timeline point's origin; "" is acceptable.
func PushSaveFile(client *romm.Client, cfg *config.Config, romPath, filePath, emulator string) PushResult {
	res := PushResult{SaveFile: filepath.Base(filePath), Emulator: emulator}
	romID, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || romID == 0 {
		res.Outcome = OutcomeResolveFail
		return res
	}
	rom, err := client.GetRom(romID)
	if err != nil || rom.ID == 0 {
		res.Outcome = OutcomeResolveFail
		res.Err = cleanErr(orErr(err))
		return res
	}
	q := romm.UploadSaveQuery{
		RomID: rom.ID, DeviceID: deviceID(cfg), Emulator: emulator,
		Slot: "autosave", Autocleanup: true, AutocleanupLimit: 25,
	}
	uploaded, err := client.UploadSave(q, filePath)
	if err != nil {
		if AlreadyOnServer(client, rom.ID, filePath) {
			res.Outcome = OutcomeAlreadyOnServer
			return res
		}
		res.Outcome = OutcomeUploadError
		res.Err = cleanErr(err)
		return res
	}
	// Ghost PREVENTION (#63): verify the uploaded staged save's content is retrievable;
	// keep it pending (UploadError) rather than declaring success on a ghost.
	if !verifyUploadedContent(client, cfg, rom.ID, filePath, uploaded) {
		res.Outcome = OutcomeUploadError
		res.Err = "upload left no retrievable content (ghost) — kept pending"
		return res
	}
	res.Outcome = OutcomePushed
	return res
}

// resolveRomAndLocalSavePath reverses romPath to a rom_id (catalog index), fetches
// the full ROM, and computes the local save path using saveExt for the filename. ok
// is false if the ROM can't be resolved or the platform has no save directory.
func resolveRomAndLocalSavePath(client *romm.Client, cfg *config.Config, romPath, saveExt string) (romm.Rom, string, bool) {
	romID, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || romID == 0 {
		return romm.Rom{}, "", false
	}
	rom, err := client.GetRom(romID)
	if err != nil || rom.ID == 0 {
		return romm.Rom{}, "", false
	}
	localPath := saveLocalPath(cfg, rom, romPath, saveExt)
	if localPath == "" {
		return rom, "", false
	}
	return rom, localPath, true
}

// saveLocalPath computes the on-disk save path for a ROM and a save extension:
// <SaveDirectory(fs_slug)>/<SaveFileName(romBase, ext)>. romBase is the ROM's full
// on-disk filename (e.g. "Game (USA).gba") — the same basename grout passed — derived
// from the ROM's local ROM path. Returns "" when the platform has no save directory
// or the ROM has no resolvable on-disk file.
func saveLocalPath(cfg *config.Config, rom romm.Rom, romPath, saveExt string) string {
	saveDir := platform.SaveDirectory(rom.PlatformFsSlug)
	if saveDir == "" {
		return ""
	}
	// The save name must mirror the ACTUAL on-disk ROM filename the emulator launched —
	// including any leading state marker ("[v] ") and mode disambiguator (" (RomM)") —
	// because minarch derives "<rom filename>.sav" from exactly that name. Prefer the real
	// launched path; reconstruct the canonical name only when no path was supplied.
	romBase := filepath.Base(romPath)
	if romPath == "" || romBase == "." || romBase == string(filepath.Separator) {
		romBase = romBasename(cfg, rom)
	}
	if romBase == "" {
		return ""
	}
	ext := strings.TrimPrefix(saveExt, ".")
	return filepath.Join(saveDir, platform.SaveFileName(romBase, ext))
}

// romBasename returns the ROM's full on-disk basename ("Game (USA).gba"), matching
// grout's filepath.Base(romPath). Falls back to fs_name if the local ROM path can't
// be built (e.g. no directory mapping in this config).
func romBasename(cfg *config.Config, rom romm.Rom) string {
	if p := platform.LocalRomPath(cfg, rom); p != "" {
		return filepath.Base(p)
	}
	return rom.FsName
}

// writeSave downloads the save content (optimistic=false, sent literally) and writes
// it non-destructively: back up an existing local file to .bak, write .tmp, atomic
// rename. Returns a PullWritten result on success or a PullError with a host-free
// reason.
func writeSave(client *romm.Client, cfg *config.Config, saveID int, localPath string) PullResult {
	data, err := client.DownloadSaveContent(saveID, deviceID(cfg), false)
	if err != nil {
		// Ghost save: the record exists but its content GET = 404 (bytes missing on the
		// server). Surface a CLEAR, distinct outcome instead of the cryptic "download
		// failed" so the restore/list-saves UI can flag it as unavailable. (#63)
		var se *romm.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return PullResult{Outcome: PullGhostSave, Reason: "save unavailable on server (no content)"}
		}
		return PullResult{Outcome: PullError, Reason: "download failed"}
	}
	if len(data) == 0 {
		// An empty body for an existing record is also an unusable (ghost-like) save.
		return PullResult{Outcome: PullGhostSave, Reason: "save unavailable on server (empty content)"}
	}
	saveDir := filepath.Dir(localPath)
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		return PullResult{Outcome: PullError, Reason: "save dir not writable"}
	}
	if _, statErr := os.Stat(localPath); statErr == nil {
		_ = os.Rename(localPath, localPath+".bak")
	}
	tmp := localPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return PullResult{Outcome: PullError, Reason: "write failed"}
	}
	if err := os.Rename(tmp, localPath); err != nil {
		_ = os.Remove(tmp)
		return PullResult{Outcome: PullError, Reason: "write failed"}
	}
	return PullResult{Outcome: PullWritten, LocalPath: localPath}
}

// newestSave returns the save with the latest UpdatedAt.
func newestSave(saves []romm.Save) romm.Save {
	newest := saves[0]
	for i := 1; i < len(saves); i++ {
		if saves[i].UpdatedAt.After(newest.UpdatedAt) {
			newest = saves[i]
		}
	}
	return newest
}
