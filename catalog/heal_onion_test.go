//go:build onion

package catalog

import (
	"testing"

	"lodor/config"
	"lodor/platform"
)

// TestHealMirrorFoldersOnionBareTagNoop: bare-TAG folders ("GG", "FC", "SFC") ARE the
// canonical OnionOS folders, so the heal pass must leave them alone (the #171 guard).
func TestHealMirrorFoldersOnionBareTagNoop(t *testing.T) {
	t.Chdir(t.TempDir())
	orig := map[string]config.DirMapping{
		"gamegear": {Slug: "gamegear", RelativePath: "GG"},
		"nes":      {Slug: "nes", RelativePath: "FC"},
		"snes":     {Slug: "snes", RelativePath: "SFC"},
	}
	cfg := &config.Config{DirectoryMappings: map[string]config.DirMapping{}}
	for k, v := range orig {
		cfg.DirectoryMappings[k] = v
	}
	healMirrorFolders(cfg)
	for slug, m := range orig {
		if got := cfg.DirectoryMappings[slug].RelativePath; got != m.RelativePath {
			t.Errorf("onion heal MUTATED canonical %s: %q -> %q (bare tag is canonical on Onion)", slug, m.RelativePath, got)
		}
	}
	if got := platform.CanonicalMirrorFolder("gamegear"); got != "GG" {
		t.Errorf("onion CanonicalMirrorFolder(gamegear) = %q, want \"GG\"", got)
	}
}

// TestHealMirrorFoldersOnionSnapsMinUIForm is the lodor#68 regression guard: a
// config.json carried from a MinUI/LodorOS Mini install seeds MinUI-form mappings
// ("Game Boy Advance (GBA)", and even MinUI tags like "...(SMS)" where OnionOS wants MS).
// Those folders are invisible to OnionOS MainUI, so the heal pass MUST snap every one
// back to the bare OnionOS TAG the frontend actually scans.
func TestHealMirrorFoldersOnionSnapsMinUIForm(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := &config.Config{DirectoryMappings: map[string]config.DirMapping{
		"gba":          {Slug: "gba", RelativePath: "Game Boy Advance (GBA)"},
		"mastersystem": {Slug: "mastersystem", RelativePath: "Sega Master System-Mark III (SMS)"},
		"fbneo":        {Slug: "fbneo", RelativePath: "Arcade (FBN)"},
		"genesis":      {Slug: "genesis", RelativePath: "Sega Mega Drive-Genesis (MD)"},
	}}
	want := map[string]string{
		"gba":          "GBA",
		"mastersystem": "MS",
		"fbneo":        "FBNEO",
		"genesis":      "MD",
	}
	healMirrorFolders(cfg)
	for slug, w := range want {
		if got := cfg.DirectoryMappings[slug].RelativePath; got != w {
			t.Errorf("onion heal %s: got %q, want bare tag %q (lodor#68)", slug, got, w)
		}
	}
}
