package platform

// Canonical disc list on the manifest (lodor#7, local-only .m3u): the .m3u on
// card lists only present discs, so the manifest record carries the full set.
// These tests lock the lifecycle guarantees the census depends on:
//
//   1. SetDiscs persists across Save/Load (SDCARD-relative key, like every entry).
//   2. The list SURVIVES kind flips in place — stub→download (fill) and
//      download→stub (evict re-Record) — and rides RenamePath (marker flips).
//   3. SetDiscs on an unrecorded path is a NO-OP: ownership is never invented.

import (
	"os"
	"path/filepath"
	"testing"
)

func discsTestEnv(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))
	if err := os.MkdirAll(filepath.Join(base, "pak"), 0o755); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestManifestDiscsPersistAndSurviveKindFlips(t *testing.T) {
	base := discsTestEnv(t)
	m3u := filepath.Join(base, "Roms", "PS", "Game.m3u")
	discs := []string{"Game/Game (Disc 1).chd", "Game/Game (Disc 2).chd"}

	man := LoadManifest()
	man.Record(m3u, ManifestStub, 42)
	man.SetDiscs(m3u, discs)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	// Round-trip.
	man = LoadManifest()
	e, ok := man.Entry(m3u)
	if !ok || len(e.Discs) != 2 || e.Discs[0] != discs[0] || e.Discs[1] != discs[1] {
		t.Fatalf("discs did not round-trip: %+v ok=%v", e, ok)
	}

	// stub→download (the fill) keeps the list.
	man.Record(m3u, ManifestDownload, 42)
	if e, _ := man.Entry(m3u); len(e.Discs) != 2 {
		t.Fatalf("discs lost on stub→download re-record: %+v", e)
	}
	// Marker flip (rename) carries the list.
	flipped := filepath.Join(base, "Roms", "PS", "✓ Game.m3u")
	man.RenamePath(m3u, flipped)
	if e, ok := man.Entry(flipped); !ok || len(e.Discs) != 2 {
		t.Fatalf("discs lost on rename: %+v ok=%v", e, ok)
	}
	// download→stub (the evict) keeps the list for the next download cycle.
	man.Record(flipped, ManifestStub, 0)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}
	man = LoadManifest()
	if e, ok := man.Entry(flipped); !ok || e.Kind != ManifestStub || len(e.Discs) != 2 {
		t.Fatalf("discs lost across evict re-record + reload: %+v ok=%v", e, ok)
	}
}

func TestManifestSetDiscsNeverInventsOwnership(t *testing.T) {
	base := discsTestEnv(t)
	m3u := filepath.Join(base, "Roms", "PS", "Users Own.m3u")

	man := LoadManifest()
	man.SetDiscs(m3u, []string{"Users Own/disc.chd"})
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadManifest().Entry(m3u); ok {
		t.Fatalf("SetDiscs created an entry for an unrecorded path")
	}
}
