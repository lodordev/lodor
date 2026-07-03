package catalog

// Meta-save exclusion in the Continue/recents feed (task #146): a .lodortime /
// .lodorshot.png record riding the saves transport must never drive recency —
// mirrors the ghost case in continue_test.go.

import (
	"strings"
	"testing"

	"lodor/romm"
)

func TestContinueListIgnoresMetaSaves(t *testing.T) {
	cfg, _, rel := continueTestEnv(t, 2)

	fake := &continueFake{
		platforms: []romm.Platform{{ID: 10, FsSlug: "gba", Name: "Game Boy Advance"}},
		saves: map[int][]romm.Save{10: {
			{ID: 1, RomID: 1, FileName: "Game 1.gba.sav", FileSizeBytes: 100, UpdatedAt: at(1)},
			{ID: 2, RomID: 2, FileName: "Game 2.gba.sav", FileSizeBytes: 100, UpdatedAt: at(3)},
			// rom1 meta-saves carry the NEWEST timestamps and healthy bytes — they
			// must neither promote rom1 above rom2 nor appear as sessions at all.
			{ID: 3, RomID: 1, FileName: "Game 1.gba.lodortime", FileSizeBytes: 64, UpdatedAt: at(6)},
			{ID: 4, RomID: 1, FileName: "Game 1.gba.lodorshot.png", FileSizeBytes: 900, UpdatedAt: at(7)},
		}},
	}

	idx, err := loadIndex(IndexPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	lines := ContinueList(fake, cfg, idx.Platforms["gba"].ByID)
	if len(lines) != 2 {
		t.Fatalf("ContinueList = %v, want 2 entries", lines)
	}
	if !strings.Contains(lines[0], "Game 2") {
		t.Errorf("lines[0] = %q, want Game 2 first (meta-saves must not promote Game 1)", lines[0])
	}
	if !strings.Contains(lines[1], rel(1)[len("/"):]) && !strings.Contains(lines[1], "Game 1") {
		t.Errorf("lines[1] = %q, want Game 1 second", lines[1])
	}
}
