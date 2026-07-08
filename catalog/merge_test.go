//go:build !onion && !muos && !knulli && !android && !lodorandroid

package catalog

// Merge-mode (C2) behavior tests: adopt-folder-by-tag mapping generation and the
// mirror's Tier-1 dedup-by-index-adoption. The synthetic-card FIXTURE test
// (merge_fixture_test.go) proves byte-identity end to end; these lock the
// individual mechanisms with focused assertions.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// mergeFake serves platforms + roms for a full MirrorCatalog pass.
type mergeFake struct {
	platforms []romm.Platform
	roms      map[int][]romm.Rom // platform id -> roms
}

func (f *mergeFake) GetRoms(q romm.GetRomsQuery) (romm.PaginatedRoms, error) {
	var items []romm.Rom
	for _, id := range q.PlatformIDs {
		items = append(items, f.roms[id]...)
	}
	return romm.PaginatedRoms{Items: items}, nil
}
func (f *mergeFake) GetCollections() ([]romm.Collection, error) { return nil, nil }
func (f *mergeFake) DownloadCover(p string) ([]byte, error)     { return nil, os.ErrNotExist }
func (f *mergeFake) GetPlatforms() ([]romm.Platform, error)     { return f.platforms, nil }

func mergeTestEnv(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_HOST_OS", "nextui")
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "Tools", "tg5040", "Lodor.pak"))
	// config.json / settings.conf are CWD-relative — keep them in the sandbox.
	t.Chdir(filepath.Join(base, mkdirAll(t, base, "Tools/tg5040/Lodor.pak")))
	// GBA + SFC emu paks installed (the HasEmuPak gate).
	for _, tag := range []string{"GBA", "SFC"} {
		mkdirAll(t, base, ".system/tg5040/paks/Emus/"+tag+".pak")
	}
	return base
}

func mkdirAll(t *testing.T, base, rel string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(base, rel), 0o755); err != nil {
		t.Fatal(err)
	}
	return rel
}

