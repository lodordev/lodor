//go:build !muos

package catalog

import (
	"os"
	"testing"
	"time"
)

// TestMuosHistoryGateOff pins that on every non-muOS build the injection call site
// is dead code: no history dir is created, no pointer written — foreign layouts
// never grow muOS files (the gamelistEnabled discipline).
func TestMuosHistoryGateOff(t *testing.T) {
	dir := t.TempDir() + "/history"
	t.Setenv("MUOS_HISTORY_DIR", dir)
	maybeInjectMuosHistory([]ContinueEntry{
		{Rel: "/Roms/Sega Game Gear/Game.gg", T: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)},
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("non-muOS build touched the history dir (err=%v)", err)
	}
}
