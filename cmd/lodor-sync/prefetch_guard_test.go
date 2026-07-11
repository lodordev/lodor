//go:build !onion && !muos && !knulli && !android && !lodorandroid

package main

// Guard for the daemons' --prefetch-discs loop: the census
// (IncompleteMultiDiscDownloads) runs once at cycle start, so a game the user
// evicts while earlier games are still transferring is stale census data. The
// re-stat guard must reject exactly the mirror's stub shape (0 bytes) plus the
// gone/degenerate cases, and accept a still-real playlist — otherwise the daemon
// silently re-downloads a game the user just deleted.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrefetchStillWanted(t *testing.T) {
	dir := t.TempDir()

	real := filepath.Join(dir, "real.m3u")
	if err := os.WriteFile(real, []byte("Game/Game (Disc 1).chd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "stub.m3u")
	if err := os.WriteFile(stub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	asDir := filepath.Join(dir, "dir.m3u")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"real playlist still wants completion", real, true},
		{"0-byte stub = evicted mid-cycle, never refill", stub, false},
		{"deleted outright", filepath.Join(dir, "gone.m3u"), false},
		{"directory at the path", asDir, false},
	}
	for _, c := range cases {
		if got := prefetchStillWanted(c.path); got != c.want {
			t.Errorf("%s: prefetchStillWanted=%v want %v", c.name, got, c.want)
		}
	}
}
