//go:build onion

package platform

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/romm"
)

// clearEnv unsets every env override the platform layer reads, so a test starts from
// the documented defaults. Restores nothing (tests set what they need explicitly).
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BASE_PATH", "ROMS_DIR", "BIOS_DIR", "SAVES_DIR", "LODOR_SAVE_SUBDIR", "LODOR_SKIP_EMU_GATE"} {
		os.Unsetenv(k)
	}
}

func TestOnionDefaultRoots(t *testing.T) {
	clearEnv(t)
	cases := map[string]string{
		BasePath(): "/mnt/SDCARD",
		RomsDir():  "/mnt/SDCARD/Roms",
		BiosDir():  "/mnt/SDCARD/BIOS",
		SavesDir():  "/mnt/SDCARD/Saves/CurrentProfile/saves",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("root = %q, want %q", got, want)
		}
	}
}

func TestOnionRootEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("BASE_PATH", "/tmp/card")
	if RomsDir() != "/tmp/card/Roms" {
		t.Errorf("RomsDir=%q", RomsDir())
	}
	if BiosDir() != "/tmp/card/BIOS" {
		t.Errorf("BiosDir=%q", BiosDir())
	}
	t.Setenv("SAVES_DIR", "/tmp/saves")
	if SavesDir() != "/tmp/saves" {
		t.Errorf("SavesDir override=%q", SavesDir())
	}
}

func TestOnionPrimaryTag(t *testing.T) {
	clearEnv(t)
	want := map[string]string{
		"gba": "GBA", "snes": "SFC", "sfam": "SFC", "nes": "FC",
		"genesis": "MD", "sms": "MS", "mastersystem": "MS",
		"sega32": "THIRTYTWOX", "psx": "PS", "atari2600": "ATARI", "lynx": "LYNX",
	}
	for slug, tag := range want {
		got, ok := PrimaryTag(slug)
		if !ok || got != tag {
			t.Errorf("PrimaryTag(%q)=%q,%v want %q,true", slug, got, ok, tag)
		}
	}
	if _, ok := PrimaryTag("totally-unknown-slug"); ok {
		t.Error("unknown slug should not map")
	}
}

func TestOnionMirrorFolderNameIsBareTag(t *testing.T) {
	for _, mode := range []string{config.MirrorModeOwn, config.MirrorModeSeparate, "merge"} {
		if got := MirrorFolderName("Game Boy Advance", "GBA", mode); got != "GBA" {
			t.Errorf("MirrorFolderName(mode=%s)=%q want bare GBA", mode, got)
		}
	}
}

func TestOnionSaveFileNameRetroArchRule(t *testing.T) {
	if got := SaveFileName("Game (USA).gba", "srm"); got != "Game (USA).srm" {
		t.Errorf("SaveFileName=%q want 'Game (USA).srm'", got)
	}
	if got := SaveFileName("Game (USA).gba", ".srm"); got != "Game (USA).srm" {
		t.Errorf("dotted ext SaveFileName=%q", got)
	}
	if got := SaveFileName("Foo.bin", ""); got != "Foo" {
		t.Errorf("empty ext SaveFileName=%q want 'Foo'", got)
	}
}

func TestOnionSaveDirectoryPinAndFallback(t *testing.T) {
	clearEnv(t)
	t.Setenv("BASE_PATH", "/c")
	// pinned by the launch shim
	t.Setenv("LODOR_SAVE_SUBDIR", "mGBA")
	if got := SaveDirectory("gba"); got != "/c/Saves/CurrentProfile/saves/mGBA" {
		t.Errorf("pinned SaveDirectory=%q", got)
	}
	// daemon fallback to default core
	os.Unsetenv("LODOR_SAVE_SUBDIR")
	if got := SaveDirectory("snes"); got != "/c/Saves/CurrentProfile/saves/Snes9x" {
		t.Errorf("fallback SaveDirectory=%q", got)
	}
	// unknown slug, no env -> no save dir (never blind-write)
	if got := SaveDirectory("totally-unknown"); got != "" {
		t.Errorf("unknown SaveDirectory=%q want empty", got)
	}
}

