package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/platform"
	"lodor/romm"
)

// prunePassEnv runs one MirrorCollections pass against a card whose Collections/
// dir was pre-shaped by the caller. Serves ONE server collection ("Server List",
// rom 1) so the pass writes exactly one collection file.
func prunePassEnv(t *testing.T) (colDir string, run func()) {
	t.Helper()
	cfg, _, _ := continueTestEnv(t, 1)
	colDir = filepath.Join(os.Getenv("SDCARD_PATH"), "Collections")
	if err := os.MkdirAll(colDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &continueFake{
		platforms: []romm.Platform{{ID: 10, FsSlug: "gba", Name: "Game Boy Advance"}},
		cols:      []romm.Collection{{Name: "Server List", RomIDs: []int{1}}},
	}
	run = func() {
		t.Helper()
		if _, _, _, _, _, _, _, err := MirrorCollections(fake, cfg, nil); err != nil {
			t.Fatalf("MirrorCollections: %v", err)
		}
	}
	return colDir, run
}

// seedLegacyLedger writes the STEP 0b collections-owned.txt (the pre-manifest
// ownership file) as an upgraded field card would still carry it.
func seedLegacyLedger(t *testing.T, names ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(collectionsLedgerPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, n := range names {
		body += n + "\n"
	}
	if err := os.WriteFile(collectionsLedgerPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRefreshNeverDeletesUserCollections is the STEP 0b contract (C1 audit V2),
// now manifest-backed: a refresh must not touch the user's own Collections/*.txt
// or NextUI's native Collections/map.txt — while a stale LODOR-owned collection
// (imported from the legacy ledger) is still pruned, and ownership lands in the
// manifest with the ledger file retired (ONE mechanism).
func TestRefreshNeverDeletesUserCollections(t *testing.T) {
	colDir, runPass := prunePassEnv(t)

	userList := filepath.Join(colDir, "My Favorites.txt")
	if err := os.WriteFile(userList, []byte("/Roms/Game Boy Advance (GBA)/Mine.gba\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mapTxt := filepath.Join(colDir, "map.txt")
	mapBody := "My Favorites\tThe Good Stuff\n"
	if err := os.WriteFile(mapTxt, []byte(mapBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stale LODOR-owned collection from a previous pass (in the LEGACY ledger —
	// the upgrade-import path): the one thing the prune is still FOR.
	stale := filepath.Join(colDir, "Old Auto.txt")
	if err := os.WriteFile(stale, []byte("/Roms/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedLegacyLedger(t, "Old Auto.txt")

	runPass()

	if _, err := os.Stat(userList); err != nil {
		t.Fatalf("user collection deleted by refresh: %v", err)
	}
	got, err := os.ReadFile(mapTxt)
	if err != nil {
		t.Fatalf("map.txt deleted by refresh: %v", err)
	}
	if string(got) != mapBody {
		t.Fatalf("map.txt mutated: %q", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("ledger-owned stale collection not pruned")
	}
	if _, err := os.Stat(filepath.Join(colDir, "Server List.txt")); err != nil {
		t.Fatalf("server collection not written: %v", err)
	}
	// Manifest now records exactly this pass's writes — the next prune's whole
	// scope — and the legacy ledger is retired.
	man := platform.LoadManifest()
	if !man.OwnsKind(filepath.Join(colDir, "Server List.txt"), platform.ManifestCollection) {
		t.Fatalf("manifest does not own the written collection")
	}
	if man.Owns(stale) || man.Owns(userList) {
		t.Fatalf("manifest claims a pruned/user collection")
	}
	if _, err := os.Stat(collectionsLedgerPath()); !os.IsNotExist(err) {
		t.Fatalf("legacy ledger not retired after manifest save")
	}
}

// TestFirstRefreshWithoutOwnershipPrunesNothing: ownership unknowable (no
// manifest, no legacy ledger — the first run after upgrading onto an existing
// card) means NO deletions at all, even of files that look like stale mirror
// output.
func TestFirstRefreshWithoutOwnershipPrunesNothing(t *testing.T) {
	colDir, runPass := prunePassEnv(t)

	ghost := filepath.Join(colDir, "Ghost.txt")
	if err := os.WriteFile(ghost, []byte("/Roms/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(colDir, "Favorites.txt")
	if err := os.WriteFile(user, []byte("/Roms/y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runPass()

	for _, f := range []string{ghost, user} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s deleted on an ownership-less refresh: %v", filepath.Base(f), err)
		}
	}
	if man := platform.LoadManifest(); !man.OwnsKind(filepath.Join(colDir, "Server List.txt"), platform.ManifestCollection) {
		t.Fatalf("manifest not seeded after the pass")
	}
	// And the SECOND pass (manifest now knows its own) still leaves both alone.
	runPass()
	for _, f := range []string{ghost, user} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s deleted on the second refresh: %v", filepath.Base(f), err)
		}
	}
}

// TestMapTxtSurvivesEvenWhenOwnershipClaimsIt: defense in depth — map.txt is
// NextUI's file, full stop; a corrupt/lying ownership record must not change that.
func TestMapTxtSurvivesEvenWhenOwnershipClaimsIt(t *testing.T) {
	colDir, runPass := prunePassEnv(t)

	mapTxt := filepath.Join(colDir, "map.txt")
	if err := os.WriteFile(mapTxt, []byte("A\tB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A lying LEGACY ledger claims map.txt; the import happily records it — the
	// hard exclude in the prune is what protects the file regardless.
	seedLegacyLedger(t, "map.txt")

	runPass()

	if _, err := os.Stat(mapTxt); err != nil {
		t.Fatalf("map.txt deleted despite the hard exclude: %v", err)
	}
}
