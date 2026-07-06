//go:build knulli

package platform

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/romm"
)

// TestKnulliRomFolderExceptions pins every slug→folder EXCEPTION against the
// Batocera es_systems names (the well-known divergences from RomM's slugs), plus
// a spread of identity entries and the RomM alias slugs. A wrong folder here
// means stubs land in a directory EmulationStation will never show.
func TestKnulliRomFolderExceptions(t *testing.T) {
	cases := []struct {
		slug, want string
		ok         bool
	}{
		// The headline exception: Batocera says megadrive, not genesis.
		{"genesis", "megadrive", true},
		// Sega short-slug / alias exceptions.
		{"sms", "mastersystem", true},
		{"mastersystem", "mastersystem", true},
		{"sega32", "sega32x", true},
		{"sega32x", "sega32x", true},
		{"dc", "dreamcast", true},
		{"dreamcast", "dreamcast", true},
		// NEC.
		{"tg16", "pcengine", true},
		{"pcengine", "pcengine", true},
		{"turbografx-cd", "pcenginecd", true},
		// Atari.
		{"lynx", "lynx", true},
		{"atarilynx", "lynx", true},
		{"jaguar", "jaguar", true},
		{"atarijaguar", "jaguar", true},
		// SNK.
		{"neogeoaes", "neogeo", true},
		{"neogeomvs", "neogeo", true},
		{"neo-geo-cd", "neogeocd", true},
		{"neo-geo-pocket", "ngp", true},
		{"neo-geo-pocket-color", "ngpc", true},
		// Bandai.
		{"wonderswan", "wswan", true},
		{"wonderswan-color", "wswanc", true},
		{"wonderswancolor", "wswanc", true},
		// Arcade: no generic "arcade" folder on Batocera — FBNeo is the home.
		{"arcade", "fbneo", true},
		{"fbneo", "fbneo", true},
		// Hyphen exceptions.
		{"pokemon-mini", "pokemini", true},
		{"pico-8", "pico8", true},
		// Identity spot checks.
		{"gba", "gba", true},
		{"snes", "snes", true},
		{"sfam", "snes", true},
		{"famicom", "nes", true},
		{"psx", "psx", true},
		{"psp", "psp", true},
		{"nds", "nds", true},
		{"saturn", "saturn", true},
		{"gamegear", "gamegear", true},
		{"segacd", "segacd", true},
		// Unknown slug: SKIPPED, never invented.
		{"nonexistent", "", false},
		{"msx", "", false}, // deliberately unmapped (msx1/msx2 folder split unverified)
	}
	for _, c := range cases {
		got, ok := PrimaryTag(c.slug)
		if got != c.want || ok != c.ok {
			t.Errorf("PrimaryTag(%q)=(%q,%v) want (%q,%v)", c.slug, got, ok, c.want, c.ok)
		}
	}
}

