package sync

// The state ledger: the engine's OWN record of every save-state artifact it has
// uploaded to or placed from the server (Handoff v1, design D2). It exists
// because 4.9.2 StateSchema carries no content_hash — identity lives client-side.
//
//	<LODOR_PAK_DIR>/state-ledger.json
//	{ "version": 1, "roms": { "9752": [ {md5,size,server_id,slot,origin,recorded_at} ] } }
//
// Same durability posture as the sync-anchor store after its review (M2/M3):
// FAT32-atomic whole-file rewrite via fsutil, deliberately NO lock — concurrent
// writers lose an update at worst, which degrades to a duplicate upload
// (idempotent by dedup) or a re-download, never a wrong transfer. A corrupt or
// absent ledger reads as empty: every local state looks unknown, which errs
// toward uploading (safe) and never toward overwriting (placement treats
// unknown local files as precious — invariant 7.1).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lodor/fsutil"
	"lodor/platform"
)

// StateLedgerEntry is one known state artifact. Own marks server records THIS
// engine created (push upload / occupant-rescue upload) — the only records
// retention may ever delete (invariant 7.9: deletion never propagates; a pulled
// record's ServerID belongs to another device's upload, so pull records leave
// Own false). A refresh-by-MD5 from a pull can clear a prior Own — that errs
// toward retaining, never toward deleting.
type StateLedgerEntry struct {
	MD5        string    `json:"md5"`
	Size       int64     `json:"size"`
	ServerID   int       `json:"server_id,omitempty"`
	Slot       string    `json:"slot,omitempty"`
	Origin     string    `json:"origin,omitempty"` // producer tuple (D3)
	Own        bool      `json:"own,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

type stateLedgerFile struct {
	Version int                          `json:"version"`
	Roms    map[string][]StateLedgerEntry `json:"roms"`
}

const stateLedgerVersion = 1

func stateLedgerPath() string {
	return filepath.Join(platform.PakDir(), "state-ledger.json")
}

func readStateLedger(path string) stateLedgerFile {
	lf := stateLedgerFile{Version: stateLedgerVersion, Roms: map[string][]StateLedgerEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return lf
	}
	_ = json.Unmarshal(data, &lf)
	if lf.Roms == nil {
		lf.Roms = map[string][]StateLedgerEntry{}
	}
	return lf
}

func writeStateLedger(path string, lf stateLedgerFile) error {
	lf.Version = stateLedgerVersion
	if lf.Roms == nil {
		lf.Roms = map[string][]StateLedgerEntry{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o644)
}

// StateLedgerEntries returns the recorded entries for one rom (nil when none).
func StateLedgerEntries(romID int) []StateLedgerEntry {
	lf := readStateLedger(stateLedgerPath())
	return lf.Roms[strconv.Itoa(romID)]
}

// StateKnown reports whether these exact bytes are already recorded for the rom —
// the dedup-before-upload check and the placement "is the local occupant safe to
// replace" check (a known hash is, by definition, already on the server).
func StateKnown(romID int, md5 string) bool {
	if md5 == "" {
		return false
	}
	for _, e := range StateLedgerEntries(romID) {
		if strings.EqualFold(e.MD5, md5) {
			return true
		}
	}
	return false
}

// RecordState appends (or refreshes, matched by MD5) one entry for the rom.
// RecordedAt is stamped now when zero.
func RecordState(romID int, entry StateLedgerEntry) error {
	if entry.RecordedAt.IsZero() {
		entry.RecordedAt = time.Now()
	}
	path := stateLedgerPath()
	lf := readStateLedger(path)
	key := strconv.Itoa(romID)
	replaced := false
	for i, e := range lf.Roms[key] {
		if strings.EqualFold(e.MD5, entry.MD5) {
			lf.Roms[key][i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		lf.Roms[key] = append(lf.Roms[key], entry)
	}
	return writeStateLedger(path, lf)
}

// ForgetStateServerID clears a server id from any entry that carries it (used
// after the engine deletes its OWN old upload for retention — the bytes may
// still exist locally, so the entry itself stays, id-less).
func ForgetStateServerID(romID, serverID int) error {
	path := stateLedgerPath()
	lf := readStateLedger(path)
	key := strconv.Itoa(romID)
	changed := false
	for i, e := range lf.Roms[key] {
		if e.ServerID == serverID && serverID != 0 {
			lf.Roms[key][i].ServerID = 0
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writeStateLedger(path, lf)
}
