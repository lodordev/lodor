package catalog

// V1 (C1 coexist-design audit): pruneUnplayableStubs used to delete EVERY 0-byte
// file in a mapped folder and RemoveAll the folder when only .media remained — in
// merge mode that folder is the USER'S adopted folder. These tests lock the
// manifest-scoped behavior: only mirror-owned stubs (or triple-gate reclaimable
// ones) are deleted, the user's placeholders/box art survive, and folder teardown
// happens only for mirror-created folders.

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/platform"
	"lodor/romm"
)

// TestPruneUnplayableStubsManifestScoped: a user's 0-byte file and their box art
// survive the no-Emu-pak prune; the mirror's own stub goes; the (user) folder stays.
func TestPruneUnplayableStubsManifestScoped(t *testing.T) {
	cfg, rom, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	if err := os.MkdirAll(filepath.Join(dir, ".media"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The mirror's own stub (manifest-owned).
	ourStub := filepath.Join(dir, platform.MarkerCloud+filepath.Base(unmarked))
	if err := os.WriteFile(ourStub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	man := platform.LoadManifest()
	man.Record(ourStub, platform.ManifestStub, rom.ID)

	// The user's own 0-byte placeholder (unowned, unmarked) + their box art.
	userZero := filepath.Join(dir, "My Placeholder.gba")
	if err := os.WriteFile(userZero, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	userArt := filepath.Join(dir, ".media", "My Game.png")
	if err := os.WriteFile(userArt, []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolves := func(p string) bool { _, ok := ResolveRomID(cfg, p); return ok }
	pruneUnplayableStubs(cfg, romm.Platform{FsSlug: "gba"}, man, resolves)

	if _, err := os.Stat(ourStub); !os.IsNotExist(err) {
		t.Fatal("mirror-owned stub not pruned")
	}
	if _, err := os.Stat(userZero); err != nil {
		t.Fatalf("user 0-byte file deleted by prune (V1): %v", err)
	}
	if data, err := os.ReadFile(userArt); err != nil || string(data) != "PNG" {
		t.Fatalf("user box art lost (V1 RemoveAll case): err=%v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("user folder removed by prune (V1): %v", err)
	}
}

// TestPruneReclaimsLegacyStubs: a manifest-less (legacy) card still self-heals —
// a ✘-marked, 0-byte, catalog-resolving stub passes the triple gate and is pruned;
// a real-bytes ✘ decoy and an unmarked 0-byte file are refused.
func TestPruneReclaimsLegacyStubs(t *testing.T) {
	cfg, _, unmarked := markerTestCfg(t)
	t.Setenv("LODOR_HOST_OS", "nextui")
	dir := filepath.Dir(unmarked)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyStub := filepath.Join(dir, platform.MarkerCloud+filepath.Base(unmarked))
	if err := os.WriteFile(legacyStub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(dir, platform.MarkerCloud+"Decoy.gba")
	if err := os.WriteFile(decoy, []byte("REAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	userZero := filepath.Join(dir, "Broken Copy.gba")
	if err := os.WriteFile(userZero, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	man := platform.LoadManifest() // empty: legacy card
	resolves := func(p string) bool { _, ok := ResolveRomID(cfg, p); return ok }
	pruneUnplayableStubs(cfg, romm.Platform{FsSlug: "gba"}, man, resolves)

	if _, err := os.Stat(legacyStub); !os.IsNotExist(err) {
		t.Fatal("triple-gate reclaimable legacy stub not pruned")
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("real-bytes ✘ decoy deleted: %v", err)
	}
	if _, err := os.Stat(userZero); err != nil {
		t.Fatalf("unmarked user 0-byte file deleted: %v", err)
	}
}

// TestPruneRemovesOnlyMirrorCreatedFolder: folder teardown fires only when the
// manifest owns the FOLDER, deletes only our own covers, and uses plain os.Remove
// (structurally incapable of taking user files with it).
func TestPruneRemovesOnlyMirrorCreatedFolder(t *testing.T) {
	cfg, rom, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	media := filepath.Join(dir, ".media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	ourStub := filepath.Join(dir, platform.MarkerCloud+filepath.Base(unmarked))
	if err := os.WriteFile(ourStub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stem := filepath.Base(unmarked)
	stem = stem[:len(stem)-len(filepath.Ext(stem))]
	ourCover := filepath.Join(media, platform.MarkerCloud+stem+".png")
	if err := os.WriteFile(ourCover, []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}

	man := platform.LoadManifest()
	man.Record(dir, platform.ManifestFolder, 0)
	man.Record(ourStub, platform.ManifestStub, rom.ID)
	man.Record(ourCover, platform.ManifestCover, rom.ID)

	resolves := func(p string) bool { _, ok := ResolveRomID(cfg, p); return ok }
	pruneUnplayableStubs(cfg, romm.Platform{FsSlug: "gba"}, man, resolves)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("mirror-created folder (stub+own cover only) not removed")
	}
	if man.Owns(dir) || man.Owns(ourStub) || man.Owns(ourCover) {
		t.Fatal("manifest still owns removed paths")
	}
}
