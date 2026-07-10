package sync

import (
	"os"
	"path/filepath"
	"strings"

	"lodor/catalog"
	"lodor/config"
	"lodor/fsutil"
	"lodor/platform"
	"lodor/romm"
)

// PullOutcome is the result of a pull/restore attempt. Pull is a single-file
// operation so a plain enum (not a slice) suffices; the cmd layer maps it to the
// RESULT pulled=<0|1> line and, on failure, a reason.
type PullOutcome int

const (
	// PullWritten: a server save was downloaded and written (local backed up
	// to .bak). Reason says why the overwrite was safe: "no-local" (nothing on the
	// card yet) or "older-lineage" (the local bytes match an OLDER server revision).
	PullWritten PullOutcome = iota
	// PullNoServerSave: the server has no save for this ROM yet — a normal no-op.
	PullNoServerSave
	// PullInSync: the local save's content already matches the NEWEST server save —
	// kept, no-op (Reason "in-sync"). Formerly PullLocalNewer, renamed with the A2
	// switch from mtime-vs-updated_at to content-hash lineage: "newer" was never a
	// provable claim on an RTC-less handheld, "same bytes as newest" is.
	PullInSync
	// PullLocalUnpushed: the local save's content matches NO server revision — it is
	// unpushed local progress and must NEVER be overwritten (Reason
	// "unpushed-local"). The caller owns pushing it through the verified-upload
	// funnel (runSyncSave's push leg / runPullSaves' explicit push), after which it
	// IS the newest revision and no pull is needed.
	PullLocalUnpushed
	// PullResolveFail: the ROM couldn't be resolved, or has no save directory.
	PullResolveFail
	// PullError: a download/write/reachability failure (Reason carries a short
	// host-free string).
	PullError
	// PullSnapshotFail: the restore was ABORTED by the Flashback lose-proof guard because
	// the device's current save could not be preserved to the timeline first. Nothing
	// was overwritten — distinct from PullError so the UI can say "your current save
	// wasn't safe to overwrite" instead of a generic failure.
	PullSnapshotFail
	// PullTombstoned: the local save file is ABSENT because it was DELETED on this
	// device after a sync — the save ledger (saveledger.go) holds the revision this
	// device last verifiably synced, and the server's newest is that revision or
	// older — so the pull is SKIPPED and the deletion sticks (Reason "tombstone").
	// A strictly newer server revision still pulls (another device advanced the
	// game), and an explicit restore (RestoreSave) or PullOptions.IncludeDeleted
	// always bypasses this. Nothing is ever deleted server-side — the tombstone
	// only stops resurrection on THIS device.
	PullTombstoned
)

func (o PullOutcome) String() string {
	switch o {
	case PullWritten:
		return "Written"
	case PullNoServerSave:
		return "NoServerSave"
	case PullInSync:
		return "InSync"
	case PullLocalUnpushed:
		return "LocalUnpushed"
	case PullResolveFail:
		return "ResolveFail"
	case PullError:
		return "Error"
	case PullSnapshotFail:
		return "SnapshotFail"
	case PullTombstoned:
		return "Tombstoned"
	default:
		return "Unknown"
	}
}

// PullResult is the structured return of PullSaveDirect / RestoreSave: the outcome,
// the local path written (when one was), and a short host-free reason on failure.
//   - Ghosts      — how many of the server's save records for this ROM are GHOSTS
//     (record present, bytes missing/zero — see IsGhostSave). Ghosts are excluded
//     from newest-wins before it runs, so a ghost can never overwrite a real local
//     save; the count lets the UI surface "N broken saves on server".
//   - AuthExpired — the server rejected our token (romm.AuthError): pairing is
//     expired/revoked; the cmd layer maps it to PAIRING_EXPIRED (exit 6).
type PullResult struct {
	Outcome     PullOutcome
	LocalPath   string
	Reason      string
	Ghosts      int
	AuthExpired bool
}

// Pulled reports whether a file was actually written (the RESULT pulled=<0|1> bit).
func (r PullResult) Pulled() bool { return r.Outcome == PullWritten }

