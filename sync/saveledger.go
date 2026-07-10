package sync

// The save ledger: the engine's OWN per-device record of the last battery-save
// revision that was verifiably SYNCED — pushed to or pulled from the server —
// for each ROM. It exists to make a LOCAL DELETE distinguishable from "never
// had a save": before it, the pull path treated an absent local file as first
// play and re-pulled the server's newest, so a save the user deliberately
// deleted on-device RESURRECTED on the next sync (the Argosy-2.0-tombstone
// gap). With the ledger, "file absent + server newest is the revision we last
// synced (or older)" reads as a deletion and the pull is skipped; a strictly
// NEWER server revision still pulls (another device advanced the game —
// resurrection is then the feature, not the bug).
//
//	<LODOR_PAK_DIR>/save-ledger.txt
//	<romID>\t<deviceID>\t<md5>\t<serverSaveID>\t<updatedAtRFC3339>
//
// One line per (rom, device); line-oriented like pending-saves.txt so it is
// inspectable/fixable on the card. Lines starting with '#' and lines that don't
// parse are PRESERVED verbatim on rewrite and ignored on read. Same durability
// posture as the state ledger (stateledger.go): FAT32-atomic whole-file rewrite
// via fsutil, deliberately NO lock — a lost concurrent update degrades to a
// stale/absent row, which fails OPEN toward today's pull-always behavior. The
// ledger only ever SKIPS a pull; it can never cause an overwrite (the local-
// file-exists paths never consult it) and never propagates a deletion to the
// server (nothing here deletes anything, anywhere).

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lodor/config"
	"lodor/fsutil"
	"lodor/platform"
	"lodor/romm"
)

// SaveLedgerEntry is one (rom, device) row: the last save revision this device
// verifiably held in synced form. MD5 is the revision's content hash (RomM's
// content_hash / the local file's MD5 — the same signal the lineage decision
// trusts); SaveID and UpdatedAt identify/order the server revision. A zero
// UpdatedAt (unparseable, or an old server that omitted it) disables the
// order-based tombstone rule for the row — fail-open, the hash rule still works.
type SaveLedgerEntry struct {
	RomID     int
	DeviceID  string
	MD5       string
	SaveID    int
	UpdatedAt time.Time
}

func saveLedgerPath() string {
	return filepath.Join(platform.PakDir(), "save-ledger.txt")
}

const saveLedgerHeader = "# lodor save-ledger v1: romID<TAB>deviceID<TAB>md5<TAB>saveID<TAB>updatedAt — last synced save revision per (rom, device); do not edit while syncing"

// parseSaveLedgerLine parses one ledger row. ok is false for comments, blanks,
// and anything malformed enough to have no usable identity (bad romID) — such
// lines are ignored on read and preserved on write. Field-level damage inside a
// parseable row degrades per-field (zero SaveID / zero UpdatedAt), never to a
// wrong tombstone: every degraded field only ever REMOVES a skip condition.
func parseSaveLedgerLine(line string) (SaveLedgerEntry, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return SaveLedgerEntry{}, false
	}
	parts := strings.Split(line, "\t")
	if len(parts) != 5 {
		return SaveLedgerEntry{}, false
	}
	romID, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || romID == 0 {
		return SaveLedgerEntry{}, false
	}
	e := SaveLedgerEntry{RomID: romID, DeviceID: parts[1], MD5: strings.TrimSpace(parts[2])}
	if id, err := strconv.Atoi(strings.TrimSpace(parts[3])); err == nil {
		e.SaveID = id
	}
	if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(parts[4])); err == nil {
		e.UpdatedAt = ts
	}
	return e, true
}