func TestOnionEmulatorFoldersScan(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	saves := filepath.Join(base, "Saves", "CurrentProfile", "saves")
	for _, core := range []string{"mGBA", "Snes9x", ".hidden"} {
		os.MkdirAll(filepath.Join(saves, core), 0o755)
	}
	// pinned: only that folder
	t.Setenv("LODOR_SAVE_SUBDIR", "mGBA")
	if got := EmulatorFoldersForFSSlug("gba"); len(got) != 1 || got[0] != "mGBA" {
		t.Errorf("pinned scan=%v want [mGBA]", got)
	}
	// daemon: glob all real core folders, skip dotfiles
	os.Unsetenv("LODOR_SAVE_SUBDIR")
	got := EmulatorFoldersForFSSlug("gba")
	found := map[string]bool{}
	for _, d := range got {
		found[d] = true
	}
	if !found["mGBA"] || !found["Snes9x"] || found[".hidden"] {
		t.Errorf("glob scan=%v want mGBA+Snes9x, no dotfiles", got)
	}
}

func TestOnionBIOSFlat(t *testing.T) {
	clearEnv(t)
	t.Setenv("BASE_PATH", "/c")
	got := BIOSFilePaths("gba_bios.bin", "gba")
	if len(got) != 1 || got[0] != "/c/BIOS/gba_bios.bin" {
		t.Errorf("BIOSFilePaths=%v want [/c/BIOS/gba_bios.bin]", got)
	}
}

func TestOnionHasEmuPakGate(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	os.MkdirAll(filepath.Join(base, "Emu", "GBA"), 0o755)
	if !HasEmuPak("GBA") {
		t.Error("GBA Emu dir present -> HasEmuPak should be true")
	}
	if HasEmuPak("PSP") {
		t.Error("no PSP Emu dir -> HasEmuPak should be false")
	}
	// PlayStation: Roms tag is PS but the Emu folder is PSX (ground truth) — the gate must
	// follow onionEmuFolder, not the bare tag.
	os.MkdirAll(filepath.Join(base, "Emu", "PSX"), 0o755)
	if !HasEmuPak("PS") {
		t.Error("Emu/PSX present -> HasEmuPak(PS) should be true via onionEmuFolder")
	}
	t.Setenv("LODOR_SKIP_EMU_GATE", "1")
	if !HasEmuPak("PSP") {
		t.Error("skip-gate env -> HasEmuPak should be true")
	}
}

func TestOnionLocalRomPathBareTagFolder(t *testing.T) {
	clearEnv(t)
	t.Setenv("BASE_PATH", "/c")
	cfg := &config.Config{
		DirectoryMappings: map[string]config.DirMapping{
			"gba": {Slug: "gba", RelativePath: "GBA"},
		},
	}
	rom := romm.Rom{
		PlatformFsSlug: "gba",
		Files:          []romm.RomFile{{FileName: "Zelda (USA).gba"}},
	}
	if got := LocalRomPath(cfg, rom); got != "/c/Roms/GBA/Zelda (USA).gba" {
		t.Errorf("LocalRomPath=%q want /c/Roms/GBA/Zelda (USA).gba", got)
	}
}

// TestOnionFsSlugForTag locks the deterministic reverse of onionRomTags that catalog.go
// (shared with !onion) depends on. The key property: every slug returned must round-trip
// back to the SAME tag via PrimaryTag, and the result must be stable for aliased tags.
func TestOnionFsSlugForTag(t *testing.T) {
	clearEnv(t)
	// Unique-tag slugs reverse exactly.
	exact := map[string]string{"GBA": "gba", "MD": "genesis", "PS": "psx", "N64": "n64"}
	for tag, wantSlug := range exact {
		got, ok := FsSlugForTag(tag)
		if !ok || got != wantSlug {
			t.Errorf("FsSlugForTag(%q)=%q,%v want %q,true", tag, got, ok, wantSlug)
		}
	}
	// Aliased tags (several slugs -> one tag) must still round-trip and be deterministic.
	for _, tag := range []string{"SFC", "FC", "MS", "PCE", "NEOGEO"} {
		got, ok := FsSlugForTag(tag)
		if !ok {
			t.Fatalf("FsSlugForTag(%q) returned ok=false", tag)
		}
		if back, ok2 := PrimaryTag(got); !ok2 || back != tag {
			t.Errorf("round-trip FsSlugForTag(%q)=%q then PrimaryTag=%q,%v want %q", tag, got, back, ok2, tag)
		}
		if got2, _ := FsSlugForTag(tag); got2 != got {
			t.Errorf("FsSlugForTag(%q) not deterministic: %q vs %q", tag, got, got2)
		}
	}
	if _, ok := FsSlugForTag(""); ok {
		t.Error("empty tag should not map")
	}
	if _, ok := FsSlugForTag("NOPE"); ok {
		t.Error("unknown tag should not map")
	}
}
