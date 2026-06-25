package sync

import (
	"fmt"
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
	localPath = saveLocalPath(cfg, rom, newest.FileExtension)
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
	localPath := saveLocalPath(cfg, rom, save.FileExtension)
	if localPath == "" {
		return PullResult{Outcome: PullResolveFail, Reason: "no save directory"}
	}

	// Rewind Pillar A — lose-proof. Before this restore overwrites the live save,
	// preserve the device's CURRENT save by pushing it to the server timeline first.
	// A rewind to an older point can then never silently destroy unsynced progress:
	// that progress becomes its own permanent point you can rewind back to. Content-
	// hash dedup makes it a no-op when the current save is already backed up. If a real
	// local save exists and CANNOT be preserved, the restore is aborted (mirroring
	// "if the backup fails, abort") rather than trading known progress for old bytes.
	if ok, pushed, reason := snapshotLocalSaves(client, cfg, rom); !ok {
		fmt.Fprintf(os.Stderr, "rewind: restore ABORTED — %s (rom %d)\n", reason, rom.ID)
		return PullResult{Outcome: PullError, Reason: reason}
	} else if pushed > 0 {
		fmt.Fprintf(os.Stderr, "rewind: preserved %d current save(s) to the timeline before restore (rom %d)\n", pushed, rom.ID)
	}

	if res := writeSave(client, cfg, save.ID, localPath); res.Outcome != PullWritten {
		return res
	}
	return PullResult{Outcome: PullWritten, LocalPath: localPath}
}

// snapshotLocalSaves preserves the device's current local save(s) for a ROM by pushing
// them to the server timeline BEFORE a restore overwrites them (Rewind Pillar A). It
// reuses the already-resolved rom (no extra GetRom). ok is true when it is SAFE to
// proceed with the overwrite — either nothing local exists to lose, or every local
// save is now on the server (freshly pushed, or already there by content hash). ok is
// false ONLY when a real local save could not be preserved (a genuine upload error),
// so the caller can abort instead of destroying unsynced progress. pushed counts saves
// newly secured this call (for an honest log line); reason is host-free.
func snapshotLocalSaves(client *romm.Client, cfg *config.Config, rom romm.Rom) (ok bool, pushed int, reason string) {
	saves := findLocalSavesForRom(rom.PlatformFsSlug, rom)
	if len(saves) == 0 {
		return true, 0, "" // nothing on the device to lose
	}
	for _, sf := range saves {
		switch pushOne(client, cfg, rom, sf).Outcome {
		case OutcomePushed, OutcomeAlreadyOnServer:
			pushed++
		case OutcomeUploadError, OutcomeHashMismatch:
			return false, pushed, "couldn't preserve current save before rewind"
		}
	}
	return true, pushed, ""
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
	localPath := saveLocalPath(cfg, rom, saveExt)
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
func saveLocalPath(cfg *config.Config, rom romm.Rom, saveExt string) string {
	saveDir := platform.SaveDirectory(rom.PlatformFsSlug)
	if saveDir == "" {
		return ""
	}
	romBase := romBasename(cfg, rom)
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
	if err != nil || len(data) == 0 {
		return PullResult{Outcome: PullError, Reason: "download failed"}
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
