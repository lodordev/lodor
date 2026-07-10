//go:build !onion

package catalog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// mockColClient implements romClient for MirrorCollections tests: it serves fixed
// manual/virtual/smart collections and (optionally) makes the auto endpoints fail to
// exercise the graceful-no-op path.
type mockColClient struct {
	manual, virtual, smart []romm.Collection
	virtualErr, smartErr   error
}

func (m mockColClient) GetRoms(romm.GetRomsQuery) (romm.PaginatedRoms, error) {
	return romm.PaginatedRoms{}, nil
}
func (m mockColClient) GetCollections() ([]romm.Collection, error) { return m.manual, nil }
func (m mockColClient) GetVirtualCollections(string) ([]romm.Collection, error) {
	return m.virtual, m.virtualErr
}
func (m mockColClient) GetSmartCollections() ([]romm.Collection, error) {
	return m.smart, m.smartErr
}
func (m mockColClient) DownloadCover(string) ([]byte, error) { return nil, nil }
func (m mockColClient) DownloadCoverCtx(context.Context, string) ([]byte, error) {
	return nil, nil
}

func writeAlignTestIndex(t *testing.T, cfg *config.Config, byID map[int]string) {
	t.Helper()
	idx := index{Version: 1, Platforms: map[string]platformIndex{
		"nes": {ByID: byID},
	}}
	b, _ := json.Marshal(idx)
	if err := os.WriteFile(IndexPath(cfg), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func listCollectionFiles(t *testing.T, base string) []string {
	t.Helper()
	dir := filepath.Join(base, "Collections")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestMirrorCollectionsAllSources verifies manual + smart + virtual shelves are all
// written, that empty shelves (no on-card members) are skipped, and that a virtual/smart
// name never clobbers a higher-precedence (manual) shelf of the same name.
func TestMirrorCollectionsAllSources(t *testing.T) {
	sd := t.TempDir()
	pak := t.TempDir()
	t.Setenv("SDCARD_PATH", sd)
	t.Setenv("BASE_PATH", sd)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_PAK_DIR", pak)

	cfg := &config.Config{}
	writeAlignTestIndex(t, cfg, map[int]string{
		1: "/Roms/NES/Game1.nes",
		2: "/Roms/NES/Game2.nes",
		3: "/Roms/NES/Game3.nes",
	})

	client := mockColClient{
		manual: []romm.Collection{
			{Name: "Favorites", RomIDs: []int{1, 2}}, // manual wins the name
			{Name: "EmptyManual", RomIDs: []int{999}}, // no on-card member -> empty
		},
		smart: []romm.Collection{
			{Name: "Most Played", RomIDs: []int{3}},
		},
		virtual: []romm.Collection{
			{Name: "Favorites", RomIDs: []int{3}}, // collides with manual -> skipped
			{Name: "Action", RomIDs: []int{1, 3}},
		},
	}

	written, empty, total, _, manual, virtual, smart, err := MirrorCollections(client, cfg, catalog_reporter())
	if err != nil {
		t.Fatalf("MirrorCollections: %v", err)
	}
	// manual: Favorites written, EmptyManual empty -> manual=1
	// smart: Most Played -> smart=1
	// virtual: Favorites skipped (collision), Action written -> virtual=1
	if manual != 1 || smart != 1 || virtual != 1 {
		t.Errorf("breakdown manual=%d smart=%d virtual=%d, want 1/1/1", manual, smart, virtual)
	}
	if written != 3 {
		t.Errorf("written=%d, want 3", written)
	}
	if empty != 1 {
		t.Errorf("empty=%d, want 1 (EmptyManual)", empty)
	}
	if total != 5 {
		t.Errorf("total=%d, want 5 (2 manual + 1 smart + 2 virtual)", total)
	}
	files := listCollectionFiles(t, sd)
	want := []string{"Action.txt", "Favorites.txt", "Most Played.txt"}
	if len(files) != len(want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Errorf("files = %v, want %v", files, want)
			break
		}
	}
	// Favorites.txt must be the MANUAL one (members 1 & 2), proving precedence.
	body, _ := os.ReadFile(filepath.Join(sd, "Collections", "Favorites.txt"))
	if string(body) != "/Roms/NES/Game1.nes\n/Roms/NES/Game2.nes\n" {
		t.Errorf("Favorites.txt = %q, want the manual members", string(body))
	}
}

// TestMirrorCollectionsAutoEndpointsAbsent verifies an older RomM whose virtual/smart
// endpoints error (e.g. 404) still mirrors the manual collections — the auto shelves
// degrade to nothing, never failing the whole operation.
func TestMirrorCollectionsAutoEndpointsAbsent(t *testing.T) {
	sd := t.TempDir()
	pak := t.TempDir()
	t.Setenv("SDCARD_PATH", sd)
	t.Setenv("BASE_PATH", sd)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_PAK_DIR", pak)

	cfg := &config.Config{}
	writeAlignTestIndex(t, cfg, map[int]string{1: "/Roms/NES/Game1.nes"})

	client := mockColClient{
		manual:     []romm.Collection{{Name: "Manual", RomIDs: []int{1}}},
		virtualErr: &romm.StatusError{Code: 404},
		smartErr:   &romm.StatusError{Code: 404},
	}
	written, _, _, _, manual, virtual, smart, err := MirrorCollections(client, cfg, catalog_reporter())
	if err != nil {
		t.Fatalf("MirrorCollections degraded path: %v", err)
	}
	if written != 1 || manual != 1 || virtual != 0 || smart != 0 {
		t.Errorf("written=%d manual=%d virtual=%d smart=%d, want 1/1/0/0", written, manual, virtual, smart)
	}
}

// TestAutoShelfPrunedWhenGoneServerSide is the prune-vs-auto-shelf interaction (the
// re-land adaptation onto main's manifest-scoped prune, 60bc6f6): an auto (virtual)
// shelf written on one pass is manifest-owned like any mirror file, so when the server
// stops serving it, the NEXT pass prunes it — while the user's own files and the
// still-served manual shelf survive untouched.
func TestAutoShelfPrunedWhenGoneServerSide(t *testing.T) {
	sd := t.TempDir()
	pak := t.TempDir()
	t.Setenv("SDCARD_PATH", sd)
	t.Setenv("BASE_PATH", sd)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_PAK_DIR", pak)

	cfg := &config.Config{}
	writeAlignTestIndex(t, cfg, map[int]string{1: "/Roms/NES/Game1.nes"})

	colDir := filepath.Join(sd, "Collections")
	if err := os.MkdirAll(colDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(colDir, "Mine.txt")
	if err := os.WriteFile(userFile, []byte("/Roms/NES/User.nes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// PASS 1: server serves a manual shelf + a virtual auto shelf.
	c1 := mockColClient{
		manual:  []romm.Collection{{Name: "Manual", RomIDs: []int{1}}},
		virtual: []romm.Collection{{Name: "Auto Shelf", RomIDs: []int{1}}},
	}
	if _, _, _, _, _, _, _, err := MirrorCollections(c1, cfg, catalog_reporter()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	autoFile := filepath.Join(colDir, "Auto Shelf.txt")
	if _, err := os.Stat(autoFile); err != nil {
		t.Fatalf("auto shelf not written on pass 1: %v", err)
	}
	if man := platform.LoadManifest(); !man.OwnsKind(autoFile, platform.ManifestCollection) {
		t.Fatalf("auto shelf not manifest-owned after pass 1")
	}

	// PASS 2: the virtual shelf disappeared server-side (endpoint still up, empty).
	c2 := mockColClient{
		manual: []romm.Collection{{Name: "Manual", RomIDs: []int{1}}},
	}
	if _, _, _, _, _, _, _, err := MirrorCollections(c2, cfg, catalog_reporter()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if _, err := os.Stat(autoFile); !os.IsNotExist(err) {
		t.Fatalf("vanished auto shelf not pruned on pass 2 (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(colDir, "Manual.txt")); err != nil {
		t.Fatalf("manual shelf lost on pass 2: %v", err)
	}
	if _, err := os.Stat(userFile); err != nil {
		t.Fatalf("user file deleted by auto-shelf prune: %v", err)
	}
	if man := platform.LoadManifest(); man.Owns(autoFile) {
		t.Fatalf("manifest still claims the pruned auto shelf")
	}
}

// catalog_reporter returns a no-op Reporter for tests.
func catalog_reporter() *Reporter {
	return &Reporter{Phase: func(string) {}, Percent: func(int) {}}
}