func formatSaveLedgerLine(e SaveLedgerEntry) string {
	ts := ""
	if !e.UpdatedAt.IsZero() {
		ts = e.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return strconv.Itoa(e.RomID) + "\t" + e.DeviceID + "\t" + e.MD5 + "\t" + strconv.Itoa(e.SaveID) + "\t" + ts
}

// LookupSyncedSave returns THIS device's ledger row for one rom. ok is false
// when no row exists (fresh card, reinstall, pre-tombstone history, corrupt
// file) — every one of which must read as "no record" so the pull behaves
// exactly as it did before the ledger existed.
func LookupSyncedSave(cfg *config.Config, romID int) (SaveLedgerEntry, bool) {
	dev := deviceID(cfg)
	data, err := os.ReadFile(saveLedgerPath())
	if err != nil {
		return SaveLedgerEntry{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if e, ok := parseSaveLedgerLine(strings.TrimRight(line, "\r")); ok &&
			e.RomID == romID && e.DeviceID == dev {
			return e, true
		}
	}
	return SaveLedgerEntry{}, false
}

// RecordSaveSynced upserts this device's row for one rom: the moment a save is
// verifiably identical here and on the server (verified push, dedup'd push,
// pull write, in-sync confirmation, explicit restore) is a "synced moment", and
// the revision involved becomes the tombstone baseline a later local delete is
// judged against. Unknown/malformed/other-device lines are preserved verbatim.
func RecordSaveSynced(cfg *config.Config, romID int, md5sum string, saveID int, updatedAt time.Time) error {
	entry := SaveLedgerEntry{RomID: romID, DeviceID: deviceID(cfg), MD5: md5sum, SaveID: saveID, UpdatedAt: updatedAt}
	path := saveLedgerPath()
	var out []string
	replaced := false
	if data, err := os.ReadFile(path); err == nil {
		for _, raw := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			line := strings.TrimRight(raw, "\r")
			if line == "" {
				continue
			}
			if e, ok := parseSaveLedgerLine(line); ok && e.RomID == entry.RomID && e.DeviceID == entry.DeviceID {
				out = append(out, formatSaveLedgerLine(entry))
				replaced = true
				continue
			}
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		out = append(out, saveLedgerHeader)
	}
	if !replaced {
		out = append(out, formatSaveLedgerLine(entry))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// FAT32-atomic: temp + fsync + rename + dir fsync (fsutil) — a power-yank
	// mid-write can zero a plainly-written file, and a zeroed ledger (while
	// fail-open) would forget every tombstone on the card.
	return fsutil.WriteFileAtomicString(path, strings.Join(out, "\n")+"\n", 0o644)
}

// recordSyncedSave is the best-effort call-site wrapper: derive the revision's
// MD5 (server content_hash first; the just-written/just-read local file as the
// fallback for a hash-less server) and upsert the row. A ledger write failure
// NEVER changes a sync outcome — the ledger only strengthens the NEXT decision,
// and a stale/missing row degrades to today's pull-always behavior.
func recordSyncedSave(cfg *config.Config, romID int, s romm.Save, localPath string) {
	if romID == 0 {
		return // no usable identity — parseSaveLedgerLine would drop the row anyway
	}
	sum := ""
	if s.ContentHash != nil {
		sum = *s.ContentHash
	}
	if sum == "" && localPath != "" {
		if l, ok := fileMD5(localPath); ok {
			sum = l
		}
	}
	_ = RecordSaveSynced(cfg, romID, sum, s.ID, s.UpdatedAt)
}

// SaveTombstoned decides the pull-side tombstone rule for a rom whose LOCAL
// SAVE FILE IS ABSENT: given the server's NEWEST real (non-ghost) revision,
// should the pull be skipped because the absence means "deleted here on
// purpose"?
//
//	no ledger row for (rom, this device)      → false (pull as today: fresh
//	                                            card / reinstall / pre-ledger)
//	newest.content_hash == ledgered MD5       → true  (the revision we last
//	                                            synced — its local copy was
//	                                            deleted; re-pulling would
//	                                            resurrect the deletion)
//	newest.updated_at <= ledgered updated_at  → true  (server has nothing newer
//	                                            than what this device already
//	                                            held and deleted)
//	otherwise                                 → false (a strictly newer revision
//	                                            exists — another device advanced
//	                                            the game; pull it)
//
// Zero/absent fields on either side disable their rule (fail-open toward
// pulling). Callers must only consult this when the local file is ABSENT — the
// existing content-hash lineage logic owns every local-file-exists decision —
// and never on the explicit-restore path (user intent resurrects, by design).
func SaveTombstoned(cfg *config.Config, romID int, newest romm.Save) bool {
	e, ok := LookupSyncedSave(cfg, romID)
	if !ok {
		return false
	}
	if newest.ContentHash != nil && *newest.ContentHash != "" && e.MD5 != "" &&
		strings.EqualFold(*newest.ContentHash, e.MD5) {
		return true
	}
	if !e.UpdatedAt.IsZero() && !newest.UpdatedAt.IsZero() && !newest.UpdatedAt.After(e.UpdatedAt) {
		return true
	}
	return false
}