func writeCard(t *testing.T, base, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGenerateMappingsMergeAdoptsUserFolders locks adopt-by-tag: the user's
// tagged folder is adopted (most-real-files tiebreak across same-tag folders),
// a bare-TAG folder is adopted, and a platform with no candidate gets the clean
// "<Display> (<TAG>)" (never the " RomM " wart).
func TestGenerateMappingsMergeAdoptsUserFolders(t *testing.T) {
	base := mergeTestEnv(t)
	mkdirAll(t, base, ".system/tg5040/paks/Emus/FC.pak")

	// Two GBA-tagged folders: "GBA Favs (gba)" (1 real file, lower-case tag) vs
	// "Game Boy Advance (GBA)" (2 real files + a marked Lodor stub that must not
	// count) — the second wins the tiebreak.
	writeCard(t, base, "Roms/GBA Favs (gba)/Only One.gba", []byte("R"))
	writeCard(t, base, "Roms/Game Boy Advance (GBA)/A.gba", []byte("R"))
	writeCard(t, base, "Roms/Game Boy Advance (GBA)/B.gba", []byte("R"))
	writeCard(t, base, "Roms/Game Boy Advance (GBA)/"+platform.MarkerCloud+"C.gba", nil)
	// Bare-TAG folder for SNES (tag SFC) — the no-paren NextUI binding.
	writeCard(t, base, "Roms/SFC/Super Mario World.smc", []byte("R"))
	// Distractors: hidden and .disabled dirs are never candidates.
	mkdirAll(t, base, "Roms/.hidden (GBA)")
	mkdirAll(t, base, "Roms/Old GBA (GBA).disabled")

	lister := &mergeFake{platforms: []romm.Platform{
		{ID: 1, FsSlug: "gba", Name: "Game Boy Advance"},
		{ID: 2, FsSlug: "snes", Name: "Super Nintendo"},
		{ID: 3, FsSlug: "nes", Name: "Nintendo Entertainment System"},
	}}
	m, gen, _, err := GenerateDirectoryMappings(lister, config.MirrorModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 3 {
		t.Fatalf("generated=%d want 3", gen)
	}
	if got := m["gba"].RelativePath; got != "Game Boy Advance (GBA)" {
		t.Errorf("gba adopted %q, want the most-files folder", got)
	}
	if got := m["snes"].RelativePath; got != "SFC" {
		t.Errorf("snes adopted %q, want the bare-TAG folder", got)
	}
	if got := m["nes"].RelativePath; got != "Nintendo Entertainment System (FC)" {
		t.Errorf("nes folder %q, want clean create (no candidate)", got)
	}
	if strings.Contains(m["nes"].RelativePath, " RomM (") {
		t.Errorf("merge must never create ' RomM ' folders")
	}
}

// TestMirrorMergeDedupAdoptsUserFile runs a full merge-mode MirrorCatalog over a
// user card and locks the §2 collision policy: exact-named user file -> adopted
// (index takes their path, NO stub, manifest never claims it); same-tag sibling
// match -> adopted cross-folder; unmatched server game -> ✘ stub; user 0-byte
// decoy -> skipped, never stubbed or claimed; user romhack -> untouched.
func TestMirrorMergeDedupAdoptsUserFile(t *testing.T) {
	base := mergeTestEnv(t)

	userRom := writeCard(t, base, "Roms/Game Boy Advance (GBA)/Pokemon - Emerald Version (USA, Europe).gba", []byte("USER EMERALD"))
	romhack := writeCard(t, base, "Roms/Game Boy Advance (GBA)/My Romhack.gba", []byte("HACK"))
	sibling := writeCard(t, base, "Roms/GBA (GBA)/Mario Kart.gba", []byte("USER MK"))
	decoy := writeCard(t, base, "Roms/Game Boy Advance (GBA)/Broken Copy.gba", nil)

	fake := &mergeFake{
		platforms: []romm.Platform{{ID: 1, FsSlug: "gba", Name: "Game Boy Advance"}},
		roms: map[int][]romm.Rom{1: {
			{ID: 100, PlatformFsSlug: "gba", FsName: "Pokemon - Emerald Version (USA, Europe).gba",
				Files: []romm.RomFile{{FileName: "Pokemon - Emerald Version (USA, Europe).gba"}}},
			{ID: 200, PlatformFsSlug: "gba", FsName: "Mario Kart.gba",
				Files: []romm.RomFile{{FileName: "Mario Kart.gba"}}},
			{ID: 300, PlatformFsSlug: "gba", FsName: "Zelda.gba",
				Files: []romm.RomFile{{FileName: "Zelda.gba"}}},
			{ID: 400, PlatformFsSlug: "gba", FsName: "Broken Copy.gba",
				Files: []romm.RomFile{{FileName: "Broken Copy.gba"}}},
		}},
	}
	cfg := &config.Config{MirrorMode: config.MirrorModeMerge}

	created, existing, skipped, _, _, adopted, err := MirrorCatalog(fake, cfg, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if adopted != 2 {
		t.Errorf("adopted=%d want 2 (Emerald + sibling Mario Kart)", adopted)
	}
	if created != 1 {
		t.Errorf("created=%d want 1 (Zelda stub)", created)
	}
	if existing != 2 || skipped != 1 {
		t.Errorf("existing=%d skipped=%d want 2/1", existing, skipped)
	}

	adoptedDir := filepath.Join(base, "Roms", "Game Boy Advance (GBA)")
	// User files byte-identical, never renamed/marked.
	for p, want := range map[string]string{userRom: "USER EMERALD", romhack: "HACK", sibling: "USER MK"} {
		data, rerr := os.ReadFile(p)
		if rerr != nil || string(data) != want {
			t.Errorf("user file %s touched (err=%v data=%q)", filepath.Base(p), rerr, data)
		}
	}
	// NO duplicate stub for adopted/decoy names, under any marker.
	for _, name := range []string{
		"Pokemon - Emerald Version (USA, Europe).gba", "Mario Kart.gba", "Broken Copy.gba",
	} {
		for _, mk := range []string{platform.MarkerCloud, platform.MarkerOnDevice} {
			if _, err := os.Stat(filepath.Join(adoptedDir, mk+name)); !os.IsNotExist(err) {
				t.Errorf("stub %q created beside user file", mk+name)
			}
		}
	}
	// The unmatched game IS stubbed, marked, in the adopted folder.
	zStub := filepath.Join(adoptedDir, platform.MarkerCloud+"Zelda.gba")
	if fi, err := os.Stat(zStub); err != nil || fi.Size() != 0 {
		t.Errorf("Zelda stub missing/non-empty: %v", err)
	}
	// Decoy: still 0 bytes, unclaimed.
	if fi, err := os.Stat(decoy); err != nil || fi.Size() != 0 {
		t.Errorf("user 0-byte decoy touched: %v", err)
	}

	// Index adoption: by_id points at THEIR paths; their names resolve to rom_ids.
	idx, err := loadIndex(IndexPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	pi := idx.Platforms["gba"]
	if got := pi.ByID[100]; got != "/Roms/Game Boy Advance (GBA)/Pokemon - Emerald Version (USA, Europe).gba" {
		t.Errorf("by_id[100] = %q, want the USER's path (adoption)", got)
	}
	if got := pi.ByID[200]; got != "/Roms/GBA (GBA)/Mario Kart.gba" {
		t.Errorf("by_id[200] = %q, want the sibling-folder user path", got)
	}
	if id, ok := ResolveRomID(cfg, userRom); !ok || id != 100 {
		t.Errorf("ResolveRomID(user Emerald) = (%d,%v), want (100,true) — save-sync attaches to their file", id, ok)
	}
	if id, ok := ResolveRomID(cfg, sibling); !ok || id != 200 {
		t.Errorf("ResolveRomID(sibling Mario Kart) = (%d,%v), want (200,true)", id, ok)
	}

	// Manifest: owns the stub, never the user's files.
	man := platform.LoadManifest()
	if !man.OwnsKind(zStub, platform.ManifestStub) {
		t.Errorf("manifest does not own the Zelda stub")
	}
	for _, p := range []string{userRom, romhack, sibling, decoy} {
		if man.Owns(p) {
			t.Errorf("manifest claims the user's %s", filepath.Base(p))
		}
	}
}
