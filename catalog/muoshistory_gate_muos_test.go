//go:build muos

package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMuosHistoryGateOn pins the muOS wrapper end-to-end: env-resolved dir + ROMS
// root, manifest persisted to the pak dir. This is the exact seam --sync-continue
// and the full mirror drive on hardware.
func TestMuosHistoryGateOn(t *testing.T) {
	root := t.TempDir()
	hist := filepath.Join(root, "storage", "info", "history")
	roms := filepath.Join(root, "mmc", "ROMS")
	pak := filepath.Join(root, "app")
	if err := os.MkdirAll(pak, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUOS_HISTORY_DIR", hist)
	t.Setenv("ROMS_DIR", roms)
	t.Setenv("LODOR_PAK_DIR", pak)
	t.Setenv("SDCARD_PATH", filepath.Join(root, "mmc"))

	ts := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	maybeInjectMuosHistory([]ContinueEntry{{Rel: "/Roms/Sega Game Gear/✘ Zilion (USA).gg", T: ts}})

	des, err := os.ReadDir(hist)
	if err != nil || len(des) != 1 {
		t.Fatalf("expected exactly 1 injected pointer, got %d (err=%v)", len(des), err)
	}
	name := des[0].Name()
	if !strings.HasPrefix(name, "✘ Zilion (USA)-") || !strings.HasSuffix(name, ".cfg") {
		t.Fatalf("pointer name = %q, want '✘ Zilion (USA)-<HASH8>.cfg'", name)
	}
	fi, _ := os.Stat(filepath.Join(hist, name))
	if !fi.ModTime().Equal(ts) {
		t.Fatalf("pointer mtime = %v, want feed time %v", fi.ModTime(), ts)
	}
	b, _ := os.ReadFile(filepath.Join(pak, "mirror-manifest.json"))
	if !strings.Contains(string(b), `"history"`) {
		t.Fatal("manifest not persisted with a kind-history entry")
	}
}
