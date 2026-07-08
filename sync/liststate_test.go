//go:build !onion && !muos && !knulli && !android && !lodorandroid

package sync

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lodor/config"
	"lodor/romm"
)

// Task #135 — the Smart Pro 2026-07-03 twin-save field bug, reproduced byte-for-byte.
//
// Card state at launch: the coexist twins both carried saves —
//
//	Saves/GBA/✓ Pokemon - Emerald Version (USA, Europe) (RomM).gba.sav  == rev 471 (newest, RG34XX)
//	Saves/GBA/✓ Pokemon - Emerald Version (USA, Europe).gba.sav          == rev 467 (older)
//
// Launching the CLEAN twin loads the rev-467 bytes, but the old aggregate LOCAL=
// computation matched the OTHER twin's save against rev 471 and emitted
// LOCAL=current -> the hook logged "newest=current action=silent" and the restore
// prompt (the A5 headline) never fired. Strict semantics: LOCAL= is judged against
// the save THE LAUNCH WILL LOAD only, and "current" only against the NEWEST revision.
func twinSaveEnv(t *testing.T) (*config.Config, romm.Rom, string, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	cfg := &config.Config{
		MirrorMode:        config.MirrorModeSeparate,
		DirectoryMappings: map[string]config.DirMapping{"gba": {RelativePath: "GBA"}},
	}
	rom := romm.Rom{
		ID:             12765,
		PlatformFsSlug: "gba",
		FsName:         "Pokemon - Emerald Version (USA, Europe).gba",
		FsNameNoExt:    "Pokemon - Emerald Version (USA, Europe)",
		Files:          []romm.RomFile{{FileName: "Pokemon - Emerald Version (USA, Europe).gba"}},
	}
	romsDir := filepath.Join(base, "Roms", "GBA")
	cleanRom := filepath.Join(romsDir, "✓ Pokemon - Emerald Version (USA, Europe).gba")
	twinRom := filepath.Join(romsDir, "✓ Pokemon - Emerald Version (USA, Europe) (RomM).gba")
	return cfg, rom, cleanRom, twinRom
}

func md5hex(b []byte) string { return fmt.Sprintf("%x", md5.Sum(b)) }

func strptr(s string) *string { return &s }

func TestListSavesLocalStateTwinField(t *testing.T) {
	cfg, rom, cleanRom, twinRom := twinSaveEnv(t)

	saveDir := filepath.Join(os.Getenv("BASE_PATH"), "Saves", "GBA")
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	newestBytes := []byte("RG34XX-PROGRESS-rev471")
	olderBytes := []byte("ROTATE-SAVE-rev467")
	twinSav := filepath.Join(saveDir, "✓ Pokemon - Emerald Version (USA, Europe) (RomM).gba.sav")
	cleanSav := filepath.Join(saveDir, "✓ Pokemon - Emerald Version (USA, Europe).gba.sav")
	if err := os.WriteFile(twinSav, newestBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cleanSav, olderBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Newest-first server list, exactly the field pair: rev 471 (RG34XX) then rev 467.
	now := time.Now()
	saves := []romm.Save{
		{ID: 471, RomID: rom.ID, ContentHash: strptr(md5hex(newestBytes)), UpdatedAt: now},
		{ID: 467, RomID: rom.ID, ContentHash: strptr(md5hex(olderBytes)), UpdatedAt: now.Add(-10 * time.Hour)},
	}

	// The launched CLEAN twin loads ONLY its own save (never the (RomM) twin's).
	primary := primaryLocalSaves(cfg, rom, cleanRom)
	if len(primary) != 1 || primary[0] != cleanSav {
		t.Fatalf("primary saves for clean twin = %v, want exactly [%s]", primary, cleanSav)
	}
	// THE FIELD ASSERTION: local == older revision, newest is foreign -> LOCAL=older
	// (the hook prompts). The old aggregate computation returned "current" here.
	if got := ListSavesLocalState(saves, primary); got != "older" {
		t.Fatalf("LOCAL for clean twin = %q, want %q", got, "older")
	}

	// Launching the (RomM) twin loads the rev-471 bytes -> genuinely current, silent.
	primary = primaryLocalSaves(cfg, rom, twinRom)
	if len(primary) != 1 || primary[0] != twinSav {
		t.Fatalf("primary saves for (RomM) twin = %v, want exactly [%s]", primary, twinSav)
	}
	if got := ListSavesLocalState(saves, primary); got != "current" {
		t.Fatalf("LOCAL for (RomM) twin = %q, want %q", got, "current")
	}

	// No save for the launched name -> none, even though a twin save exists (the
	// launch loads nothing; pulling the newest is lose-proof).
	if err := os.Remove(cleanSav); err != nil {
		t.Fatal(err)
	}
	if primary = primaryLocalSaves(cfg, rom, cleanRom); len(primary) != 0 {
		t.Fatalf("primary saves after removal = %v, want none", primary)
	}
	if got := ListSavesLocalState(saves, primary); got != "none" {
		t.Fatalf("LOCAL with no launched-name save = %q, want %q", got, "none")
	}

	// Launched save matching NO revision -> unpushed local progress.
	if err := os.WriteFile(cleanSav, []byte("BRAND-NEW-LOCAL-PROGRESS"), 0o644); err != nil {
		t.Fatal(err)
	}
	primary = primaryLocalSaves(cfg, rom, cleanRom)
	if got := ListSavesLocalState(saves, primary); got != "unpushed" {
		t.Fatalf("LOCAL for unmatched save = %q, want %q", got, "unpushed")
	}
}
