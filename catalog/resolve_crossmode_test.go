package catalog

// Cross-mode / cross-decoration resolution regression tests (workstream A1,
// 2026-07-02). A coexist-mode flip re-keys the catalog index (own mode keys
// "Game"; separate/merge key "Game (RomM)") but never renames files already on
// the card, so EVERY on-disk name shape — with or without the leading ✘/✓ state
// marker, with or without the " (RomM)" disambiguator, single-file or .m3u —
// must keep reversing to its rom_id against EITHER index shape. Field origin:
// the Smart Pro card (2026-07-02) carried "✓ Pokemon - Emerald Version
// (USA, Europe).gba" (pre-flip download) against a merge-mode index keyed
// "Pokemon - Emerald Version (USA, Europe) (RomM)".

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
)

// writeTestIndex writes a catalog-index.json with the given gba lookup tables,
// rooted at a temp SDCARD, and returns a cfg mapping the GBA folder.
func writeTestIndex(t *testing.T, byBasename map[string]int, byFsname map[string]int) *config.Config {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))

	cfg := &config.Config{
		DirectoryMappings: map[string]config.DirMapping{"gba": {RelativePath: "Game Boy Advance (GBA)"}},
	}
	idx := index{Version: 1, Platforms: map[string]platformIndex{
		"gba": {ByBasename: byBasename, ByFsname: byFsname, ByID: map[int]string{}},
	}}
	if err := os.MkdirAll(filepath.Dir(IndexPath(cfg)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeIndexAtomic(IndexPath(cfg), idx); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func gbaPath(name string) string {
	return filepath.Join(os.Getenv("SDCARD_PATH"), "Roms", "Game Boy Advance (GBA)", name)
}

// TestResolveLegacyCleanNameAgainstTaggedIndex is the exact Smart Pro shape: the
// index was rebuilt in merge mode (keys carry " (RomM)") while the real download
// on disk still wears its pre-flip clean name under the on-device marker.
func TestResolveLegacyCleanNameAgainstTaggedIndex(t *testing.T) {
	cfg := writeTestIndex(t,
		map[string]int{"Pokemon - Emerald Version (USA, Europe) (RomM)": 12765},
		map[string]int{"Pokemon - Emerald Version (USA, Europe).gba": 12765},
	)
	cases := []string{
		"✓ Pokemon - Emerald Version (USA, Europe).gba", // the field case (by_fsname)
		"✘ Pokemon - Emerald Version (USA, Europe).gba",
		"Pokemon - Emerald Version (USA, Europe).gba",
		"✓ Pokemon - Emerald Version (USA, Europe) (RomM).gba", // current-mode name (by_basename)
	}
	for _, name := range cases {
		id, ok := ResolveRomID(cfg, gbaPath(name))
		if !ok || id != 12765 {
			t.Errorf("ResolveRomID(%q) = (%d,%v), want (12765,true)", name, id, ok)
		}
	}
}

// TestResolveLegacyCleanM3UAgainstTaggedIndex: a multi-file ROM's fs_name is the
// extension-less game folder name, so a legacy clean-named "Game.m3u" can only
// resolve via the stem lookup — the full-base by_fsname lookup can never hit.
func TestResolveLegacyCleanM3UAgainstTaggedIndex(t *testing.T) {
	cfg := writeTestIndex(t,
		map[string]int{"Final Fantasy VII (USA) (RomM)": 77},
		map[string]int{"Final Fantasy VII (USA)": 77}, // multi-file: fs_name = folder name, no ext
	)
	for _, name := range []string{
		"Final Fantasy VII (USA).m3u",
		"✓ Final Fantasy VII (USA).m3u",
		"✘ Final Fantasy VII (USA) (RomM).m3u",
	} {
		id, ok := ResolveRomID(cfg, gbaPath(name))
		if !ok || id != 77 {
			t.Errorf("ResolveRomID(%q) = (%d,%v), want (77,true)", name, id, ok)
		}
	}
}

// TestResolveTaggedNameAgainstCleanIndex is the reverse flip (separate/merge →
// own): the on-disk file still carries " (RomM)" but the rebuilt index keys are
// canonical.
func TestResolveTaggedNameAgainstCleanIndex(t *testing.T) {
	cfg := writeTestIndex(t,
		map[string]int{"Sonic (USA)": 41},
		map[string]int{"Sonic (USA).gba": 41},
	)
	for _, name := range []string{
		"Sonic (USA) (RomM).gba",
		"✓ Sonic (USA) (RomM).gba",
		"✘ Sonic (USA) (RomM).gba",
	} {
		id, ok := ResolveRomID(cfg, gbaPath(name))
		if !ok || id != 41 {
			t.Errorf("ResolveRomID(%q) = (%d,%v), want (41,true)", name, id, ok)
		}
	}
}

// TestResolveCleanNameStillResolves is the LodorOS-fleet regression guard: a
// clean canonical name against a clean (own-mode) index — the shape every
// LodorOS device uses — must keep resolving exactly as before the A1 cascade.
func TestResolveCleanNameStillResolves(t *testing.T) {
	cfg := writeTestIndex(t,
		map[string]int{"Zelda (USA)": 9},
		map[string]int{"Zelda (USA).gba": 9},
	)
	for _, name := range []string{
		"Zelda (USA).gba",
		platform.MarkerCloud + "Zelda (USA).gba",
		platform.MarkerOnDevice + "Zelda (USA).gba",
		"[^] Zelda (USA).gba", // legacy ASCII markers still recognized
		"[v] Zelda (USA).gba",
	} {
		id, ok := ResolveRomID(cfg, gbaPath(name))
		if !ok || id != 9 {
			t.Errorf("ResolveRomID(%q) = (%d,%v), want (9,true)", name, id, ok)
		}
	}
	// And an unknown game still honestly fails to resolve.
	if id, ok := ResolveRomID(cfg, gbaPath("Not In Library.gba")); ok {
		t.Errorf("unknown game resolved to %d, want miss", id)
	}
}
