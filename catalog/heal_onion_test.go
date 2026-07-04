//go:build onion

package catalog

import (
	"testing"

	"lodor/config"
	"lodor/platform"
)

// TestHealMirrorFoldersOnionNoop is the #171 regression guard for OnionOS: bare-TAG
// folders ("GG", "FC") ARE canonical on Onion, so the heal pass must leave them alone.
// platform.CanonicalMirrorFolder returns "" for every slug here (only muOS overrides it).
func TestHealMirrorFoldersOnionNoop(t *testing.T) {
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
			t.Errorf("onion heal MUTATED %s: %q -> %q (bare tag is canonical on Onion)", slug, m.RelativePath, got)
		}
	}
	if got := platform.CanonicalMirrorFolder("gamegear"); got != "" {
		t.Errorf("onion CanonicalMirrorFolder(gamegear) = %q, want \"\" (no heal on Onion)", got)
	}
}
