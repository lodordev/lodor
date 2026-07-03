//go:build muos

package platform

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/romm"
)

func TestMuosSaveFileName(t *testing.T) {
	cases := []struct{ rom, ext, want string }{
		{"Game (USA).gba", "srm", "Game (USA).srm"},
		{"Sonic.gg", "srm", "Sonic.srm"},
		{"Game (USA).gba", ".srm", "Game (USA).srm"}, // leading dot tolerated
		{"NoExt", "srm", "NoExt.srm"},
		{"Game.gba", "", "Game"}, // empty ext -> stripped basename
	}
	for _, c := range cases {
		if got := SaveFileName(c.rom, c.ext); got != c.want {
			t.Errorf("SaveFileName(%q,%q)=%q want %q", c.rom, c.ext, got, c.want)
		}
	}
}

func TestMuosSaveDirectoryEnvPinned(t *testing.T) {
	t.Setenv("SAVES_DIR", "/tmp/saves")
	t.Setenv(saveSubdirEnv, "PCSX-ReARMed")
	want := filepath.Join("/tmp/saves", "PCSX-ReARMed")
	if got := SaveDirectory("psx"); got != want {
		t.Errorf("SaveDirectory pinned = %q want %q", got, want)
	}
}

func TestMuosSaveDirectoryDefaultCoreFallback(t *testing.T) {
	t.Setenv("SAVES_DIR", "/tmp/saves")
	os.Unsetenv(saveSubdirEnv)
	if got := SaveDirectory("psx"); got != filepath.Join("/tmp/saves", "PCSX-ReARMed") {
		t.Errorf("psx default-core dir = %q", got)
	}
	if got := SaveDirectory("totally-unknown-slug"); got != "" {
		t.Errorf("unknown slug must yield no save dir (no blind write), got %q", got)
	}
}

