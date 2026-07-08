package platform

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/romm"
)

func manifestTestEnv(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "Tools", "tg5040", "Lodor.pak"))
	return base
}

// TestManifestRoundTrip: record/rename/forget survive a Save/Load cycle, paths
// stored SDCARD-relative, kind updates keep created_at.
func TestManifestRoundTrip(t *testing.T) {
	base := manifestTestEnv(t)
	stub := filepath.Join(base, "Roms", "GBA", "✘ Foo.gba")

	m := LoadManifest()
	if m.Owns(stub) {
		t.Fatal("fresh manifest owns a path")
	}
	m.Record(stub, ManifestStub, 42)
	if !m.OwnsKind(stub, ManifestStub) {
		t.Fatal("recorded stub not owned")
	}
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}

	m2 := LoadManifest()
	e, ok := m2.Entry(stub)
	if !ok || e.Kind != ManifestStub || e.RomID != 42 || e.CreatedAt == "" {
		t.Fatalf("round-trip entry = %+v ok=%v", e, ok)
	}
	// Stored key must be SDCARD-relative (card movable between mounts).
	if _, raw := m2.Entries[stub]; raw {
		t.Fatal("manifest stored an absolute path")
	}
	if _, rel := m2.Entries[filepath.Join(string(os.PathSeparator), "Roms", "GBA", "✘ Foo.gba")]; !rel {
		t.Fatalf("manifest keys = %v, want SDCARD-relative", keys(m2.Entries))
	}

	// Kind upgrade (stub filled by a download) keeps created_at.
	m2.Record(stub, ManifestDownload, 42)
	e2, _ := m2.Entry(stub)
	if e2.Kind != ManifestDownload || e2.CreatedAt != e.CreatedAt {
		t.Fatalf("kind upgrade = %+v, want download with original created_at %q", e2, e.CreatedAt)
	}

	// Rename moves ownership.
	dev := filepath.Join(base, "Roms", "GBA", "✓ Foo.gba")
	m2.RenamePath(stub, dev)
	if m2.Owns(stub) || !m2.OwnsKind(dev, ManifestDownload) {
		t.Fatal("rename did not move ownership")
	}
	m2.Forget(dev)
	if m2.Owns(dev) {
		t.Fatal("forget did not drop ownership")
	}
}

