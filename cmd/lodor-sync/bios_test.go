//go:build !onion

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The --check-bios stdout contract: absent BIOS -> bios_ok=0 with the missing file(s)
// and system named; present -> bios_ok=1; a no-BIOS system -> bios_ok=1.
func TestCheckBiosResult_Contract(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("LODOR_ROM_TAG", "")   // force folder-tag resolution
	t.Setenv("LODOR_BIOS_DIRS", "") // no extra dirs

	// A Dreamcast ROM in a "Sega Dreamcast (DC)" folder, BIOS absent.
	dcRom := filepath.Join(base, "Roms", "Sega Dreamcast (DC)", "Game.chd")
	if got := checkBiosResult(dcRom); got != "RESULT bios_ok=0 missing=dc_boot.bin,dc_flash.bin system=Dreamcast" {
		t.Errorf("DC absent: got %q", got)
	}

	// Provide the BIOS -> ok.
	for _, f := range []string{"dc_boot.bin", "dc_flash.bin"} {
		p := filepath.Join(base, "Bios", "DC", f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := checkBiosResult(dcRom); got != "RESULT bios_ok=1" {
		t.Errorf("DC present: got %q", got)
	}

	// A no-BIOS system (GBA) is never gated.
	gbaRom := filepath.Join(base, "Roms", "Game Boy Advance (GBA)", "Game.gba")
	if got := checkBiosResult(gbaRom); got != "RESULT bios_ok=1" {
		t.Errorf("GBA (no BIOS): got %q", got)
	}
}

// LODOR_ROM_TAG overrides the folder-derived tag.
func TestCheckBiosResult_TagEnvOverride(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("LODOR_ROM_TAG", "DC")
	t.Setenv("LODOR_BIOS_DIRS", "")
	// ROM path carries NO parenthetical tag; the env tag decides.
	if got := checkBiosResult(filepath.Join(base, "whatever", "Game.chd")); got != "RESULT bios_ok=0 missing=dc_boot.bin,dc_flash.bin system=Dreamcast" {
		t.Errorf("env tag override: got %q", got)
	}
}
