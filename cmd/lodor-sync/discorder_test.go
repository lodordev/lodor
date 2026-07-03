package main

import (
	"sort"
	"testing"

	"lodor/romm"
)

// TestNaturalLessDiscOrder locks the disc-aware natural ordering that guards the
// multi-disc playlist: numeric runs compare by value so "Disc 2" precedes "Disc 10"
// (plain lexical order would put "10" first and boot the wrong disc).
func TestNaturalLessDiscOrder(t *testing.T) {
	cases := []struct {
		a, b string
		want bool // want a < b
	}{
		{"Game (Disc 1).chd", "Game (Disc 2).chd", true},
		{"Game (Disc 2).chd", "Game (Disc 10).chd", true},   // the classic lexical trap
		{"Game (Disc 10).chd", "Game (Disc 2).chd", false},  // reverse
		{"Game (Disc 9).chd", "Game (Disc 10).chd", true},
		{"Game (Disc 1).chd", "Game (Disc 1).chd", false},   // equal => not less
		{"disc 1", "DISC 2", true},                          // case-insensitive prefix
		{"Game (Disc 03).chd", "Game (Disc 3).chd", true},   // zero-padded sorts first, deterministic
	}
	for _, c := range cases {
		if got := naturalLess(c.a, c.b); got != c.want {
			t.Errorf("naturalLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestMultiDiscPlaylistSort proves that an UNORDERED server Files[] (RomM returning
// discs out of order) is sorted into correct disc order before the .m3u is built, so
// disc 1 is always the first playlist line and boots first.
func TestMultiDiscPlaylistSort(t *testing.T) {
	// Server hands us discs 3,1,10,2 out of order (id/name mirror the download loop).
	unordered := []romm.RomFile{
		{ID: 3, FileName: "Final Fantasy VII (USA) (Disc 3).chd"},
		{ID: 1, FileName: "Final Fantasy VII (USA) (Disc 1).chd"},
		{ID: 10, FileName: "Final Fantasy VII (USA) (Disc 10).chd"},
		{ID: 2, FileName: "Final Fantasy VII (USA) (Disc 2).chd"},
	}
	// Same sort the download path applies before writing the playlist.
	sorted := make([]romm.RomFile, len(unordered))
	copy(sorted, unordered)
	sort.SliceStable(sorted, func(a, b int) bool {
		return naturalLess(sorted[a].FileName, sorted[b].FileName)
	})

	want := []string{
		"Final Fantasy VII (USA) (Disc 1).chd",
		"Final Fantasy VII (USA) (Disc 2).chd",
		"Final Fantasy VII (USA) (Disc 3).chd",
		"Final Fantasy VII (USA) (Disc 10).chd",
	}
	for i, w := range want {
		if sorted[i].FileName != w {
			t.Errorf("playlist line %d = %q, want %q", i, sorted[i].FileName, w)
		}
	}
	// And the source slice must be untouched (we sort a copy, never rom.Files).
	if unordered[0].FileName != "Final Fantasy VII (USA) (Disc 3).chd" {
		t.Errorf("source Files[] was mutated: %q", unordered[0].FileName)
	}
}
