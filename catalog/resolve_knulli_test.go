//go:build knulli

package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// Folder-name-native resolve on Knulli (task #186, riding the #182 fallback).
//
// Knulli/Batocera names its roms/ system folders by the BARE Batocera slug
// ("megadrive", "gba") — no MinUI "(TAG)" suffix — so with directory_mappings
// absent/stale, slugForRomPath must reverse the bare folder name directly via
// platform.FsSlugForTag (the same folder-name-native fallback the muOS build
// proved in resolve_muos_test.go). Includes the megadrive exception: the folder
// is Batocera's name, the resolved slug is RomM's ("genesis").

// knulliResolveEnv writes a catalog index mapping one ROM to a rom_id, with the
// ROM living in the bare Batocera folder and NO directory_mappings in cfg.
func knulliResolveEnv(t *testing.T, slug, folder, fsName string, id int) (*config.Config, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))

	// NO directory_mappings: the folder-name-native resolve must not depend on them.
	cfg := &config.Config{MirrorMode: config.MirrorModeOwn}
	stem := fsName[:len(fsName)-len(filepath.Ext(fsName))]
	rom := romm.Rom{
		ID:             id,
		PlatformFsSlug: slug,
		FsName:         fsName,
		FsNameNoExt:    stem,
		Files:          []romm.RomFile{{FileName: fsName}},
	}

	idx := index{Version: 1, Platforms: map[string]platformIndex{
		slug: {
			ByBasename: map[string]int{platform.LocalBasename(cfg, rom): rom.ID},
			ByFsname:   map[string]int{rom.FsName: rom.ID},
			ByID:       map[int]string{},
		},
	}}
	if err := os.MkdirAll(filepath.Dir(IndexPath(cfg)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeIndexAtomic(IndexPath(cfg), idx); err != nil {
		t.Fatal(err)
	}
	return cfg, filepath.Join(base, "Roms", folder, fsName)
}

// TestResolveKnulliBareFolderNoMappings: a ROM inside a bare Batocera slug folder
// must reverse to its rom_id with NO directory_mapping present — including the
// megadrive→genesis exception folder.
func TestResolveKnulliBareFolderNoMappings(t *testing.T) {
	cases := []struct {
		slug, folder, fsName string
		id                   int
	}{
		{"gba", "gba", "Metroid Fusion (USA).gba", 4242},
		{"genesis", "megadrive", "Sonic The Hedgehog (USA, Europe).md", 1991},
	}
	for _, c := range cases {
		cfg, romPath := knulliResolveEnv(t, c.slug, c.folder, c.fsName, c.id)
		got, ok := ResolveRomID(cfg, romPath)
		if !ok || got != c.id {
			t.Errorf("ResolveRomID(%q) = (%d,%v), want (%d,true) — bare Batocera folder must resolve with no directory_mapping",
				romPath, got, ok, c.id)
		}
	}
}

// TestSlugForKnulliBareFolderNoMappings pins the slug reversal itself:
// "megadrive" resolves to the RomM "genesis" fs_slug via the folder-name-native
// path, no mapping required.
func TestSlugForKnulliBareFolderNoMappings(t *testing.T) {
	cfg := &config.Config{MirrorMode: config.MirrorModeOwn}
	romPath := filepath.Join("Roms", "megadrive", "Sonic The Hedgehog (USA, Europe).md")
	slug, ok := slugForRomPath(cfg, romPath)
	if !ok || slug != "genesis" {
		t.Fatalf("slugForRomPath(%q) = (%q,%v), want (genesis,true)", romPath, slug, ok)
	}
}

// TestResolveKnulliStillHonorsExplicitMapping guards the un-regressed path: an
// EXPLICIT directory_mapping still wins (the mirror-written mapping is the
// primary signal; the folder-name-native fallback is additive, never a
// replacement).
func TestResolveKnulliStillHonorsExplicitMapping(t *testing.T) {
	cfg, romPath := knulliResolveEnv(t, "genesis", "megadrive", "Sonic The Hedgehog (USA, Europe).md", 1991)
	cfg.DirectoryMappings = map[string]config.DirMapping{
		"genesis": {Slug: "genesis", RelativePath: "megadrive"},
	}
	got, ok := ResolveRomID(cfg, romPath)
	if !ok || got != 1991 {
		t.Fatalf("ResolveRomID with explicit mapping = (%d,%v), want (1991,true)", got, ok)
	}
}
