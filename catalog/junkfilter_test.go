package catalog

import "testing"

// TestJunkFilterDropsNonGames verifies the mirror's non-game denylist drops the
// bundle members RomM's API returns as standalone "rom" entries (manuals, videos,
// info, box-art) and the save-as-rom entries, while NEVER dropping a real ROM.
func TestJunkFilterDropsNonGames(t *testing.T) {
	// (path, wantSave, wantAsset) — at least one true => not stubbed into Roms/.
	cases := []struct {
		path      string
		wantSave  bool
		wantAsset bool
	}{
		// Live-confirmed junk rom entries (RomM library, 2026-06-27):
		{"Roms/Game Boy Color (GBC)/_info.txt", false, true},
		{"Roms/Sega Game Gear (GG)/_info.txt", false, true},
		{"Roms/Game Boy Color (GBC)/Pokemon Core Crystal.rtc", true, false},
		{"Roms/Neo Geo Pocket/Sonic The Hedgehog - Pocket Adventure (World) (Demo).flash", true, false},
		{"Roms/Nintendo DS (NDS)/Diddy Kong Racing DS (USA).sav", true, false},
		// Manual/video bundle members (by extension and by name convention):
		{"Roms/SNES/Chrono Trigger-manual.pdf", false, true},
		{"Roms/SNES/Chrono Trigger-video.mp4", false, true},
		{"Roms/SNES/Chrono Trigger-manual.weirdext", false, true},
		{"Roms/SNES/box.png", false, true},
		// Real ROMs — MUST NOT be filtered:
		{"Roms/SNES/Chrono Trigger (USA).sfc", false, false},
		{"Roms/PS1/Final Fantasy VII (USA).m3u", false, false},
		{"Roms/PS1/Final Fantasy VII (USA).cue", false, false},
		{"Roms/PS1/Final Fantasy VII (USA).bin", false, false},
		{"Roms/PS1/Final Fantasy VII (USA).chd", false, false},
		{"Roms/Game Boy Color (GBC)/Pokemon Core Crystal.gbc", false, false},
		{"Roms/N64/Mario.z64", false, false},
		{"Roms/NDS/Mario.nds", false, false},
	}
	for _, c := range cases {
		if got := isSaveExt(c.path); got != c.wantSave {
			t.Errorf("isSaveExt(%q)=%v want %v", c.path, got, c.wantSave)
		}
		if got := isNonGameAsset(c.path); got != c.wantAsset {
			t.Errorf("isNonGameAsset(%q)=%v want %v", c.path, got, c.wantAsset)
		}
	}
}