// PullSaveDirect downloads the newest server save for ONE ROM and writes it to the
// local save file, bypassing the full-library negotiate (BLUEPRINT §2). It is the
// read mirror of the device's cheap upload.
//
// CONTENT-HASH LINEAGE (workstream A2, 2026-07-02): the overwrite decision compares
// the LOCAL save's MD5 against the server save set's content_hash — never file
// mtime vs updated_at. Clock comparison was fragile-by-design on RTC-less handhelds
// (a device that boots with a skewed clock writes save mtimes from the future and
// silently blocks every pull). updated_at now ORDERS the server set only (which
// revision is "newest"); content decides:
//
//	no local save                     → pull newest              (Reason "no-local")
//	local == newest server revision   → in sync, no-op           (Reason "in-sync")
//	local == an OLDER server revision → pull newest, .bak kept   (Reason "older-lineage")
//	local matches NO server revision  → NEVER overwrite: it is unpushed local
//	                                    progress (Reason "unpushed-local"); the
//	                                    caller pushes it via the verified funnel
//
// Non-destructive: any overwrite renames the existing local save to "<name>.bak"
// first (write .tmp → atomic rename), so even a wrong decision can never lose bytes.
// Ghost records (#63) are excluded before any of this runs.
//
// DELETED-SAVE TOMBSTONE: "no local save" is refined by the save ledger
// (saveledger.go) — when this device has a ledgered synced revision and the
// server's newest is that revision or OLDER, the absence is a deliberate local
// delete and the pull is SKIPPED (PullTombstoned, Reason "tombstone") instead of
// resurrecting the save. A strictly newer server revision still pulls; no ledger
// row (fresh card / reinstall / pre-tombstone history) pulls exactly as before;
// the local-file-EXISTS paths never consult the ledger. See PullOptions for the
// explicit escape hatch.
func PullSaveDirect(client *romm.Client, cfg *config.Config, romPath string) PullResult {
	return PullSaveDirectOpts(client, cfg, romPath, PullOptions{})
}

// PullOptions tunes PullSaveDirectOpts. The zero value is the default behavior
// (tombstones honored).
type PullOptions struct {
	// IncludeDeleted bypasses the deleted-save tombstone skip: the newest server
	// save is pulled even when this device's ledger says its local copy was
	// deliberately deleted after a sync. This is the --include-deleted escape
	// hatch for bulk pulls; the explicit restore path (RestoreSave) never
	// consults the tombstone at all and doesn't need it.
	IncludeDeleted bool
}

