//go:build muos

package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// Field-bug regression — task #182 (muOS directory_mappings DLFAIL).
//
// muOS names its ROMS/ system folders by the CATALOGUE name info/assign binds to an
// emulator ("Nintendo N64", "Sony PlayStation") — NOT MinUI's "<Display> (<TAG>)"
// form. So a ROM path inside such a folder has NO trailing "(TAG)" for the paren
// extract to key off. When directory_mappings are absent/stale (a fresh card, an
// offline --reconcile/--download-queue that never ran the mapping self-heal, or a
// config carried across CFWs), slugForRomPath used to fall straight through to
// tagFromFolderName — which returns "" for a paren-less catalogue name — and ResolveRomID
// reported DLFAIL. The fix is a FOLDER-NAME-NATIVE fallback: on a host whose folders ARE
// catalogue names, resolve the bare folder name directly via platform.FsSlugForTag.
//
// muOS-tagged: platform.FsSlugForTag reverses the muOS catalogue map, so this is a
// muOS-build behavior. On MinUI (folders carry "(TAG)") a bare full name matches no
// tag, so the same fallback is a no-op there (proven by the untagged default suite
// staying green).

// muosResolveEnv writes a catalog index mapping one N64 ROM to a rom_id, with the ROM
// living in the muOS catalogue folder "Nintendo N64" and NO directory_mappings in cfg.
func muosResolveEnv(t *testing.T) (*config.Config, romm.Rom, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))

	// NO directory_mappings: the folder-name-native resolve must not depend on them.
	cfg := &config.Config{MirrorMode: config.MirrorModeOwn}
	rom := romm.Rom{
		ID:             64123,
		PlatformFsSlug: "n64",
		FsName:         "Super Mario 64 (USA).z64",
		FsNameNoExt:    "Super Mario 64 (USA)",
		Files:          []romm.RomFile{{FileName: "Super Mario 64 (USA).z64"}},
	}

	idx := index{Version: 1, Platforms: map[string]platformIndex{
		"n64": {
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

	// The on-disk path muOS launches: ROMS/<catalogue name>/<file>. Only the parent
	// folder name ("Nintendo N64") drives slug resolution.
	romPath := filepath.Join(base, "Roms", "Nintendo N64", "Super Mario 64 (USA).z64")
	return cfg, rom, romPath
}

// TestResolveMuosCatalogueFolderNoMappings is the #182 regression: a ROM inside a muOS
// catalogue-name folder must reverse to its rom_id with NO directory_mapping present.
func TestResolveMuosCatalogueFolderNoMappings(t *testing.T) {
	cfg, rom, romPath := muosResolveEnv(t)

	got, ok := ResolveRomID(cfg, romPath)
	if !ok || got != rom.ID {
		t.Fatalf("ResolveRomID(%q) = (%d,%v), want (%d,true) — muOS catalogue folder must resolve with no directory_mapping (#182 DLFAIL)",
			romPath, got, ok, rom.ID)
	}
}

// TestSlugForMuosCatalogueFolderNoMappings pins the slug reversal itself: "Nintendo N64"
// resolves to the n64 fs_slug via the folder-name-native path, no mapping required.
func TestSlugForMuosCatalogueFolderNoMappings(t *testing.T) {
	cfg := &config.Config{MirrorMode: config.MirrorModeOwn}
	romPath := filepath.Join("Roms", "Nintendo N64", "Super Mario 64 (USA).z64")
	slug, ok := slugForRomPath(cfg, romPath)
	if !ok || slug != "n64" {
		t.Fatalf("slugForRomPath(%q) = (%q,%v), want (n64,true)", romPath, slug, ok)
	}
}

// TestResolveMuosStillHonorsExplicitMapping guards the un-regressed path: an EXPLICIT
// directory_mapping still wins (the mirror-written mapping is the primary signal; the
// folder-name-native fallback is additive, never a replacement).
func TestResolveMuosStillHonorsExplicitMapping(t *testing.T) {
	cfg, rom, romPath := muosResolveEnv(t)
	cfg.DirectoryMappings = map[string]config.DirMapping{
		"n64": {Slug: "n64", RelativePath: "Nintendo N64"},
	}
	got, ok := ResolveRomID(cfg, romPath)
	if !ok || got != rom.ID {
		t.Fatalf("ResolveRomID with explicit mapping = (%d,%v), want (%d,true)", got, ok, rom.ID)
	}
}
