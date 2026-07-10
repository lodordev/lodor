package platform

import (
	"path/filepath"
	"testing"

	"lodor/romm"
)

func TestSafePathComponent(t *testing.T) {
	safe := []string{
		"Game.gba",
		"Final Fantasy VII (USA) (Disc 1).chd",
		"Pokémon Red.gb",           // unicode
		"Rock'n'Roll Racing.smc",   // apostrophe
		"Legend of Zelda [!].nes",  // brackets/bang
		"1943 - The Battle.zip",    // dashes, digits
		"a+b.bin",                  // plus
		"game.chd",
	}
	for _, s := range safe {
		if !safePathComponent(s) {
			t.Errorf("safePathComponent(%q) = false, want true (legit filename must be allowed)", s)
		}
	}
	unsafe := []string{
		"",
		".",
		"..",
		"../evil",
		"../../../../.system/miyoomini/bin/lodor-sync",
		"foo/bar",
		"foo\\bar",
		"/etc/passwd",
		"a/../b",
		"sub/dir/file.gba",
	}
	for _, s := range unsafe {
		if safePathComponent(s) {
			t.Errorf("safePathComponent(%q) = true, want false (traversal/separator must be rejected)", s)
		}
	}
}

func TestContainedUnder(t *testing.T) {
	base := "/mnt/SDCARD/Roms"
	in := []string{
		"/mnt/SDCARD/Roms/GBA/Game.gba",
		"/mnt/SDCARD/Roms/Game Boy Advance (GBA)/x.gba",
		"/mnt/SDCARD/Roms", // base itself
	}
	for _, p := range in {
		if !containedUnder(base, p) {
			t.Errorf("containedUnder(%q,%q) = false, want true", base, p)
		}
	}
	out := []string{
		"/mnt/SDCARD/Roms/../.system/bin/lodor-sync",
		"/mnt/SDCARD/.system/bin/lodor-sync",
		"/etc/passwd",
		"/mnt/SDCARD/RomsEvil/x", // sibling prefix, not contained
	}
	for _, p := range out {
		if containedUnder(base, p) {
			t.Errorf("containedUnder(%q,%q) = true, want false (escape)", base, p)
		}
	}
}

func TestIsSafeRelFolder(t *testing.T) {
	// RomsDir() reads BASE_PATH; pin it for a deterministic base.
	t.Setenv("BASE_PATH", "/mnt/SDCARD")
	good := []string{
		"Game Boy Advance (GBA)",
		"Sony PlayStation",
		"nested/legit/path",
		"GBA",
	}
	for _, f := range good {
		if !isSafeRelFolder(f) {
			t.Errorf("isSafeRelFolder(%q) = false, want true (legit folder)", f)
		}
	}
	bad := []string{
		"",
		"..",
		"../../../../data/local/tmp",
		"a/../../b",
		"/abs/path",
		"foo/../../bar",
		"..\\windows",
	}
	for _, f := range bad {
		if isSafeRelFolder(f) {
			t.Errorf("isSafeRelFolder(%q) = true, want false (escape/absolute)", f)
		}
	}
}

func TestPathWithinRoms(t *testing.T) {
	t.Setenv("BASE_PATH", "/mnt/SDCARD")
	roms := RomsDir()
	if !PathWithinRoms(filepath.Join(roms, "GBA", "Game.gba")) {
		t.Error("PathWithinRoms rejected a legit ROM path")
	}
	if PathWithinRoms(roms) {
		t.Error("PathWithinRoms accepted RomsDir itself as a file dest")
	}
	if PathWithinRoms(filepath.Join(roms, "..", ".system", "bin", "lodor-sync")) {
		t.Error("PathWithinRoms accepted an escaping dest")
	}
}

func TestValidateRomNames(t *testing.T) {
	// A fully hostile rom: traversal in file_name AND fs_name_no_ext -> REJECTED.
	evil := romm.Rom{
		PlatformFsSlug: "gba",
		FsNameNoExt:    "../../x",
		Files: []romm.RomFile{
			{ID: 1, FileName: "../../../evil"},
		},
	}
	if ValidateRomNames(evil) {
		t.Error("ValidateRomNames accepted a rom with traversal names")
	}

	// Traversal only in the slug -> REJECTED.
	evilSlug := romm.Rom{
		PlatformFsSlug: "../../../../.system/miyoomini/bin",
		Files:          []romm.RomFile{{ID: 1, FileName: "Game.gba"}},
	}
	if ValidateRomNames(evilSlug) {
		t.Error("ValidateRomNames accepted a rom with a traversal slug")
	}

	// A legit single-component rom still passes.
	good := romm.Rom{
		PlatformFsSlug: "gba",
		FsNameNoExt:    "Final Fantasy VI Advance (USA)",
		Files: []romm.RomFile{
			{ID: 1, FileName: "Final Fantasy VI Advance (USA).gba"},
		},
	}
	if !ValidateRomNames(good) {
		t.Error("ValidateRomNames rejected a legit rom")
	}

	// A legit multi-disc rom (per-disc file names) still passes.
	multi := romm.Rom{
		PlatformFsSlug:   "psx",
		FsNameNoExt:      "Final Fantasy VII (USA)",
		HasMultipleFiles: true,
		Files: []romm.RomFile{
			{ID: 1, FileName: "Final Fantasy VII (USA) (Disc 1).chd"},
			{ID: 2, FileName: "Final Fantasy VII (USA) (Disc 2).chd"},
		},
	}
	if !ValidateRomNames(multi) {
		t.Error("ValidateRomNames rejected a legit multi-disc rom")
	}
}

// TestBIOSFilePaths_NoTagFallbackBasesName guards FIX 4: the no-tag BIOS fallback
// must strip any directory component from the server-supplied name.
func TestBIOSFilePaths_NoTagFallbackBasesName(t *testing.T) {
	t.Setenv("BASE_PATH", "/mnt/SDCARD")
	// "3do" maps to an empty tag slice -> no-tag fallback branch.
	got := BIOSFilePaths("../../../evil.bin", "3do")
	if len(got) != 1 {
		t.Fatalf("BIOSFilePaths returned %d paths, want 1", len(got))
	}
	want := filepath.Join(BiosDir(), "evil.bin")
	if got[0] != want {
		t.Errorf("BIOSFilePaths no-tag fallback = %q, want %q (must base the name)", got[0], want)
	}
}