// TestKnulliFsSlugForTag: aliased folder slugs resolve DETERMINISTICALLY to the
// lexicographically-first RomM slug (arcade < fbneo, famicom < nes).
func TestKnulliFsSlugForTag(t *testing.T) {
	cases := []struct {
		tag, want string
		ok        bool
	}{
		{"megadrive", "genesis", true},
		{"nes", "famicom", true},
		{"fbneo", "arcade", true},
		{"lynx", "atarilynx", true},
		{"gba", "gba", true},
		{"No Such System", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := FsSlugForTag(c.tag)
		if got != c.want || ok != c.ok {
			t.Errorf("FsSlugForTag(%q)=(%q,%v) want (%q,%v)", c.tag, got, ok, c.want, c.ok)
		}
	}
}

func TestKnulliSaveFileName(t *testing.T) {
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

// TestKnulliSaveDirectory: the save dir is the deterministic PER-SYSTEM Batocera
// slug (sort-by-core is disabled upstream) — including the megadrive exception —
// with a shim-pinned LODOR_SAVE_SUBDIR still winning, and NO dir for an unknown
// slug (never blind-write).
func TestKnulliSaveDirectory(t *testing.T) {
	t.Setenv("SAVES_DIR", "/tmp/saves")
	os.Unsetenv(saveSubdirEnv)
	if got := SaveDirectory("gba"); got != filepath.Join("/tmp/saves", "gba") {
		t.Errorf("gba save dir = %q", got)
	}
	if got := SaveDirectory("genesis"); got != filepath.Join("/tmp/saves", "megadrive") {
		t.Errorf("genesis save dir = %q want the megadrive folder", got)
	}
	if got := SaveDirectory("totally-unknown-slug"); got != "" {
		t.Errorf("unknown slug must yield no save dir (no blind write), got %q", got)
	}
	t.Setenv(saveSubdirEnv, "Pinned")
	if got := SaveDirectory("gba"); got != filepath.Join("/tmp/saves", "Pinned") {
		t.Errorf("pinned save dir = %q", got)
	}
}

// TestKnulliEmulatorFolders: the scan set is the one per-system folder (env pin
// wins; unknown slug scans nothing).
func TestKnulliEmulatorFolders(t *testing.T) {
	os.Unsetenv(saveSubdirEnv)
	if got := EmulatorFoldersForFSSlug("genesis"); len(got) != 1 || got[0] != "megadrive" {
		t.Errorf("genesis scan folders = %v want [megadrive]", got)
	}
	if got := EmulatorFoldersForFSSlug("unknown"); got != nil {
		t.Errorf("unknown slug scan folders = %v want nil", got)
	}
	t.Setenv(saveSubdirEnv, "Pinned")
	if got := EmulatorFoldersForFSSlug("genesis"); len(got) != 1 || got[0] != "Pinned" {
		t.Errorf("pinned scan folders = %v want [Pinned]", got)
	}
}

// TestKnulliMirrorFolderName: the mirror folder is the bare Batocera slug in
// EVERY mirror mode — ES binds roms/<slug> to a system purely by that name; the
// separate-mode split lives in LocalBasename's filename disambiguator instead.
func TestKnulliMirrorFolderName(t *testing.T) {
	for _, mode := range []string{config.MirrorModeOwn, config.MirrorModeSeparate, config.MirrorModeMerge} {
		if got := MirrorFolderName("Sega Mega Drive Display", "megadrive", mode); got != "megadrive" {
			t.Errorf("MirrorFolderName(mode=%s) = %q want bare batocera slug", mode, got)
		}
	}
}

// TestKnulliCanonicalMirrorFolder: a drifted mapping heals to the Batocera slug;
// an unmapped slug is left alone.
func TestKnulliCanonicalMirrorFolder(t *testing.T) {
	if got := CanonicalMirrorFolder("genesis"); got != "megadrive" {
		t.Errorf("CanonicalMirrorFolder(genesis) = %q want megadrive", got)
	}
	if got := CanonicalMirrorFolder("unknown"); got != "" {
		t.Errorf("CanonicalMirrorFolder(unknown) = %q want \"\"", got)
	}
}

// TestKnulliHasEmuPak: launchable == the roms/<slug> folder exists (Knulli
// pre-creates one per supported system); LODOR_SKIP_EMU_GATE=1 bypasses.
func TestKnulliHasEmuPak(t *testing.T) {
	roms := t.TempDir()
	t.Setenv("ROMS_DIR", roms)
	os.Unsetenv("LODOR_SKIP_EMU_GATE")
	if err := os.MkdirAll(filepath.Join(roms, "megadrive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !HasEmuPak("megadrive") {
		t.Error("HasEmuPak = false with the roms folder present")
	}
	if HasEmuPak("n64") {
		t.Error("HasEmuPak = true with NO roms folder (would stub unlaunchable games)")
	}
	if HasEmuPak("") {
		t.Error("HasEmuPak(\"\") must be false")
	}
	t.Setenv("LODOR_SKIP_EMU_GATE", "1")
	if !HasEmuPak("n64") {
		t.Error("LODOR_SKIP_EMU_GATE=1 must bypass the gate")
	}
}

// TestKnulliLocalBasename: default/muos semantics — canonical in own AND merge
// (merge-canonical, C1 §2), " (RomM)"-disambiguated only in separate.
func TestKnulliLocalBasename(t *testing.T) {
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

// TestKnulliSaveArtifactAnchors: RetroArch derives artifacts from the stem, so
// the anchor set is {stem, full} — a marker flip must carry "<stem>.srm" saves.
func TestKnulliSaveArtifactAnchors(t *testing.T) {
	got := saveArtifactAnchors("✘ Game (USA).gba")
	if len(got) != 2 || got[0] != "✘ Game (USA)" || got[1] != "✘ Game (USA).gba" {
		t.Errorf("anchors = %v want [✘ Game (USA), ✘ Game (USA).gba]", got)
	}
	if got := saveArtifactAnchors("NoExt"); len(got) != 1 || got[0] != "NoExt" {
		t.Errorf("extension-less anchors = %v want [NoExt]", got)
	}
}

func TestKnulliDirsEnvOverride(t *testing.T) {
	t.Setenv("ROMS_DIR", "/x/roms")
	t.Setenv("SAVES_DIR", "/x/save")
	t.Setenv("BIOS_DIR", "/x/bios")
	if RomsDir() != "/x/roms" || SavesDir() != "/x/save" || BiosDir() != "/x/bios" {
		t.Errorf("env overrides not honored: %s %s %s", RomsDir(), SavesDir(), BiosDir())
	}
	if got := BIOSFilePaths("scph5501.bin", "psx"); len(got) != 1 || got[0] != "/x/bios/scph5501.bin" {
		t.Errorf("knulli BIOS path = %v want flat /x/bios/scph5501.bin", got)
	}
}

// TestKnulliBasePathRelocation: BASE_PATH set (sandbox/tests) relocates the whole
// tree to the generic <BASE_PATH>/{Roms,Saves,Bios} layout; unset (device) the
// Batocera-true /userdata defaults apply.
func TestKnulliBasePathRelocation(t *testing.T) {
	for _, k := range []string{"ROMS_DIR", "SAVES_DIR", "BIOS_DIR"} {
		os.Unsetenv(k)
	}
	t.Setenv("BASE_PATH", "/x/base")
	if RomsDir() != "/x/base/Roms" || SavesDir() != "/x/base/Saves" || BiosDir() != "/x/base/Bios" {
		t.Errorf("relocated dirs wrong: %s %s %s", RomsDir(), SavesDir(), BiosDir())
	}
	os.Unsetenv("BASE_PATH")
	if RomsDir() != "/userdata/roms" || SavesDir() != "/userdata/saves" || BiosDir() != "/userdata/bios" {
		t.Errorf("device defaults wrong: %s %s %s", RomsDir(), SavesDir(), BiosDir())
	}
	if BasePath() != "/userdata" {
		t.Errorf("BasePath() = %q want /userdata", BasePath())
	}
}

// TestKnulliMarkerFlipCarriesRetroArchSave: the composition check — a ✘→✓
// reconcile on a downloaded ROM must carry the RetroArch-named save
// ("<stem>.srm") in the PER-SYSTEM save folder to the new marker name, not
// orphan it.
func TestKnulliMarkerFlipCarriesRetroArchSave(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	os.Unsetenv(saveSubdirEnv)
	os.Unsetenv("LODOR_HOST_OS")

	rom := romm.Rom{
		ID: 7, PlatformFsSlug: "gba",
		FsName: "Game (USA).gba", FsNameNoExt: "Game (USA)",
		Files: []romm.RomFile{{FileName: "Game (USA).gba"}},
	}
	cfg := &config.Config{
		MirrorMode:        config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{"gba": {Slug: "gba", RelativePath: "gba"}},
	}
	dir := filepath.Join(RomsDir(), "gba")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A downloaded (real-bytes) game still wearing the CLOUD marker, with its
	// RetroArch save + state beside it in the per-system folder.
	marked := filepath.Join(dir, MarkerCloud+"Game (USA).gba")
	if err := os.WriteFile(marked, []byte("REAL ROM BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveDir := filepath.Join(SavesDir(), "gba")
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
