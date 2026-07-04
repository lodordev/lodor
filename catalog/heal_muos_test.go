//go:build muos

package catalog

import (
	"testing"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// TestHealMirrorFoldersMuos locks the #171 fix: a config.json carried across CFWs (the
// onion-card config, with onion bare-TAG folders + PSP absent) is HEALED so every muOS
// mirror folder is the catalogue name info/assign recognises — including the aliased
// slugs (gamegear/gb/nes/snes/genesis/mastersystem) that regressed to short tags before.
func TestHealMirrorFoldersMuos(t *testing.T) {
	t.Chdir(t.TempDir()) // WriteDirectoryMappings persists to ./config.json
	cfg := &config.Config{DirectoryMappings: map[string]config.DirMapping{
		"gamegear":     {Slug: "gamegear", RelativePath: "GG"},  // onion bare tag -> heal
		"gb":           {Slug: "gb", RelativePath: "GB"},        // onion bare tag -> heal
		"nes":          {Slug: "nes", RelativePath: "FC"},       // onion bare tag -> heal
		"snes":         {Slug: "snes", RelativePath: "SFC"},     // onion bare tag -> heal
		"genesis":      {Slug: "genesis", RelativePath: "MD"},   // onion bare tag -> heal
		"mastersystem": {Slug: "mastersystem", RelativePath: "MS"}, // onion bare tag -> heal
		"psp":          {Slug: "psp", RelativePath: "Sony PlayStation Portable"}, // already right
		"cave-story":   {Slug: "cave-story", RelativePath: "Weird Custom"},       // known slug -> heal
	}}
	healMirrorFolders(cfg)

	want := map[string]string{
		"gamegear":     "Sega Game Gear",
		"gb":           "Nintendo Game Boy",
		"nes":          "Nintendo NES - Famicom",
		"snes":         "Nintendo SNES - SFC",
		"genesis":      "Sega Mega Drive - Genesis",
		"mastersystem": "Sega Master System",
		"psp":          "Sony PlayStation Portable",
		"cave-story":   "Cave Story",
	}
	for slug, w := range want {
		if got := cfg.DirectoryMappings[slug].RelativePath; got != w {
			t.Errorf("healed %s = %q, want catalogue name %q", slug, got, w)
		}
	}
	// CanonicalMirrorFolder is the catalogue name for a known slug, "" for an unknown one.
	if got := platform.CanonicalMirrorFolder("gamegear"); got != "Sega Game Gear" {
		t.Errorf("CanonicalMirrorFolder(gamegear) = %q, want Sega Game Gear", got)
	}
	if got := platform.CanonicalMirrorFolder("totally-unknown"); got != "" {
		t.Errorf("CanonicalMirrorFolder(unknown) = %q, want empty (leave untouched)", got)
	}
}

// TestGenerateDirectoryMappingsMuos locks that a FRESH (empty) config auto-generates
// catalogue-name folders for the aliased slugs — the correct baseline the heal restores.
func TestGenerateDirectoryMappingsMuos(t *testing.T) {
	t.Setenv("LODOR_SKIP_EMU_GATE", "1")
	lister := fakeListerMuos{platforms: muosTestPlatforms()}
	m, gen, _, err := GenerateDirectoryMappings(lister, config.MirrorModeOwn)
	if err != nil {
		t.Fatal(err)
	}
	if gen == 0 {
		t.Fatal("generated 0 mappings")
	}
	want := map[string]string{
		"gamegear": "Sega Game Gear",
		"gb":       "Nintendo Game Boy",
		"nes":      "Nintendo NES - Famicom",
		"snes":     "Nintendo SNES - SFC",
		"psp":      "Sony PlayStation Portable",
	}
	for slug, w := range want {
		if got := m[slug].RelativePath; got != w {
			t.Errorf("generated %s = %q, want %q", slug, got, w)
		}
	}
}

type fakeListerMuos struct{ platforms []romm.Platform }

func (f fakeListerMuos) GetPlatforms() ([]romm.Platform, error) { return f.platforms, nil }

func muosTestPlatforms() []romm.Platform {
	return []romm.Platform{
		{ID: 1, FsSlug: "gamegear", Name: "Sega Game Gear"},
		{ID: 2, FsSlug: "gb", Name: "Game Boy"},
		{ID: 3, FsSlug: "nes", Name: "Nintendo Entertainment System"},
		{ID: 4, FsSlug: "snes", Name: "Super Nintendo Entertainment System"},
		{ID: 5, FsSlug: "psp", Name: "PlayStation Portable"},
	}
}
