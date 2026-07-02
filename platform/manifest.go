// Mirror-owned manifest (C2 of the coexist/merge design, 2026-07-02 — the
// prune-safety core; BETA-BLOCKING per the C1 gate decision).
//
// THE ONE RULE: no engine code path may delete, truncate, or rename a path under
// Roms/ or Collections/ unless this manifest owns it. The manifest is the mirror's
// ledger of everything it CREATED on the card — stubs, downloads, covers, folders,
// collection files — so "ours vs the user's" is recorded fact, never inference.
// Every destructive path funnels through Owns() (the single choke point); a path
// the manifest doesn't own may be re-claimed only through the conservative
// ReclaimableStub triple gate below.
//
// FAIL-SAFE BY CONSTRUCTION: a missing or unparseable manifest disables ownership
// entirely (Owns() = false for every path) — destructive paths degrade to no-ops
// (or the triple gate), while CREATION proceeds and re-records, so a lost manifest
// converges back to complete over the next mirror pass. Real (non-stub) files are
// NEVER re-claimed automatically: losing the manifest demotes our own downloads to
// user-like (never auto-deleted) — the acceptable direction.
//
// Lives beside catalog-index.json in the host pak's working dir (engine-owned,
// never in the user's Roms tree). Paths are stored SDCARD-relative (leading "/",
// the same TrimPrefix(sdcardRoot) shape the catalog index by_id uses) so a card
// moved between mounts stays valid. Writes are temp + fsync + rename + dir-fsync:
// FAT32 card writes have zeroed config files in the field before
// (feedback_lodor_fat32_atomic_writes) and a torn manifest must never exist —
// worst case the rename never lands and pruning stays disabled, the safe way.
//
// CGO-free, stdlib only. Tag-free: built identically for MinUI/NextUI and OnionOS.
package platform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Manifest entry kinds. "stub" is a 0-byte placeholder the mirror wrote; "download"
// is a real ROM file the engine downloaded (or, in own/separate modes, a real file
// found at a mirror-owned name — those folders are the mirror's by construction);
// "cover" is box art the engine fetched into .media/; "folder" is a directory the
// mirror created (removable at uninstall only when empty); "collection"/"continue"
// are Collections/*.txt files the mirror wrote.
const (
	ManifestStub       = "stub"
	ManifestDownload   = "download"
	ManifestCover      = "cover"
	ManifestFolder     = "folder"
	ManifestCollection = "collection"
	ManifestContinue   = "continue"
)

// manifestFileName is the on-disk name, beside catalog-index.json in PakDir().
const manifestFileName = "mirror-manifest.json"

