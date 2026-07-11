package platform

// Dot-hidden disc folder coverage (lodor#7, hardware-confirmed UX bug): the
// per-game multi-disc folder must be DOT-PREFIXED so MinUI-family launchers hide
// it, and it must still sit exactly beside the .m3u (the m3u's relative lines are
// resolved against its own directory). NO build tag: these invariants hold for
// EVERY CFW variant's MultiDiscDir, so the test runs under every -tags build.

import (
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/romm"
)

func TestDiscFolderName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Final Fantasy VII (USA)", ".Final Fantasy VII (USA)"},
		// Already-hidden name: left alone — double-dotting would form a ".."
		// prefix the defensive m3u/manifest line filters rightly refuse.
		{".hack//Infection (USA)", ".hack//Infection (USA)"},
		{"", ""},
	}
	for _, c := range cases {
		if got := DiscFolderName(c.in); got != c.want {
			t.Errorf("DiscFolderName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMultiDiscDirDotHidden(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	cfg := &config.Config{
		DirectoryMappings: map[string]config.DirMapping{
			"psx": {Slug: "psx", RelativePath: "Sony PlayStation (PS)"},
		},
	}
	rom := romm.Rom{
		PlatformFsSlug:      "psx",
		PlatformDisplayName: "Sony PlayStation",
		HasMultipleFiles:    true,
		FsNameNoExt:         "Final Fantasy VII (USA)",
		Files:               []romm.RomFile{{ID: 1, FileName: "Final Fantasy VII (USA) (Disc 1).chd"}},
	}
	discDir := MultiDiscDir(cfg, rom)
	if discDir == "" {
		t.Fatal("MultiDiscDir returned empty for a mapped multi-disc rom")
	}
	if filepath.Base(discDir) != ".Final Fantasy VII (USA)" {
		t.Errorf("disc folder = %q, want the dot-hidden name .Final Fantasy VII (USA)", filepath.Base(discDir))
	}
	// The .m3u sits one level up, beside the folder — the layout every lane's
	// launcher and the m3u's relative disc lines depend on.
	if m3u := LocalRomPath(cfg, rom); filepath.Dir(m3u) != filepath.Dir(discDir) {
		t.Errorf("m3u dir %q != disc folder parent %q", filepath.Dir(m3u), filepath.Dir(discDir))
	}
	if got := MultiDiscDir(cfg, romm.Rom{}); got != "" {
		t.Errorf("MultiDiscDir(no slug) = %q, want empty", got)
	}
}
