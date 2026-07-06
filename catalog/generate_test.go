//go:build !onion && !muos && !knulli

package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/romm"
)

// fakeLister is a platformLister returning a fixed platform set, so mapping generation
// is tested without a live server.
type fakeLister struct{ platforms []romm.Platform }

func (f fakeLister) GetPlatforms() ([]romm.Platform, error) { return f.platforms, nil }

// TestGenerateDirectoryMappings locks two behaviors at once on a tg5040-shaped card:
//   - the host-aware pak gate (#1): N64 has a known tag but NO N64.pak installed, so it
//     is SKIPPED (never stubbed) while NES (FC.pak present) is mapped;
//   - the mode-aware folder naming (#2): "own" => "<Display> (<TAG>)", "separate" =>
//     "<Display> RomM (<TAG>)".
func TestGenerateDirectoryMappings(t *testing.T) {
	sd := t.TempDir()
	t.Setenv("SDCARD_PATH", sd)
	t.Setenv("PLATFORM", "tg5040")
	// FC.pak present (so NES maps); deliberately NO N64.pak (so N64 is gated out).
	if err := os.MkdirAll(filepath.Join(sd, ".system", "tg5040", "paks", "Emus", "FC.pak"), 0o755); err != nil {
		t.Fatal(err)
	}

	lister := fakeLister{platforms: []romm.Platform{
		{ID: 1, FsSlug: "nes", Name: "Nintendo Entertainment System"},
		{ID: 2, FsSlug: "n64", Name: "Nintendo 64"},
	}}

	t.Run("own", func(t *testing.T) {
		m, gen, skip, err := GenerateDirectoryMappings(lister, config.MirrorModeOwn)
		if err != nil {
			t.Fatal(err)
		}
		if gen != 1 || skip != 1 {
			t.Fatalf("generated=%d skipped=%d, want 1/1 (n64 gated out)", gen, skip)
		}
		if _, ok := m["n64"]; ok {
			t.Error("n64 mapped despite no N64.pak — host gate failed")
		}
		if got := m["nes"].RelativePath; got != "Nintendo Entertainment System (FC)" {
			t.Errorf("own nes folder = %q, want %q", got, "Nintendo Entertainment System (FC)")
		}
	})

	t.Run("separate", func(t *testing.T) {
		m, gen, skip, err := GenerateDirectoryMappings(lister, config.MirrorModeSeparate)
		if err != nil {
			t.Fatal(err)
		}
		if gen != 1 || skip != 1 {
			t.Fatalf("generated=%d skipped=%d, want 1/1", gen, skip)
		}
		if got := m["nes"].RelativePath; got != "Nintendo Entertainment System RomM (FC)" {
			t.Errorf("separate nes folder = %q, want %q", got, "Nintendo Entertainment System RomM (FC)")
		}
	})
}

// TestConsoleCoverageMapping locks the 2026-06-27 console-coverage fix end to end: a
// strong device (tg5040) with installed Emus paks for Dreamcast (DC), Saturn (SATURN),
// Arcade (FBN) and N64 now MAPS those RomM platforms. Before the fix, dreamcast had no
// engine tag at all and saturn's tag was empty, so the mirror skipped them even with the
// pak present. GameCube (gc) has no viable emulator and stays unmapped.
func TestConsoleCoverageMapping(t *testing.T) {
	sd := t.TempDir()
	t.Setenv("SDCARD_PATH", sd)
	t.Setenv("PLATFORM", "tg5040")
	for _, tag := range []string{"FC", "DC", "SATURN", "FBN", "N64"} {
		if err := os.MkdirAll(filepath.Join(sd, "Emus", "tg5040", tag+".pak"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lister := fakeLister{platforms: []romm.Platform{
		{ID: 1, FsSlug: "dreamcast", Name: "Dreamcast"},
		{ID: 2, FsSlug: "saturn", Name: "Sega Saturn"},
		{ID: 3, FsSlug: "fbneo", Name: "Arcade"},
		{ID: 4, FsSlug: "n64", Name: "Nintendo 64"},
		{ID: 5, FsSlug: "gc", Name: "Nintendo GameCube"},
	}}
	m, gen, skip, err := GenerateDirectoryMappings(lister, config.MirrorModeSeparate)
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"dreamcast", "saturn", "fbneo", "n64"} {
		if _, ok := m[slug]; !ok {
			t.Errorf("%s not mapped despite its Emus pak present — console-coverage gap", slug)
		}
	}
	if _, ok := m["gc"]; ok {
		t.Error("gc mapped — GameCube has no viable tg5040 emulator and must stay unmapped")
	}
	if gen != 4 || skip != 1 {
		t.Fatalf("generated=%d skipped=%d, want 4/1 (gc skipped)", gen, skip)
	}
}
