package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"lodor/platform"
)

// The sync-anchor store: the persisted per-rom common-ancestor the 3-way reconciler
// compares against (research #1). It lives beside the offline upload queue under the
// host pak dir so it travels with the rest of the engine's pak-local state:
//
//	<LODOR_PAK_DIR>/sync-anchors.json
//
// Schema (version 1):
//
//	{ "version": 1, "anchors": {
//	    "1234": { "hash": "<md5>", "server_save_id": 9,
//	              "server_updated_at": "2026-...", "synced_at": "2026-..." } } }
//
// The map is keyed by rom_id as a string. hash is the MD5 of the save bytes both sides
// agreed on at the last successful sync. The whole file is rewritten atomically
// (tmp+rename) under a mkdir lock so a concurrent engine invocation can't corrupt it.

// Anchor is the per-rom sync anchor — the content state local and server agreed on at
// the last successful sync (the 3-way common ancestor).
type Anchor struct {
	Hash            string    `json:"hash"`
	ServerSaveID    int       `json:"server_save_id,omitempty"`
	ServerUpdatedAt time.Time `json:"server_updated_at,omitempty"`
	SyncedAt        time.Time `json:"synced_at"`
}

// anchorFile is the on-disk envelope.
type anchorFile struct {
	Version int               `json:"version"`
	Anchors map[string]Anchor `json:"anchors"`
}

const anchorStoreVersion = 1

// anchorPath returns the absolute path of the anchor store under the pak dir.
// MULTI-USER: the filename is profile-namespaced (sync-anchors.<profile>.json) so
// each user's integrity-critical reconcile state is isolated; single-user cards keep
// the historical un-namespaced sync-anchors.json (ProfileStateName returns it when no
// profile is active).
func anchorPath() string {
	return filepath.Join(platform.PakDir(), platform.ProfileStateName("sync-anchors", "json"))
}

// anchorLockPath returns the advisory mkdir-lock used to serialize store mutations.
func anchorLockPath() string {
	return filepath.Join(platform.PakDir(), ".anchors.lock")
}

// readAnchorFile loads the store, returning an empty (but initialized) envelope when
// the file is missing or unparseable — a corrupt/absent anchor store must degrade to
// "no anchors" (every rom reconciles as first-sync), never error out the sync.
func readAnchorFile(path string) anchorFile {
	af := anchorFile{Version: anchorStoreVersion, Anchors: map[string]Anchor{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return af
	}
	_ = json.Unmarshal(data, &af)
	if af.Anchors == nil {
		af.Anchors = map[string]Anchor{}
	}
	return af
}

// writeAnchorFile atomically rewrites the store (tmp file + rename).
func writeAnchorFile(path string, af anchorFile) error {
	af.Version = anchorStoreVersion
	if af.Anchors == nil {
		af.Anchors = map[string]Anchor{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lockAnchors takes the anchor-store lock by creating a lock DIRECTORY (mkdir is
// atomic). Best-effort, mirroring the pending-queue lock: a few quick retries then
// proceed unlocked rather than wedge the sync. Returns a release func.
func lockAnchors() (release func()) {
	noop := func() {}
	for i := 0; i < 50; i++ {
		if err := os.Mkdir(anchorLockPath(), 0o755); err == nil {
			return func() { _ = os.Remove(anchorLockPath()) }
		}
		if i >= 5 {
			break
		}
	}
	return noop
}

// LoadAnchor returns the stored anchor for romID, ok=false when none is recorded.
func LoadAnchor(romID int) (Anchor, bool) {
	af := readAnchorFile(anchorPath())
	a, ok := af.Anchors[strconv.Itoa(romID)]
	return a, ok
}

// SaveAnchor records (or replaces) the anchor for romID under the store lock. SyncedAt
// is stamped to now when the caller left it zero.
func SaveAnchor(romID int, a Anchor) error {
	if a.SyncedAt.IsZero() {
		a.SyncedAt = time.Now()
	}
	release := lockAnchors()
	defer release()
	path := anchorPath()
	af := readAnchorFile(path)
	af.Anchors[strconv.Itoa(romID)] = a
	return writeAnchorFile(path, af)
}

// DeleteAnchor removes the anchor for romID under the store lock. A missing entry is a
// no-op (nil error).
func DeleteAnchor(romID int) error {
	release := lockAnchors()
	defer release()
	path := anchorPath()
	af := readAnchorFile(path)
	if _, ok := af.Anchors[strconv.Itoa(romID)]; !ok {
		return nil
	}
	delete(af.Anchors, strconv.Itoa(romID))
	return writeAnchorFile(path, af)
}
