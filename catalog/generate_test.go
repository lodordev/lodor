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