func TestMuosEmulatorFoldersEnvVsGlob(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAVES_DIR", dir)
	// glob mode (no env): returns existing subdirs
	os.Unsetenv(saveSubdirEnv)
	for _, d := range []string{"Snes9x", "mGBA", ".hidden"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := EmulatorFoldersForFSSlug("snes")
	if len(got) != 2 { // .hidden skipped
		t.Errorf("glob folders = %v want 2 (hidden skipped)", got)
	}
	// pinned mode: exactly the env folder
	t.Setenv(saveSubdirEnv, "Snes9x")
	if g := EmulatorFoldersForFSSlug("snes"); len(g) != 1 || g[0] != "Snes9x" {
		t.Errorf("pinned folders = %v want [Snes9x]", g)
	}
}

// TestMuosPrimaryTag: the muOS "tag" is the catalogue folder name from the card's own
// info/assign directories, including the RomM-instance slug aliases.
func TestMuosPrimaryTag(t *testing.T) {
	cases := []struct {
		slug, want string
		ok         bool
	}{
		{"psx", "Sony PlayStation", true},
		{"snes", "Nintendo SNES - SFC", true},
		{"mastersystem", "Sega Master System", true}, // alias for this RomM instance
		{"atarilynx", "Atari Lynx", true},
		{"pcengine", "NEC PC Engine", true},
		{"nonexistent", "", false},
	}
	for _, c := range cases {
		got, ok := PrimaryTag(c.slug)
		if got != c.want || ok != c.ok {
			t.Errorf("PrimaryTag(%q)=(%q,%v) want (%q,%v)", c.slug, got, ok, c.want, c.ok)
		}
	}
}

// TestMuosFsSlugForTag: aliased catalogue names resolve DETERMINISTICALLY to the
// lexicographically-first slug (famicom < nes, sfam < snes).
func TestMuosFsSlugForTag(t *testing.T) {
	if s, ok := FsSlugForTag("Nintendo NES - Famicom"); !ok || s != "famicom" {
		t.Errorf("FsSlugForTag(NES) = (%q,%v) want (famicom,true)", s, ok)
	}
	if s, ok := FsSlugForTag("Sony PlayStation"); !ok || s != "psx" {
		t.Errorf("FsSlugForTag(PlayStation) = (%q,%v) want (psx,true)", s, ok)
	}
	if _, ok := FsSlugForTag("No Such System"); ok {
		t.Error("unknown catalogue name must not resolve")
	}
	if _, ok := FsSlugForTag(""); ok {
		t.Error("empty tag must not resolve")
	}
}

// TestMuosMirrorFolderName: the mirror folder is the bare catalogue name in EVERY
// mirror mode — muOS binds ROMS/<System> to an emulator purely by that name; the
// separate-mode split lives in LocalBasename's filename disambiguator instead.
func TestMuosMirrorFolderName(t *testing.T) {
	for _, mode := range []string{config.MirrorModeOwn, config.MirrorModeSeparate, config.MirrorModeMerge} {
		if got := MirrorFolderName("Sony PlayStation Display", "Sony PlayStation", mode); got != "Sony PlayStation" {
			t.Errorf("MirrorFolderName(mode=%s) = %q want bare catalogue name", mode, got)
		}
	}
}

// TestMuosHasEmuPak: launchable == the assign config dir for the catalogue name
// exists; LODOR_SKIP_EMU_GATE=1 bypasses for the pre-install sandbox.
func TestMuosHasEmuPak(t *testing.T) {
	assign := t.TempDir()
	t.Setenv("MUOS_ASSIGN_DIR", assign)
	os.Unsetenv("LODOR_SKIP_EMU_GATE")
	if err := os.MkdirAll(filepath.Join(assign, "Sony PlayStation"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !HasEmuPak("Sony PlayStation") {
		t.Error("HasEmuPak = false with the assign dir present")
	}
	if HasEmuPak("Nintendo N64") {
		t.Error("HasEmuPak = true with NO assign dir (would stub unlaunchable games)")
	}
	if HasEmuPak("") {
		t.Error("HasEmuPak(\"\") must be false")
	}
	t.Setenv("LODOR_SKIP_EMU_GATE", "1")
	if !HasEmuPak("Nintendo N64") {
		t.Error("LODOR_SKIP_EMU_GATE=1 must bypass the gate")
	}
}

// TestMuosLocalBasename: current default semantics — canonical in own AND merge
// (merge-canonical, C1 §2), " (RomM)"-disambiguated only in separate.
func TestMuosLocalBasename(t *testing.T) {
	rom := romm.Rom{
		PlatformFsSlug: "gba",
		FsName:         "Game (USA).gba",
		FsNameNoExt:    "Game (USA)",
		Files:          []romm.RomFile{{FileName: "Game (USA).gba"}},
	}
	for mode, want := range map[string]string{
		config.MirrorModeOwn:      "Game (USA)",
		config.MirrorModeMerge:    "Game (USA)",
		config.MirrorModeSeparate: "Game (USA) (RomM)",
	} {
		cfg := &config.Config{MirrorMode: mode}
		if got := LocalBasename(cfg, rom); got != want {
			t.Errorf("LocalBasename(mode=%s) = %q want %q", mode, got, want)
		}
	}
}

// TestMuosSaveArtifactAnchors: RetroArch derives artifacts from the stem, so the
// anchor set is {stem, full} — and a name with no extension degrades to itself.
func TestMuosSaveArtifactAnchors(t *testing.T) {
	got := saveArtifactAnchors("✘ Game (USA).gba")
	if len(got) != 2 || got[0] != "✘ Game (USA)" || got[1] != "✘ Game (USA).gba" {
		t.Errorf("anchors = %v want [✘ Game (USA), ✘ Game (USA).gba]", got)
	}
	if got := saveArtifactAnchors("NoExt"); len(got) != 1 || got[0] != "NoExt" {
		t.Errorf("extension-less anchors = %v want [NoExt]", got)
	}
}

// TestMuosMarkerFlipCarriesRetroArchSave: the composition check — a ✘→✓ reconcile
// on a downloaded ROM must carry the RetroArch-named save ("<stem>.srm") to the new
// marker name, not orphan it (the full-filename anchor alone would miss it).
func TestMuosMarkerFlipCarriesRetroArchSave(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv(saveSubdirEnv, "mGBA")
	os.Unsetenv("LODOR_HOST_OS")

	rom := romm.Rom{
		ID: 7, PlatformFsSlug: "gba",
		FsName: "Game (USA).gba", FsNameNoExt: "Game (USA)",
		Files: []romm.RomFile{{FileName: "Game (USA).gba"}},
	}
	cfg := &config.Config{
		MirrorMode:        config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{"gba": {Slug: "gba", RelativePath: "Nintendo Game Boy Advance"}},
	}
	dir := filepath.Join(RomsDir(), "Nintendo Game Boy Advance")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A downloaded (real-bytes) game still wearing the CLOUD marker, with its
	// RetroArch save + state beside it under the marked stem.
	marked := filepath.Join(dir, MarkerCloud+"Game (USA).gba")
	if err := os.WriteFile(marked, []byte("REAL ROM BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveDir := filepath.Join(SavesDir(), "mGBA")
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{MarkerCloud + "Game (USA).srm", MarkerCloud + "Game (USA).state1"} {
		if err := os.WriteFile(filepath.Join(saveDir, f), []byte("SAVE"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	unmarked := filepath.Join(dir, "Game (USA).gba")
	final, created := ReconcileMarkedPresence(cfg, rom, unmarked)
	if created || final != filepath.Join(dir, MarkerOnDevice+"Game (USA).gba") {
		t.Fatalf("reconcile = (%q,%v) want on-device marker, no create", final, created)
	}
	for _, f := range []string{MarkerOnDevice + "Game (USA).srm", MarkerOnDevice + "Game (USA).state1"} {
		if _, err := os.Stat(filepath.Join(saveDir, f)); err != nil {
			t.Errorf("save artifact %q not carried across the marker flip: %v", f, err)
		}
	}
	for _, f := range []string{MarkerCloud + "Game (USA).srm", MarkerCloud + "Game (USA).state1"} {
		if _, err := os.Stat(filepath.Join(saveDir, f)); !os.IsNotExist(err) {
			t.Errorf("stale save artifact %q left under the old marker name", f)
		}
	}
}

func TestMuosDirsEnvOverride(t *testing.T) {
	t.Setenv("ROMS_DIR", "/x/roms")
	t.Setenv("SAVES_DIR", "/x/save")
	t.Setenv("BIOS_DIR", "/x/bios")
	if RomsDir() != "/x/roms" || SavesDir() != "/x/save" || BiosDir() != "/x/bios" {
		t.Errorf("env overrides not honored: %s %s %s", RomsDir(), SavesDir(), BiosDir())
	}
	if got := BIOSFilePaths("scph5501.bin", "psx"); len(got) != 1 || got[0] != "/x/bios/scph5501.bin" {
		t.Errorf("muOS BIOS path = %v want flat /x/bios/scph5501.bin", got)
	}
}

// TestMuosBasePathRelocation: BASE_PATH set (sandbox/tests) relocates the whole tree
// to the generic <BASE_PATH>/{Roms,Saves,Bios} layout; unset (device) the muOS-true
// absolute defaults apply.
func TestMuosBasePathRelocation(t *testing.T) {
	for _, k := range []string{"ROMS_DIR", "SAVES_DIR", "BIOS_DIR"} {
		os.Unsetenv(k)
	}
	t.Setenv("BASE_PATH", "/x/base")
	if RomsDir() != "/x/base/Roms" || SavesDir() != "/x/base/Saves" || BiosDir() != "/x/base/Bios" {
		t.Errorf("relocated dirs wrong: %s %s %s", RomsDir(), SavesDir(), BiosDir())
	}
	os.Unsetenv("BASE_PATH")
	if RomsDir() != "/mnt/mmc/ROMS" || SavesDir() != "/run/muos/storage/save/file" || BiosDir() != "/run/muos/storage/bios" {
		t.Errorf("device defaults wrong: %s %s %s", RomsDir(), SavesDir(), BiosDir())
	}
}