func keys(m map[string]ManifestEntry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestManifestCorruptFailSafe: an unparseable manifest owns NOTHING (every
// destructive path degrades to no-op / triple gate) while recording proceeds.
func TestManifestCorruptFailSafe(t *testing.T) {
	base := manifestTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(ManifestPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ManifestPath(), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := LoadManifest()
	if m == nil {
		t.Fatal("LoadManifest returned nil")
	}
	p := filepath.Join(base, "Roms", "GBA", "✘ Foo.gba")
	if m.Owns(p) {
		t.Fatal("corrupt manifest claims ownership")
	}
	// Recording + saving replaces the corrupt file with a valid one.
	m.Record(p, ManifestStub, 1)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	if !LoadManifest().Owns(p) {
		t.Fatal("re-recorded manifest did not persist")
	}
}

// TestManifestSaveIsAtomicAndClean: Save writes no .tmp litter and a clean
// (undirty) manifest writes nothing at all.
func TestManifestSaveIsAtomicAndClean(t *testing.T) {
	base := manifestTestEnv(t)
	m := LoadManifest()
	if err := m.Save(); err != nil { // not dirty — must be a no-op
		t.Fatal(err)
	}
	if _, err := os.Stat(ManifestPath()); !os.IsNotExist(err) {
		t.Fatal("undirty Save wrote a manifest")
	}
	m.Record(filepath.Join(base, "Roms", "GBA", "✘ A.gba"), ManifestStub, 0)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ManifestPath() + ".tmp"); !os.IsNotExist(err) {
		t.Fatal(".tmp litter left after Save")
	}
}

// TestReclaimableStubTripleGate: each leg of the triple gate individually denies —
// a user file can never be all three (0-byte AND ✘-marked AND catalog-resolving).
func TestReclaimableStubTripleGate(t *testing.T) {
	if HostShowsStateNatively() {
		t.Skip("marker-less host (hard-true build tag): the ✘ marker leg under test does not apply")
	}
	base := manifestTestEnv(t)
	t.Setenv("LODOR_HOST_OS", "nextui") // marker-baking host: the marker leg applies
	dir := filepath.Join(base, "Roms", "GBA")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	yes := func(string) bool { return true }
	no := func(string) bool { return false }

	stub := write("✘ Foo.gba", nil)
	if !ReclaimableStub(stub, yes) {
		t.Error("genuine ✘ 0-byte resolving stub not reclaimable")
	}
	if ReclaimableStub(stub, no) {
		t.Error("non-resolving stub reclaimed (catalog leg bypassed)")
	}
	if ReclaimableStub(stub, nil) {
		t.Error("nil resolver reclaimed a stub")
	}
	if real := write("✘ Decoy.gba", []byte("REAL BYTES")); ReclaimableStub(real, yes) {
		t.Error("REAL-bytes marker-lookalike reclaimed — user file at risk")
	}
	if unmarked := write("Broken Copy.gba", nil); ReclaimableStub(unmarked, yes) {
		t.Error("user's unmarked 0-byte file reclaimed — download-over-user hole")
	}
	if legacy := write("[^] Old.gba", nil); !ReclaimableStub(legacy, yes) {
		t.Error("legacy [^] stub not reclaimable")
	}
	if dev := write("✓ Got.gba", nil); ReclaimableStub(dev, yes) {
		t.Error("0-byte ✓ (on-device marker) reclaimed — only the CLOUD marker qualifies")
	}
}

// TestReconcileGuardedProtectsUserLegacyFile (V4): in merge mode an UNOWNED file
// at the bare canonical name is the user's — reconcile must not rename it (or its
// saves), must not stub beside it, and must report skip ("", false).
func TestReconcileGuardedProtectsUserLegacyFile(t *testing.T) {
	base := manifestTestEnv(t)
	t.Setenv("BASE_PATH", base)
	dir := filepath.Join(base, "Roms", "Game Boy Advance (GBA)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(dir, "Emerald.gba")
	if err := os.WriteFile(user, []byte("USER ROM"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveDir := filepath.Join(base, "Saves", "GBA")
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userSave := filepath.Join(saveDir, "Emerald.gba.sav")
	if err := os.WriteFile(userSave, []byte("USER SAVE"), 0o644); err != nil {
		t.Fatal(err)
	}

	rom := romm.Rom{ID: 1, PlatformFsSlug: "gba", FsName: "Emerald.gba"}
	final, created := ReconcileMarkedPresenceGuarded(nil, rom, user, func(string) bool { return false })
	if final != "" || created {
		t.Fatalf("guarded reconcile = (%q,%v), want skip (\"\",false)", final, created)
	}
	if data, err := os.ReadFile(user); err != nil || string(data) != "USER ROM" {
		t.Fatalf("user ROM touched (err=%v data=%q)", err, data)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerOnDevice+"Emerald.gba")); !os.IsNotExist(err) {
		t.Fatal("user ROM was ✓-renamed — the exact V4 field bug")
	}
	if data, err := os.ReadFile(userSave); err != nil || string(data) != "USER SAVE" {
		t.Fatalf("user save touched (err=%v data=%q)", err, data)
	}

	// The SAME shape with ownership (a pre-marker Lodor deployment's own file)
	// still migrates — the guard denies only unowned candidates.
	owned, _ := ReconcileMarkedPresenceGuarded(nil, rom, user, func(string) bool { return true })
	if fi, err := os.Stat(filepath.Join(dir, MarkerOnDevice+"Emerald.gba")); err != nil || fi.Size() == 0 {
		t.Fatalf("owned legacy file did not migrate to ✓ (final=%q err=%v)", owned, err)
	}
}
