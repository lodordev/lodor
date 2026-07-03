package sync

// Meta-save exclusion tests (task #146 first-commit contract): a .lodortime /
// .lodorshot.png record riding the saves transport must never behave like a
// real save anywhere — extending the ghost-test patterns (ghost_test.go).

import (
	"testing"
	"time"

	"lodor/romm"
)

func TestIsMetaSaveName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Game (USA).gba.lodortime", true},
		{"Game (USA).gba.lodorshot.png", true},
		{"GAME.LODORTIME", true},      // case-insensitive
		{"Game.LodorShot.PNG", true},  // case-insensitive
		{"Game (USA).gba.sav", false}, // real save
		{"Game (USA).srm", false},     // real save
		{"lodortime", false},          // no dot — not the extension shape
		{"Game.lodortime.sav", false}, // meta-like stem but a real save ext
		{"Game (USA).png", false},     // plain screenshot name is not a meta-save
		{"", false},
	}
	for _, c := range cases {
		if got := IsMetaSaveName(c.name); got != c.want {
			t.Errorf("IsMetaSaveName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsMetaSave(t *testing.T) {
	if !IsMetaSave(romm.Save{FileName: "Game.gba.lodortime", FileSizeBytes: 42}) {
		t.Error("lodortime record not classified as meta")
	}
	if IsMetaSave(romm.Save{FileName: "Game.gba.sav", FileSizeBytes: 42}) {
		t.Error("real save classified as meta")
	}
}

// TestSplitGhostsDropsMetaSaves: SplitGhosts is the shared "usable server
// saves" funnel (pull, restore listing) — meta-saves must vanish from real[]
// WITHOUT being counted as ghosts (they aren't broken, just not saves).
func TestSplitGhostsDropsMetaSaves(t *testing.T) {
	saves := []romm.Save{
		{ID: 1, FileName: "Game.gba.sav", FileSizeBytes: 100},
		{ID: 2, FileName: "Game.gba.lodortime", FileSizeBytes: 64},      // meta, healthy bytes
		{ID: 3, FileName: "Game.gba.lodorshot.png", FileSizeBytes: 900}, // meta, healthy bytes
		{ID: 4, FileName: "Game.gba.sav", FileSizeBytes: 0},             // ghost
	}
	real, ghosts := SplitGhosts(saves)
	if len(real) != 1 || real[0].ID != 1 {
		t.Errorf("real = %+v, want only ID 1", real)
	}
	if ghosts != 1 {
		t.Errorf("ghosts = %d, want 1 (meta-saves are not ghosts)", ghosts)
	}
}

// TestMetaSaveNeverNewest locks the pull invariant (the #146 contract's whole
// point): even a meta-save with the newest timestamp and healthy bytes can
// never win newest-wins and overwrite a real local save file.
func TestMetaSaveNeverNewest(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	saves := []romm.Save{
		{ID: 1, FileName: "Game.gba.sav", FileSizeBytes: 100, UpdatedAt: t0},
		{ID: 2, FileName: "Game.gba.lodortime", FileSizeBytes: 64, UpdatedAt: t0.Add(48 * time.Hour)},
	}
	real, _ := SplitGhosts(saves)
	if got := newestSave(real); got.ID != 1 {
		t.Errorf("newest = ID %d, want 1 (a .lodortime must never be 'newest save')", got.ID)
	}
}

// TestRestoreSaveRefusesMetaSave: an explicit restore of a meta-save record is
// refused up front — mirrors TestRestoreSaveRefusesGhost.
func TestRestoreSaveRefusesMetaSave(t *testing.T) {
	res := RestoreSave(nil, nil, "/tmp/nonexistent.gba",
		romm.Save{ID: 9, FileName: "Game.gba.lodortime", FileSizeBytes: 64})
	if res.Outcome != PullError {
		t.Fatalf("RestoreSave(meta) outcome = %v, want PullError", res.Outcome)
	}
}

// TestMarkerTwinIDsIgnoresMetaSaves: the canonical-name heal must neither
// delete a meta record nor let one vouch for a marker-named twin's bytes.
func TestMarkerTwinIDsIgnoresMetaSaves(t *testing.T) {
	h := "aabbccdd"
	saves := []romm.Save{
		// meta record under a clean name with a hash: must NOT vouch
		{ID: 1, FileName: "Game.gba.lodortime", FileSizeBytes: 64, ContentHash: &h},
		// marker-named REAL save with the same hash: no clean REAL twin -> keep
		{ID: 2, FileName: "✘ Game.gba.sav", FileSizeBytes: 100, ContentHash: &h},
		// marker-named META record: never a heal candidate, whatever its hash
		{ID: 3, FileName: "✘ Game.gba.lodorshot.png", FileSizeBytes: 900, ContentHash: &h},
	}
	if ids := markerTwinIDs(saves); len(ids) != 0 {
		t.Errorf("markerTwinIDs = %v, want none (meta-saves out of the heal entirely)", ids)
	}
}