// PullSaveDirectOpts is PullSaveDirect with explicit options (see PullOptions).
func PullSaveDirectOpts(client *romm.Client, cfg *config.Config, romPath string, opt PullOptions) PullResult {
	rom, _, fail, ok := resolveRomAndLocalSavePath(client, cfg, romPath, "")
	if !ok {
		return fail
	}

	saves, err := client.GetSaves(romm.SaveQuery{RomID: rom.ID})
	if err != nil {
		return PullResult{Outcome: PullError, Reason: "couldn't reach server", AuthExpired: romm.IsAuthError(err)}
	}
	// CROSS-DEVICE PREVIEW (#149): the raw list is already in hand — land the
	// newest .lodorshot.png at the local .minui convention. Best-effort/cosmetic;
	// zero extra requests when none exists or the local copy already matches.
	pullPreviewBestEffort(client, romPath, saves)
	// GHOST FILTER (#63): drop records whose bytes are missing/zero BEFORE the
	// lineage decision runs — a ghost, however new its timestamp, has no bytes and
	// can neither be "newest" nor vouch for local content. All-ghosts == no usable
	// server save.
	real, ghosts := SplitGhosts(saves)
	if len(real) == 0 {
		return PullResult{Outcome: PullNoServerSave, Ghosts: ghosts}
	}
	newest := newestSave(real)

	// localPath was computed with the placeholder extension above; recompute with the
	// chosen save's real extension so the on-disk name matches what minarch reads.
	localPath := saveLocalPath(cfg, rom, romPath, newest.FileExtension)
	if localPath == "" {
		return PullResult{Outcome: PullResolveFail, Reason: "no save directory", Ghosts: ghosts}
	}

	if _, statErr := os.Stat(localPath); statErr != nil {
		// No local save on the card — but that is NOT always "first play": if the
		// save ledger shows this device previously SYNCED a revision and the server
		// holds nothing newer, the file's absence means the user DELETED it, and
		// pulling would resurrect a deleted save (the Argosy-2.0-tombstone gap).
		// A strictly newer server revision still pulls (another device advanced
		// the game); a missing/corrupt ledger reads as no record (pull as always);
		// explicit restores never come through here (RestoreSave bypasses by design).
		if !opt.IncludeDeleted && SaveTombstoned(cfg, rom.ID, newest) {
			return PullResult{Outcome: PullTombstoned, Reason: "tombstone", Ghosts: ghosts}
		}
		// No local save yet (first play of this game on this device): pull newest.
		if res := writeSave(client, cfg, newest.ID, localPath); res.Outcome != PullWritten {
			res.Ghosts = ghosts
			return res
		}
		// SYNCED MOMENT: the newest revision verifiably landed on the card — it is
		// the tombstone baseline a later local delete is judged against.
		recordSyncedSave(cfg, rom.ID, newest, localPath)
		return PullResult{Outcome: PullWritten, LocalPath: localPath, Reason: "no-local", Ghosts: ghosts}
	}

	localMD5, hashOK := fileMD5(localPath)
	if !hashOK {
		// Can't fingerprint the local file — treat as unpushed: refusing to overwrite
		// what we can't prove is on the server is the only lose-proof choice.
		return PullResult{Outcome: PullLocalUnpushed, LocalPath: localPath, Reason: "unpushed-local", Ghosts: ghosts}
	}
	matches := func(s romm.Save) bool {
		return s.ContentHash != nil && *s.ContentHash != "" && strings.EqualFold(*s.ContentHash, localMD5)
	}

	if matches(newest) {
		// SYNCED MOMENT: local == newest is as synced as a pull gets — refresh the
		// ledger so a later local delete tombstones against exactly this revision.
		recordSyncedSave(cfg, rom.ID, newest, localPath)
		return PullResult{Outcome: PullInSync, LocalPath: localPath, Reason: "in-sync", Ghosts: ghosts}
	}
	for _, s := range real {
		if s.ID != newest.ID && matches(s) {
			// The local bytes verifiably live on the server as an OLDER revision —
			// pulling newest cannot lose progress (.bak kept regardless).
			if res := writeSave(client, cfg, newest.ID, localPath); res.Outcome != PullWritten {
				res.Ghosts = ghosts
				return res
			}
			// SYNCED MOMENT: the newest revision is now the on-card save.
			recordSyncedSave(cfg, rom.ID, newest, localPath)
			return PullResult{Outcome: PullWritten, LocalPath: localPath, Reason: "older-lineage", Ghosts: ghosts}
		}
	}
	// Local content matches NO server revision (this also covers a hash-less server
	// set, where lineage can't be proven): unpushed local progress — never overwrite.
	return PullResult{Outcome: PullLocalUnpushed, LocalPath: localPath, Reason: "unpushed-local", Ghosts: ghosts}
}

// RestoreSave downloads ONE explicit server save by id and writes it to the local
// save file (BLUEPRINT §2). Unlike PullSaveDirect it applies NO age check — the user
// explicitly chose this version — but it is equally non-destructive: the existing
// local save is renamed to .bak, and the new bytes are written .tmp → atomic rename.
//
// TOMBSTONE BYPASS (by design): explicit user intent always resurrects — this path
// never consults the deleted-save tombstone (SaveTombstoned). It DOES refresh the
// save ledger on success: the restored revision becomes the device's new synced
// baseline, so a later delete tombstones against what the user actually chose.
//
// save carries the chosen record (its FileExtension picks the local filename and its
// ID names the content endpoint). romPath identifies the ROM for the save directory.
func RestoreSave(client *romm.Client, cfg *config.Config, romPath string, save romm.Save) PullResult {
	// META GUARD (#146): a .lodortime/.lodorshot.png sidecar record is not a
	// save — flashing one over a real local save file would destroy it. The
	// listing already hides meta records; this covers a direct CLI call.
	if IsMetaSave(save) {
		return PullResult{Outcome: PullError, Reason: "not a game save (meta record)"}
	}
	// GHOST GUARD (#63): a record with no stored bytes can't be restored — refuse
	// up front (the listing already hides ghosts; this covers a direct CLI call)
	// rather than overwrite a real local save with nothing.
	if IsGhostSave(save) {
		return PullResult{Outcome: PullError, Reason: "broken save on server (no bytes)", Ghosts: 1}
	}
	_, localPath, fail, ok := resolveRomAndLocalSavePath(client, cfg, romPath, save.FileExtension)
	if !ok {
		return fail
	}

	// Pure overwrite. Preserving the device's CURRENT save before this lands (Flashback
	// Pillar A, lose-proof) is the CALLER's job (cmd runRestoreSave), because it must
	// work OFFLINE: when the current save can't be pushed to the timeline right now, the
	// caller stages its bytes and queues them for a later upload rather than blocking the
	// flashback. writeSave still renames the existing local save to .bak as a local net.
	if res := writeSave(client, cfg, save.ID, localPath); res.Outcome != PullWritten {
		return res
	}
	// SYNCED MOMENT: the explicitly chosen revision is now the on-card save and the
	// device's new tombstone baseline (see the TOMBSTONE BYPASS note above). The rom
	// id comes from the save record — resolveRomAndLocalSavePath already resolved it.
	recordSyncedSave(cfg, save.RomID, save, localPath)
	return PullResult{Outcome: PullWritten, LocalPath: localPath}
}

