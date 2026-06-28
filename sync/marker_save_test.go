package sync

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// markerSaveEnv roots a temp SDCARD, maps the GBA folder to slug "gba", and returns the
// cfg, the ROM, and the ACTUAL on-disk ROM path the launcher would open under the active
// mirror mode (LocalRomPath) — the same name minarch derives its "<rom>.sav" save from.
func markerSaveEnv(t *testing.T, mode string) (*config.Config, romm.Rom, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	cfg := &config.Config{
		MirrorMode:        mode,
		DirectoryMappings: map[string]config.DirMapping{"gba": {RelativePath: "GBA"}},
	}
	rom := romm.Rom{
		ID:             1234,
		PlatformFsSlug: "gba",
		FsName:         "Sonic (USA).gba",
		FsNameNoExt:    "Sonic (USA)",
		Files:          []romm.RomFile{{FileName: "Sonic (USA).gba"}},
	}
	return cfg, rom, platform.LocalRomPath(cfg, rom)
}

// TestSaveLocalPathMirrorsMarkedRomName: a pull must write the save under the SAME marked
// on-disk name the emulator reads (derived from the launched romPath), in BOTH own and
// separate (NextUI default) modes. Reconstructing from the unmarked canonical name would
// strand the pulled save where the emulator never looks.
func TestSaveLocalPathMirrorsMarkedRomName(t *testing.T) {
	for _, mode := range []string{config.MirrorModeOwn, config.MirrorModeSeparate} {
		cfg, rom, canonical := markerSaveEnv(t, mode)
		marked := filepath.Join(filepath.Dir(canonical), platform.MarkerOnDevice+filepath.Base(canonical))
		got := saveLocalPath(cfg, rom, marked, "")
		// Derive the expected save name through the platform helper so this asserts the
		// real property on BOTH CFWs: MinUI yields "<name>.sav" (unchanged); OnionOS yields
		// the RetroArch basename rule. saveLocalPath builds the name the same way.
		want := filepath.Join(platform.SaveDirectory("gba"), platform.SaveFileName(filepath.Base(marked), ""))
		if got != want {
			t.Errorf("[%s] saveLocalPath = %q, want %q", mode, got, want)
		}
	}
}

// TestFindLocalSavesFindsMarkedSave: the push side must DISCOVER a save the emulator wrote
// under the marked on-disk name, in both own and separate modes (separate carries the
// " (RomM)" disambiguator too). Pre-fix this matched only the bare server fs_name and
// silently found nothing, so a marked game's save never pushed.
func TestFindLocalSavesFindsMarkedSave(t *testing.T) {
	for _, mode := range []string{config.MirrorModeOwn, config.MirrorModeSeparate} {
		cfg, rom, canonical := markerSaveEnv(t, mode)
		marked := filepath.Join(filepath.Dir(canonical), platform.MarkerOnDevice+filepath.Base(canonical))
		saveDir := platform.SaveDirectory("gba")
		if err := os.MkdirAll(saveDir, 0o755); err != nil {
			t.Fatal(err)
		}
		savePath := filepath.Join(saveDir, filepath.Base(marked)+".sav")
		if err := os.WriteFile(savePath, []byte("SAVE"), 0o644); err != nil {
			t.Fatal(err)
		}
		found := findLocalSavesForRom(cfg, rom)
		if len(found) != 1 || found[0].path != savePath {
			t.Errorf("[%s] findLocalSavesForRom = %v, want exactly [%s]", mode, found, savePath)
		}
	}
}

// TestFindLocalSavesRetroArchNoExt: RetroArch-style ".srm" (which REPLACES the extension)
// under the marked name must match via the no-extension key too.
func TestFindLocalSavesRetroArchNoExt(t *testing.T) {
	cfg, rom, canonical := markerSaveEnv(t, config.MirrorModeSeparate)
	marked := platform.MarkerOnDevice + filepath.Base(canonical) // "[v] Sonic (USA) (RomM).gba"
	stem := marked[:len(marked)-len(filepath.Ext(marked))]       // drop ".gba"
	saveDir := platform.SaveDirectory("gba")
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	savePath := filepath.Join(saveDir, stem+".srm")
	if err := os.WriteFile(savePath, []byte("SRM"), 0o644); err != nil {
		t.Fatal(err)
	}
	found := findLocalSavesForRom(cfg, rom)
	if len(found) != 1 || found[0].path != savePath {
		t.Fatalf("srm: findLocalSavesForRom = %v, want [%s]", found, savePath)
	}
}