// ManifestEntry describes one mirror-created path.
type ManifestEntry struct {
	Kind      string `json:"kind"`
	RomID     int    `json:"rom_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// Manifest is the mirror's ownership ledger. Zero value is unusable; obtain one
// via LoadManifest (never nil).
type Manifest struct {
	Version int `json:"version"`
	// Mode records the mirror mode the directory mappings/stubs were generated
	// under (own|separate|merge) so a later run can detect a mode flip and run the
	// explicit migration instead of silently double-stubbing under new names.
	Mode            string                   `json:"mode,omitempty"`
	GeneratedModeAt string                   `json:"generated_mode_at,omitempty"`
	Entries         map[string]ManifestEntry `json:"entries"`

	dirty bool
}

// ManifestPath returns the manifest's absolute path (engine-owned pak dir).
func ManifestPath() string {
	return filepath.Join(PakDir(), manifestFileName)
}

// manifestSDRoot mirrors catalog's sdcardRoot: the SDCARD_PATH env, defaulting to
// /mnt/SDCARD — the root manifest paths are stored relative to.
func manifestSDRoot() string {
	if sd := os.Getenv("SDCARD_PATH"); sd != "" {
		return sd
	}
	return "/mnt/SDCARD"
}

// manifestRel converts an absolute card path to the stored SDCARD-relative form
// (leading separator kept, matching the catalog index by_id convention). A path
// already relative is returned unchanged.
func manifestRel(abs string) string {
	return strings.TrimPrefix(abs, manifestSDRoot())
}

// LoadManifest reads the manifest from ManifestPath(). NEVER returns nil:
//   - absent file  -> a fresh, empty manifest (owns nothing; recording rebuilds it)
//   - corrupt file -> a fresh, empty manifest + a loud stderr line (owns nothing —
//     every destructive path degrades to the triple gate / no-op this run — while
//     creation re-records, per the fail-safe contract above)
func LoadManifest() *Manifest {
	m := &Manifest{Version: 1, Entries: map[string]ManifestEntry{}}
	data, err := os.ReadFile(ManifestPath())
	if err != nil {
		return m // absent (or unreadable): fresh manifest, owns nothing
	}
	var parsed Manifest
	if uerr := json.Unmarshal(data, &parsed); uerr != nil || parsed.Entries == nil {
		fmt.Fprintf(os.Stderr, "MANIFEST corrupt — ownership unknowable; prune/evict/rename disabled this run (re-recording)\n")
		return m
	}
	if parsed.Version == 0 {
		parsed.Version = 1
	}
	return &parsed
}

// Entry returns the manifest entry for a path (absolute or SDCARD-relative).
func (m *Manifest) Entry(path string) (ManifestEntry, bool) {
	if m == nil || len(m.Entries) == 0 {
		return ManifestEntry{}, false
	}
	e, ok := m.Entries[manifestRel(path)]
	return e, ok
}

// Owns is THE choke point: whether the mirror created (and therefore may mutate)
// this path. Every delete/truncate/rename under Roms/ or Collections/ must gate on
// this (or on ReclaimableStub for the conservative unmanifested-stub case).
func (m *Manifest) Owns(path string) bool {
	_, ok := m.Entry(path)
	return ok
}

// OwnsKind reports ownership AND that the entry is of the given kind.
func (m *Manifest) OwnsKind(path, kind string) bool {
	e, ok := m.Entry(path)
	return ok && e.Kind == kind
}

// Record marks a path as mirror-created. Recording an already-owned path updates
// its kind/rom_id in place (a stub filled by a download is re-recorded as
// "download") while keeping the original created_at.
func (m *Manifest) Record(path, kind string, romID int) {
	if m == nil || path == "" {
		return
	}
	rel := manifestRel(path)
	e, ok := m.Entries[rel]
	if !ok {
		e = ManifestEntry{CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	if e.Kind == kind && e.RomID == romID && ok {
		return // no change — don't dirty the manifest for a byte-identical entry
	}
	e.Kind = kind
	if romID != 0 {
		e.RomID = romID
	}
	m.Entries[rel] = e
	m.dirty = true
}

// Forget drops a path from the manifest (after the mirror deleted it).
func (m *Manifest) Forget(path string) {
	if m == nil {
		return
	}
	rel := manifestRel(path)
	if _, ok := m.Entries[rel]; ok {
		delete(m.Entries, rel)
		m.dirty = true
	}
}

// RenamePath moves ownership from oldPath to newPath (a mirror-owned rename, e.g.
// the ✘/✓ marker flip or the separate→merge folder migration).
func (m *Manifest) RenamePath(oldPath, newPath string) {
	if m == nil {
		return
	}
	oldRel, newRel := manifestRel(oldPath), manifestRel(newPath)
	e, ok := m.Entries[oldRel]
	if !ok {
		return
	}
	delete(m.Entries, oldRel)
	m.Entries[newRel] = e
	m.dirty = true
}

// SetMode records the mirror mode the current mappings/stubs were generated under.
func (m *Manifest) SetMode(mode string) {
	if m == nil || m.Mode == mode {
		return
	}
	m.Mode = mode
	m.GeneratedModeAt = time.Now().UTC().Format(time.RFC3339)
	m.dirty = true
}

// Save atomically persists the manifest when anything changed: temp + fsync +
// rename + parent-dir fsync (FAT32-atomic; see the package comment). A no-change
// manifest writes nothing — one write per mirror run, one per download/evict.
func (m *Manifest) Save() error {
	if m == nil || !m.dirty {
		return nil
	}
	path := ManifestPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, werr := f.Write(data); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return werr
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return serr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return rerr
	}
	if d, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	m.dirty = false
	return nil
}

// ReclaimableStub is the conservative triple gate for re-claiming an UNMANIFESTED
// path as a mirror-owned stub (lost/legacy manifest): the path must be
//
//  1. a 0-byte regular file, AND
//  2. cloud-marker-prefixed ("✘ "/legacy "[^] ") — required only on hosts where
//     the engine bakes markers (NextUI/muOS); LodorOS names are canonical by
//     design (HostShowsStateNatively), so the marker leg is skipped there, AND
//  3. resolvable in the catalog index (the caller supplies the resolver — the
//     platform package cannot import catalog).
//
// A user's file is never all three: real bytes fail (1); a user's 0-byte decoy has
// no marker (2); a hand-copied REAL "✘ …" file fails (1). Real files are NEVER
// reclaimed — a lost manifest demotes our own downloads to user-like, the safe way.
func ReclaimableStub(path string, resolves func(string) bool) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.IsDir() || fi.Size() != 0 {
		return false
	}
	if !HostShowsStateNatively() && !HasCloudMarker(filepath.Base(path)) {
		return false
	}
	return resolves != nil && resolves(path)
}