// LocalSaveFilesForRom returns the absolute paths of the save file(s) currently on the
// card for the ROM at romPath — the bytes a flashback is about to overwrite. Exported so
// the cmd layer can stage them for deferred upload before calling RestoreSave. Empty when
// the ROM doesn't resolve or has no local save yet.
func LocalSaveFilesForRom(client *romm.Client, cfg *config.Config, romPath string) []string {
	rom, _, _, ok := resolveRomAndLocalSavePath(client, cfg, romPath, "")
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

// PrimaryLocalSaveFilesForRomPath returns the save file(s) the emulator will ACTUALLY
// load when launching the ROM at romPath — the files whose (marker-stripped) stem equals
// the LAUNCHED file's own basename — as opposed to LocalSaveFilesForRom, which returns
// every file the same rom_id can occupy (both coexist twins). The distinction is the
// 2026-07-03 Smart Pro field bug (task #135): with the clean-named twin AND the
// " (RomM)" twin both carrying saves, --list-saves matched the OTHER twin's save against
// the newest server revision and emitted LOCAL=current, so the pre-launch hook silently
// launched the clean twin into its OLDER save. Empty when the ROM doesn't resolve or the
// launched name has no save yet — even if a twin does, because the launch loads nothing.
func PrimaryLocalSaveFilesForRomPath(client *romm.Client, cfg *config.Config, romPath string) []string {
	rom, _, _, ok := resolveRomAndLocalSavePath(client, cfg, romPath, "")
	if !ok {
		return nil
	}
	return primaryLocalSaves(cfg, rom, romPath)
}

// primaryLocalSaves is the pure (network-free) core of PrimaryLocalSaveFilesForRomPath:
// filter findLocalSavesForRom down to stems matching the launched basename — with the
// ROM extension (minarch appends ".sav" to the full filename) and without it (RetroArch
// ".srm" replaces the extension).
func primaryLocalSaves(cfg *config.Config, rom romm.Rom, romPath string) []string {
	base := platform.StripLeadingMarker(filepath.Base(romPath))
	noExt := strings.TrimSuffix(base, filepath.Ext(base))
	var out []string
	for _, sf := range findLocalSavesForRom(cfg, rom) {
		stem := platform.StripLeadingMarker(strings.TrimSuffix(filepath.Base(sf.path), filepath.Ext(sf.path)))
		if stem == base || (noExt != "" && stem == noExt) {
			out = append(out, sf.path)
		}
	}
	return out
}

// ListSavesLocalState computes --list-saves' LOCAL= trailer under the STRICT semantics
// (task #135): the state is judged against the NEWEST non-ghost revision AND only the
// save the launch will load (primaryPaths — see PrimaryLocalSaveFilesForRomPath).
// saves must be sorted newest-first with ghosts already split out.
//
//	none     — the launched name has no local save file (a twin's save doesn't count:
//	           the launch loads nothing, so pulling the newest is lose-proof)
//	current  — a primary save's MD5 == the NEWEST revision's content_hash
//	older    — a primary save matches an OLDER revision only (the restore prompt case)
//	unpushed — a primary save exists but matches no revision (unpushed local progress)
//
// A revision without a content_hash can never match (honest by omission — same rule as
// the row-level CURRENT mark).
func ListSavesLocalState(saves []romm.Save, primaryPaths []string) string {
	if len(primaryPaths) == 0 {
		return "none"
	}
	var hashes []string
	for _, p := range primaryPaths {
		if sum, ok := fileMD5(p); ok {
			hashes = append(hashes, sum)
		}
	}
	matches := func(s romm.Save) bool {
		if s.ContentHash == nil {
			return false
		}
		for _, h := range hashes {
			if strings.EqualFold(*s.ContentHash, h) {
				return true
			}
		}
		return false
	}
	for i, s := range saves {
		if matches(s) {
			if i == 0 {
				return "current"
			}
			return "older"
		}
	}
	return "unpushed"
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
		res.AuthExpired = romm.IsAuthError(err)
		return res
	}
	// Same verified upload gate as every other push route: 0-byte guard, conflict
	// retry, server-side verify + one retry (see uploadVerified in push.go).
	return uploadVerified(client, cfg, rom, filePath, emulator)
}

