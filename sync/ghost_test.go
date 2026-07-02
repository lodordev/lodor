package sync

// Ghost-save (#63) unit tests: classification, filtering, and the invariant that
// a ghost can never be selected as the "newest" server save (i.e. can never
// overwrite a real local save in a newest-wins pull).

import (
	"testing"
	"time"

	"lodor/romm"
)

func TestIsGhostSave(t *testing.T) {
	cases := []struct {
		name string
		s    romm.Save
		want bool
	}{
		{"zero size is a ghost", romm.Save{ID: 1, FileSizeBytes: 0}, true},
		{"negative size is a ghost", romm.Save{ID: 1, FileSizeBytes: -1}, true},
		{"omitted size (zero value) is a ghost", romm.Save{ID: 1}, true},
		{"real save is not", romm.Save{ID: 1, FileSizeBytes: 8192}, false},
		{"1 byte is not", romm.Save{ID: 1, FileSizeBytes: 1}, false},
	}
	for _, c := range cases {
		if got := IsGhostSave(c.s); got != c.want {
			t.Errorf("%s: IsGhostSave = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSplitGhosts(t *testing.T) {
	saves := []romm.Save{
		{ID: 1, FileSizeBytes: 100},
		{ID: 2, FileSizeBytes: 0},
		{ID: 3, FileSizeBytes: 200},
		{ID: 4, FileSizeBytes: 0},
	}
	real, ghosts := SplitGhosts(saves)
	if ghosts != 2 {
		t.Errorf("ghosts = %d, want 2", ghosts)
	}
	if len(real) != 2 || real[0].ID != 1 || real[1].ID != 3 {
		t.Errorf("real = %+v, want IDs [1 3] in order", real)
	}

	real, ghosts = SplitGhosts(nil)
	if real != nil || ghosts != 0 {
		t.Errorf("SplitGhosts(nil) = (%v, %d), want (nil, 0)", real, ghosts)
	}
}

// TestGhostNeverNewest locks the pull invariant: even when a ghost record carries
// the NEWEST timestamp, filtering + newestSave must pick the newest REAL save —
// so a ghost can never win newest-wins and overwrite a real local file.
func TestGhostNeverNewest(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	saves := []romm.Save{
		{ID: 1, FileSizeBytes: 100, UpdatedAt: t0},
		{ID: 2, FileSizeBytes: 0, UpdatedAt: t0.Add(48 * time.Hour)}, // newest, but a ghost
		{ID: 3, FileSizeBytes: 200, UpdatedAt: t0.Add(24 * time.Hour)},
	}
	real, ghosts := SplitGhosts(saves)
	if ghosts != 1 {
		t.Fatalf("ghosts = %d, want 1", ghosts)
	}
	if got := newestSave(real); got.ID != 3 {
		t.Errorf("newest real save = ID %d, want 3 (the ghost with the newest timestamp must not win)", got.ID)
	}
}

// TestRestoreSaveRefusesGhost: an explicit restore of a byte-less record is
// refused up front — nothing is fetched, nothing overwritten.
func TestRestoreSaveRefusesGhost(t *testing.T) {
	res := RestoreSave(nil, nil, "/tmp/nonexistent.gba", romm.Save{ID: 9, FileSizeBytes: 0})
	if res.Outcome != PullError {
		t.Fatalf("RestoreSave(ghost) outcome = %v, want PullError", res.Outcome)
	}
	if res.Ghosts != 1 {
		t.Errorf("RestoreSave(ghost) Ghosts = %d, want 1", res.Ghosts)
	}
}
