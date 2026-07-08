//go:build android || lodorandroid

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// BASE_PATH relocates the whole tree to the generic sandbox layout (the cross-lane
// contract the off-hardware suite depends on); explicit *_DIR envs win over it.
func TestAndroidBasePathRelocation(t *testing.T) {
	t.Setenv("BASE_PATH", "/sandbox")
	if got := RomsDir(); got != filepath.Join("/sandbox", "Roms") {
		t.Fatalf("RomsDir under BASE_PATH = %q", got)
	}
	if got := SavesDir(); got != filepath.Join("/sandbox", "Saves") {
		t.Fatalf("SavesDir under BASE_PATH = %q", got)
	}
	if got := BiosDir(); got != filepath.Join("/sandbox", "Bios") {
		t.Fatalf("BiosDir under BASE_PATH = %q", got)
	}
	t.Setenv("ROMS_DIR", "/explicit/roms")
	if got := RomsDir(); got != "/explicit/roms" {
		t.Fatalf("ROMS_DIR must beat BASE_PATH, got %q", got)
	}
}

// On-device defaults are RetroArch Android's shared-storage layout.
func TestAndroidDeviceDefaults(t *testing.T) {
	t.Setenv("BASE_PATH", "")
	t.Setenv("ROMS_DIR", "")
	t.Setenv("SAVES_DIR", "")
	t.Setenv("BIOS_DIR", "")
	if got := RomsDir(); got != "/storage/emulated/0/ROMs" {
		t.Fatalf("device RomsDir = %q", got)
	}
	if got := SavesDir(); got != "/storage/emulated/0/RetroArch/saves" {
		t.Fatalf("device SavesDir = %q", got)
	}
	if got := BiosDir(); got != "/storage/emulated/0/RetroArch/system" {
		t.Fatalf("device BiosDir = %q", got)
	}
}

// The save-directory matrix: app pin wins (including the "." flat pin, which must
// clean to the root itself), known slug defaults to the FLAT root (RA ships sorting
// off), unknown slug gets nothing (never blind-write).
func TestAndroidSaveDirectoryMatrix(t *testing.T) {
	t.Setenv("SAVES_DIR", "/ra/saves")

	t.Setenv(saveSubdirEnv, "mgba")
	if got := SaveDirectory("gba"); got != filepath.Join("/ra/saves", "mgba") {
		t.Fatalf("pinned core dir = %q", got)
	}

	t.Setenv(saveSubdirEnv, ".")
	if got := SaveDirectory("gba"); got != "/ra/saves" {
		t.Fatalf(`pin "." must clean to the flat root, got %q`, got)
	}

	t.Setenv(saveSubdirEnv, "")
	if got := SaveDirectory("gba"); got != "/ra/saves" {
		t.Fatalf("default (flat) = %q", got)
	}
	if got := SaveDirectory("not-a-system"); got != "" {
		t.Fatalf("unknown slug must yield no save dir, got %q", got)
	}
}

// Discovery scans the superset — flat root first, then first-level subdirs — unless
// the app pinned the folder.
func TestAndroidEmulatorFoldersSuperset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAVES_DIR", dir)
	t.Setenv(saveSubdirEnv, "")

	for _, sub := range []string{"mgba", "snes9x", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := EmulatorFoldersForFSSlug("gba")
	if len(got) != 3 || got[0] != "." {
		t.Fatalf("superset scan = %v (want [. mgba snes9x] modulo order after '.')", got)
	}

	t.Setenv(saveSubdirEnv, "Pinned")
	if got := EmulatorFoldersForFSSlug("gba"); len(got) != 1 || got[0] != "Pinned" {
		t.Fatalf("pin must win, got %v", got)
	}
}

// RetroArch save naming: stem + save extension.
func TestAndroidSaveFileName(t *testing.T) {
	if got := SaveFileName("Game (USA).gba", "srm"); got != "Game (USA).srm" {
		t.Fatalf("SaveFileName = %q", got)
	}
	if got := SaveFileName("Game (USA).gba", ""); got != "Game (USA)" {
		t.Fatalf("SaveFileName empty ext = %q", got)
	}
}

// The statecores "dir":"." flat-root convention: StateDirFor(".") must resolve to
// the state root itself. This pins the filepath.Join(root, ".") cleaning behavior
// the Android lane's flat RA layout depends on — a states.go refactor that breaks
// it must fail here, not on a device.
func TestAndroidStateDirFlatDot(t *testing.T) {
	t.Setenv("LODOR_STATE_ROOT", "/ra/states")
	if got := StateDirFor("."); got != "/ra/states" {
		t.Fatalf(`StateDirFor(".") = %q, want the root itself`, got)
	}
}

// Marker-less lane: the host-state gate is hard-true (see hoststate_android.go).
func TestAndroidHostShowsStateNatively(t *testing.T) {
	if !HostShowsStateNatively() {
		t.Fatal("android lane must be marker-less (HostShowsStateNatively=true)")
	}
}