// resolveRomAndLocalSavePath reverses romPath to a rom_id (catalog index), fetches
// the full ROM, and computes the local save path using saveExt for the filename.
// ok is false when any step fails; fail then carries the HONEST failure result:
// an index miss is PullResolveFail ("not matched to a RomM game"), but a GetRom
// network/auth failure is PullError ("couldn't reach server", AuthExpired set) —
// pre-A2 both collapsed into ResolveFail, so an offline device reported a resolvable
// game as "not matched" (the Smart Pro 2026-07-02 field lie).
func resolveRomAndLocalSavePath(client *romm.Client, cfg *config.Config, romPath, saveExt string) (rom romm.Rom, localPath string, fail PullResult, ok bool) {
	romID, rok := catalog.ResolveRomID(cfg, romPath)
	if !rok || romID == 0 {
		return romm.Rom{}, "", PullResult{Outcome: PullResolveFail, Reason: "not matched to a RomM game"}, false
	}
	rom, err := client.GetRom(romID)
	if err != nil {
		return romm.Rom{}, "", PullResult{Outcome: PullError, Reason: "couldn't reach server", AuthExpired: romm.IsAuthError(err)}, false
	}
	if rom.ID == 0 {
		return romm.Rom{}, "", PullResult{Outcome: PullResolveFail, Reason: "not matched to a RomM game"}, false
	}
	localPath = saveLocalPath(cfg, rom, romPath, saveExt)
	if localPath == "" {
		return rom, "", PullResult{Outcome: PullResolveFail, Reason: "no save directory"}, false
	}
	return rom, localPath, PullResult{}, true
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
	if err != nil || len(data) == 0 {
		// len(data)==0 is the last-line ghost net: even if a byte-less record slipped
		// past the list filter, an empty body never overwrites a real local save.
		return PullResult{Outcome: PullError, Reason: "download failed", AuthExpired: romm.IsAuthError(err)}
	}
	saveDir := filepath.Dir(localPath)
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		return PullResult{Outcome: PullError, Reason: "save dir not writable"}
	}
	if _, statErr := os.Stat(localPath); statErr == nil {
		_ = os.Rename(localPath, localPath+".bak")
	}
	// FAT32-atomic: temp + fsync + rename + dir fsync. Critical here — the live
	// save was just moved to .bak, so a non-durable write that a power-yank zeroes
	// would leave the live file empty with the good copy hidden in .bak.
	if err := fsutil.WriteFileAtomic(localPath, data, 0o644); err != nil {
		return PullResult{Outcome: PullError, Reason: "write failed"}
	}
	// LEDGER CONFIRM (#176): the bytes are now safely on the card, so record that this
	// device holds save saveID (POST /api/saves/{id}/downloaded). We download with
	// optimistic=false precisely so the server does NOT advance the sync row until this
	// point. Best-effort and non-blocking — a failed confirm never turns a written save
	// into a pull error (the save is already durable); gated on RomM >= 4.9.0.
	confirmDownloaded(client, cfg, saveID)
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
