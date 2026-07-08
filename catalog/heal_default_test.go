//go:build !onion && !muos && !knulli && !android && !lodorandroid

package catalog

import (
	"testing"

	"lodor/config"
	"lodor/platform"
)

// TestHealMirrorFoldersDefaultNoop is the #171 regression guard for MinUI/NextUI: the
// heal pass must NEVER touch a user's mappings on the default build (their relative_path
// is legitimately custom — hand-tuned or merge-adopted). platform.CanonicalMirrorFolder
// returns "" for every slug here, so healMirrorFolders is a pure no-op.
func TestHealMirrorFoldersDefaultNoop(t *testing.T) {
	t.Chdir(t.TempDir())
	orig := map[string]config.DirMapping{
		"gamegear": {Slug: "gamegear", RelativePath: "GG"},
		"nes":      {Slug: "nes", RelativePath: "My Custom NES Folder"},
		"snes":     {Slug: "snes", RelativePath: "Super Nintendo (SUPA)"},
	}
	cfg := &config.Config{DirectoryMappings: map[string]config.DirMapping{}}
	for k, v := range orig {
		cfg.DirectoryMappings[k] = v
	}
	healMirrorFolders(cfg)
	for slug, m := range orig {
		if got := cfg.DirectoryMappings[slug].RelativePath; got != m.RelativePath {
			t.Errorf("default heal MUTATED %s: %q -> %q (must be untouched)", slug, m.RelativePath, got)
		}
	}
	if got := platform.CanonicalMirrorFolder("gamegear"); got != "" {
		t.Errorf("default CanonicalMirrorFolder(gamegear) = %q, want \"\" (no heal on MinUI)", got)
	}
}
