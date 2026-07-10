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
//
// Seq is a per-ledger monotonic upload sequence stamped once when the entry
// first enters the ledger and PRESERVED across every MD5-refresh (#9). It is
// the stable retention ordering: RecordedAt is reset by a refresh (a re-record
// of the same bytes stamps "now"), so ordering by RecordedAt can make the
// NEWEST upload look oldest and pick it as the retention victim — a data-loss
// bug. Seq never moves once assigned, so "oldest upload" stays oldest.
type StateLedgerEntry struct {
	MD5        string    `json:"md5"`
	Size       int64     `json:"size"`
	ServerID   int       `json:"server_id,omitempty"`
	Slot       string    `json:"slot,omitempty"`
	Origin     string    `json:"origin,omitempty"` // producer tuple (D3)
	Own        bool      `json:"own,omitempty"`
	Seq        int64     `json:"seq,omitempty"` // monotonic upload order; stable across MD5-refresh (#9)
	RecordedAt time.Time `json:"recorded_at"`
}

type stateLedgerFile struct {
	Version int                           `json:"version"`
	NextSeq int64                         `json:"next_seq,omitempty"` // monotonic Seq allocator (#9)
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

// StateKnown reports whether these exact bytes are EVER recorded for the rom —
// the dedup-before-upload check (a re-upload of known bytes is idempotent, so
// "ever recorded" is the right, safe gate for push).
//
// NOTE: this is NOT the right gate for the placement occupant-preserve decision.
// StateKnown ignores ServerID, and retention (retireOwnOldStates →
// ForgetStateServerID) keeps the MD5 entry while zeroing its ServerID after a
// server-side delete — so bytes that have been DELETED off the server still read
// as "known" here. Placement must use StateOnServer instead (#23): renaming an
// occupant to .bak is only safe when its bytes are CURRENTLY on the server.
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

// StateOnServer reports whether these exact bytes are CURRENTLY on the server —
// i.e. some ledger entry for the rom carries this MD5 AND a live (non-zero)
// ServerID. This is the placement occupant-preserve gate (#23): unlike
// StateKnown it does not treat a retention-forgotten record (ServerID zeroed
// after a server-side delete) as safe, so the occupant is re-uploaded before
// the destructive rename rather than surviving only on disk.
func StateOnServer(romID int, md5 string) bool {
	if md5 == "" {
		return false
	}
	for _, e := range StateLedgerEntries(romID) {
		if e.ServerID != 0 && strings.EqualFold(e.MD5, md5) {
			return true
		}
	}
	return false
}

// RecordState appends (or refreshes, matched by the (Slot, ServerID, MD5) identity) one entry for the rom.
// RecordedAt is stamped now when zero. Seq (the stable retention ordering, #9)
// is allocated once on first insert from the ledger's monotonic NextSeq and
// PRESERVED on an MD5-refresh — a refresh must never make an old upload look new
// (or a new one look old) to retention, so it carries the original entry's Seq
// forward regardless of what the caller passed.
func RecordState(romID int, entry StateLedgerEntry) error {
	if entry.RecordedAt.IsZero() {
		entry.RecordedAt = time.Now()
	}
	path := stateLedgerPath()
	lf := readStateLedger(path)
	key := strconv.Itoa(romID)
	replaced := false
	for i, e := range lf.Roms[key] {
		// Identity is (Slot, ServerID, MD5) — NOT MD5 alone. Two DISTINCT server
		// records that happen to carry identical bytes in different slots (or under
		// different server ids) are distinct artifacts: collapsing them by MD5 alone
		// orphaned one server id from the ledger, so retention never deleted it (a
		// server-side leak). A true refresh is a re-record of the SAME logical entry
		// — same slot AND same server id AND same bytes — and still updates in place,
		// preserving Seq so retention ordering is stable (#9).
		if e.Slot == entry.Slot && e.ServerID == entry.ServerID && strings.EqualFold(e.MD5, entry.MD5) {
			entry.Seq = e.Seq // preserve original upload order across refresh (#9)
			lf.Roms[key][i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		lf.NextSeq++ // 1-based; 0 means "never sequenced" (legacy entries)
		entry.Seq = lf.NextSeq
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
