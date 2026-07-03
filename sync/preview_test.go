package sync

// Cross-device preview tests (task #149 Lane-A half): newest-record selection
// (ghost/meta discipline) and the on-card landing-path convention.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lodor/romm"
)

func TestNewestPreviewMeta(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	saves := []romm.Save{
		{ID: 1, FileName: "Game.gba.sav", FileSizeBytes: 100, UpdatedAt: t0.Add(9 * time.Hour)}, // real save: never a preview
		{ID: 2, FileName: "Game.gba.lodorshot.png", FileSizeBytes: 900, UpdatedAt: t0},
		{ID: 3, FileName: "Game.gba.lodorshot.png", FileSizeBytes: 950, UpdatedAt: t0.Add(2 * time.Hour)}, // newest usable
		{ID: 4, FileName: "Game.gba.lodorshot.png", FileSizeBytes: 0, UpdatedAt: t0.Add(5 * time.Hour)},   // ghost preview: skip
		{ID: 5, FileName: "Game.gba.lodortime", FileSizeBytes: 64, UpdatedAt: t0.Add(6 * time.Hour)},      // playtime record: not a preview
	}
	best, ok := NewestPreviewMeta(saves)
	if !ok || best.ID != 3 {
		t.Fatalf("NewestPreviewMeta = (%+v, %v), want ID 3", best, ok)
	}
	if _, ok := NewestPreviewMeta([]romm.Save{{ID: 1, FileName: "Game.gba.sav", FileSizeBytes: 1}}); ok {
		t.Error("a save-only list produced a preview")
	}
}

func TestPreviewLocalPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SDCARD_PATH", base)

	romPath := filepath.Join(base, "Roms", "Nintendo 64 (N64)", "✓ GoldenEye (USA).z64")

	// capability gate: no .minui shared dir -> "" (foreign layout, never spray)
	if got := PreviewLocalPath(romPath); got != "" {
		t.Errorf("no .minui dir: got %q, want \"\"", got)
	}

	if err := os.MkdirAll(filepath.Join(base, ".userdata", "shared", ".minui"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, ".userdata", "shared", ".minui", "N64", "✓ GoldenEye (USA).z64.auto.png")
	if got := PreviewLocalPath(romPath); got != want {
		t.Errorf("PreviewLocalPath = %q, want %q", got, want)
	}

	// folder without a parenthetical TAG names itself
	p2 := filepath.Join(base, "Roms", "Homebrew", "demo.gba")
	want2 := filepath.Join(base, ".userdata", "shared", ".minui", "Homebrew", "demo.gba.auto.png")
	if got := PreviewLocalPath(p2); got != want2 {
		t.Errorf("no-tag folder = %q, want %q", got, want2)
	}

	if got := PreviewLocalPath(""); got != "" {
		t.Errorf("empty path: got %q", got)
	}
}

func TestMetaSlotForPreview(t *testing.T) {
	if got := MetaSlotFor("Game.gba.lodorshot.png"); got != "lodorshot" {
		t.Errorf("slot = %q, want lodorshot", got)
	}
	if got := MetaSlotFor("Game.gba.lodortime"); got != "lodortime" {
		t.Errorf("slot = %q, want lodortime", got)
	}
}
