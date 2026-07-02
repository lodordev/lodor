//go:build !onion

package platform

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/romm"
)

// TestHasEmuPak locks the host-aware Emu-pak gate (the OoT-class fix): a tag is
// "launchable" only when a matching <TAG>.pak directory exists at the builtin
// (.system/<plat>/paks/Emus) or user (Emus/<plat>) location. Modeled on a tg5040
// (Trimui Smart Pro) card, which ships FC but NO N64 pak — so FC launches and N64 must
// be gated out (its games can't run).
func TestHasEmuPak(t *testing.T) {
	sd := t.TempDir()
	t.Setenv("SDCARD_PATH", sd)
	t.Setenv("PLATFORM", "tg5040")

	// Builtin FC.pak (the .system path) and a user-installed GG.pak (the Emus path).
	mustMkPak(t, filepath.Join(sd, ".system", "tg5040", "paks", "Emus", "FC.pak"))
	mustMkPak(t, filepath.Join(sd, "Emus", "tg5040", "GG.pak"))

	if !HasEmuPak("FC") {
		t.Error("HasEmuPak(FC) = false, want true (builtin .system pak present)")
	}
	if !HasEmuPak("GG") {
		t.Error("HasEmuPak(GG) = false, want true (user Emus pak present)")
	}
	if HasEmuPak("N64") {
		t.Error("HasEmuPak(N64) = true, want false (tg5040 ships no N64 pak — must be gated out)")
	}
	if HasEmuPak("") {
		t.Error("HasEmuPak(\"\") = true, want false")
	}
}

// TestLocalBasenameModes locks the #3 disambiguation: "own" mode keeps the server's
// basename byte-identical; "separate"/"merge" append " (RomM)" so a stub's save can't
// collide with a user's local game of the same name binding the same TAG.
func TestLocalBasenameModes(t *testing.T) {
	single := romm.Rom{
		PlatformFsSlug: "n64",
		Files:          []romm.RomFile{{FileName: "Zelda (USA).z64"}},
	}
	multi := romm.Rom{
		PlatformFsSlug:   "psx",
		HasMultipleFiles: true,
		FsNameNoExt:      "Final Fantasy VII (USA)",
		Files:            []romm.RomFile{{ID: 1, FileName: "Disc 1.chd"}},
	}

	cases := []struct {
		mode   string
		single string
		multi  string
	}{
		{config.MirrorModeOwn, "Zelda (USA)", "Final Fantasy VII (USA)"},
		{config.MirrorModeSeparate, "Zelda (USA) (RomM)", "Final Fantasy VII (USA) (RomM)"},
		// merge is CANONICAL by design (C1 §2): dedup/adoption needs the byte-identical
		// name; the ✘/✓ markers are what keep Lodor files distinct from the user's.
		{config.MirrorModeMerge, "Zelda (USA)", "Final Fantasy VII (USA)"},
	}
	for _, c := range cases {
		cfg := &config.Config{MirrorMode: c.mode}
		if got := LocalBasename(cfg, single); got != c.single {
			t.Errorf("[%s] LocalBasename(single) = %q, want %q", c.mode, got, c.single)
		}
		if got := LocalBasename(cfg, multi); got != c.multi {
			t.Errorf("[%s] LocalBasename(multi) = %q, want %q", c.mode, got, c.multi)
		}
	}
}

// TestLocalRomPathModes confirms the on-disk path uses the mapped (mode-named) folder
// and the mode-aware basename, and that "own" mode stays byte-identical to the legacy
// "<folder>/<server filename>" layout.
func TestLocalRomPathModes(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	roms := filepath.Join(base, "Roms")

	single := romm.Rom{
		PlatformFsSlug: "n64",
		Files:          []romm.RomFile{{FileName: "Zelda (USA).z64"}},
	}
	multi := romm.Rom{
		PlatformFsSlug:   "psx",
		HasMultipleFiles: true,
		FsNameNoExt:      "Final Fantasy VII (USA)",
		Files:            []romm.RomFile{{ID: 1, FileName: "Disc 1.chd"}},
	}

	own := &config.Config{
		MirrorMode: config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{
			"n64": {Slug: "n64", RelativePath: "Nintendo 64 (N64)"},
			"psx": {Slug: "psx", RelativePath: "Sony PlayStation (PS)"},
		},
	}
	sep := &config.Config{
		MirrorMode: config.MirrorModeSeparate,
		DirectoryMappings: map[string]config.DirMapping{
			"n64": {Slug: "n64", RelativePath: "Nintendo 64 RomM (N64)"},
			"psx": {Slug: "psx", RelativePath: "Sony PlayStation RomM (PS)"},
		},
	}

	checks := []struct {
		name string
		cfg  *config.Config
		rom  romm.Rom
		want string
	}{
		{"own single", own, single, filepath.Join(roms, "Nintendo 64 (N64)", "Zelda (USA).z64")},
		{"own multi", own, multi, filepath.Join(roms, "Sony PlayStation (PS)", "Final Fantasy VII (USA).m3u")},
		{"separate single", sep, single, filepath.Join(roms, "Nintendo 64 RomM (N64)", "Zelda (USA) (RomM).z64")},
		{"separate multi", sep, multi, filepath.Join(roms, "Sony PlayStation RomM (PS)", "Final Fantasy VII (USA) (RomM).m3u")},
	}
	for _, c := range checks {
		if got := LocalRomPath(c.cfg, c.rom); got != c.want {
			t.Errorf("%s: LocalRomPath = %q, want %q", c.name, got, c.want)
		}
	}
}

func mustMkPak(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// TestPrimaryTagConsoleCoverage locks the engine tags for consoles that were previously
// unmapped (dreamcast) or empty (saturn, jaguar/atarijaguar), so a strong device with
// the matching Emus pak can sync them. GameCube (gc) has no viable handheld emulator and
// must stay unmapped (the honest "hardware can't" column).
func TestPrimaryTagConsoleCoverage(t *testing.T) {
	cases := map[string]string{
		"dreamcast":   "DC",
		"saturn":      "SATURN",
		"jaguar":      "JAGUAR",
		"atarijaguar": "JAGUAR",
		"n64":         "N64",
		"psp":         "PSP",
		"fbneo":       "FBN",
		"arcade":      "FBN",
	}
	for slug, want := range cases {
		got, ok := PrimaryTag(slug)
		if !ok || got != want {
			t.Errorf("PrimaryTag(%q) = %q,%v; want %q,true", slug, got, ok, want)
		}
	}
	if _, ok := PrimaryTag("gc"); ok {
		t.Error("PrimaryTag(gc) ok=true; GameCube must stay unmapped (no viable handheld emulator)")
	}
}
