//go:build !onion

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func writeBIOS(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A BIOS-requiring system with the file ABSENT reports missing; PRESENT reports ok.
func TestCheckBIOS_DreamcastMissingThenPresent(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)

	ok, missing, sys := CheckBIOS("DC", nil)
	if ok {
		t.Fatalf("expected DC to be gated when no BIOS present; got ok=true")
	}
	if sys != "Dreamcast" {
		t.Errorf("system = %q, want Dreamcast", sys)
	}
	// dc_boot.bin AND dc_flash.bin both missing.
	if len(missing) != 2 || missing[0] != "dc_boot.bin" || missing[1] != "dc_flash.bin" {
		t.Errorf("missing = %v, want [dc_boot.bin dc_flash.bin]", missing)
	}

	// Provide both required files in Bios/DC/ (the minarch system_directory layout).
	writeBIOS(t, filepath.Join(base, "Bios", "DC", "dc_boot.bin"), 2048)
	writeBIOS(t, filepath.Join(base, "Bios", "DC", "dc_flash.bin"), 131072)
	ok, missing, _ = CheckBIOS("DC", nil)
	if !ok {
		t.Errorf("expected DC ok once both BIOS present; missing=%v", missing)
	}
}

// A zero-byte BIOS is treated as ABSENT (a torn/placeholder file must not pass).
func TestCheckBIOS_ZeroByteIsMissing(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	writeBIOS(t, filepath.Join(base, "Bios", "DC", "dc_boot.bin"), 0)
	writeBIOS(t, filepath.Join(base, "Bios", "DC", "dc_flash.bin"), 0)
	if ok, _, _ := CheckBIOS("DC", nil); ok {
		t.Errorf("zero-byte BIOS files should count as missing")
	}
}

// One-of-N region alternatives: any single present file satisfies the slot.
func TestCheckBIOS_RegionAlternative(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	// Only the EU Sega CD BIOS present — still satisfies the single regional slot.
	writeBIOS(t, filepath.Join(base, "Bios", "SEGACD", "bios_CD_E.bin"), 131072)
	if ok, missing, _ := CheckBIOS("SEGACD", nil); !ok {
		t.Errorf("SEGACD should be ok with one regional BIOS present; missing=%v", missing)
	}
}

// A system with NO BIOS requirement is never gated (ok=true, no lookup).
func TestCheckBIOS_NoRequirement(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	for _, tag := range []string{"GBA", "FC", "SFC", "N64", "PSP", "PS", ""} {
		ok, missing, sys := CheckBIOS(tag, nil)
		if !ok || missing != nil || sys != "" {
			t.Errorf("tag %q: expected never-gated (ok,nil,\"\"); got ok=%v missing=%v sys=%q", tag, ok, missing, sys)
		}
	}
}

// extraDirs (e.g. the vendor RA system_directory) are searched alongside Bios/<TAG>/,
// including a per-system subdir (flycast's dc/).
func TestCheckBIOS_ExtraDirAndSubdir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	vendor := t.TempDir() // stands in for /mnt/vendor/deep/retro/system
	// flycast reads dc_boot.bin from <system_directory>/dc/ — exercise the Subdir path.
	writeBIOS(t, filepath.Join(vendor, "dc", "dc_boot.bin"), 2048)
	writeBIOS(t, filepath.Join(vendor, "dc", "dc_flash.bin"), 131072)
	if ok, missing, _ := CheckBIOS("DC", []string{vendor}); !ok {
		t.Errorf("DC should be ok when BIOS lives in extraDir/dc/; missing=%v", missing)
	}
}

// Tag matching is case-insensitive.
func TestCheckBIOS_TagCaseInsensitive(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	if _, ok := BIOSRequirementForTag("dc"); !ok {
		t.Errorf("BIOSRequirementForTag(\"dc\") should resolve case-insensitively")
	}
}
