//go:build !onion && !muos && !knulli && !android && !lodorandroid

package catalog

// Stranded multi-disc bytes (interrupted download): the mirror stubs the .m3u
// FIRST, discs land in the per-game folder, and the real playlist is only written
// at the end — so a mid-download failure leaves a 0-byte stub .m3u with real disc
// bytes beside it. EvictToStub's "stub" refusal used to make those bytes
// unreclaimable forever (a stub references nothing, so evictDiscFiles found
// nothing). The sweep must reclaim EXACTLY the manifest-owned per-game folder and
// never touch an unowned same-named folder (the user's own files in merge mode).

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
)

func strandedEnv(t *testing.T) (cfg *config.Config, base string) {
	t.Helper()
	base = mergeTestEnv(t)
	cfg = &config.Config{
		MirrorMode: config.MirrorModeMerge,
		DirectoryMappings: map[string]config.DirMapping{
			"psx": {Slug: "psx", RelativePath: "PlayStation (PS)"},
		},
	}
	return cfg, base
}

// TestEvictStubM3USweepsOwnedDiscDir: stub .m3u + real disc in the manifest-owned
// per-game folder — evict still refuses with "stub" (the playlist IS the stub
// shape) but the stranded disc bytes are swept.
func TestEvictStubM3USweepsOwnedDiscDir(t *testing.T) {
	cfg, base := strandedEnv(t)

	m3u := writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA).m3u", nil) // 0-byte stub
	disc := writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd", []byte("REAL DISC BYTES"))
	discDir := filepath.Dir(disc)

	man := platform.LoadManifest()
	man.Record(discDir, platform.ManifestFolder, 77) // record-intent-then-act wrote this before disc 1 landed
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	evicted, reason := EvictToStub(cfg, m3u)
	if evicted || reason != "stub" {
		t.Fatalf("EvictToStub = (%v, %q), want (false, \"stub\")", evicted, reason)
	}
	if _, err := os.Stat(discDir); !os.IsNotExist(err) {
		t.Errorf("owned disc dir not swept: stat err = %v", err)
	}
	if fi, err := os.Stat(m3u); err != nil || fi.Size() != 0 {
		t.Errorf("stub .m3u must survive as the 0-byte stub: fi=%v err=%v", fi, err)
	}
}

// TestEvictStubM3USweepsMarkedName: the stub may sit under its cloud-marked name
// ("✘ Game.m3u") while the per-game folder keeps the canonical stem — the sweep
// derives the folder marker-stripped, exactly like the download path.
func TestEvictStubM3USweepsMarkedName(t *testing.T) {
	if platform.HostShowsStateNatively() {
		t.Skip("marker-less host (hard-true build tag): marked-name expectations do not apply")
	}
	cfg, base := strandedEnv(t)

	m3u := writeCard(t, base, "Roms/PlayStation (PS)/"+platform.MarkerCloud+"Chrono Cross (USA).m3u", nil)
	disc := writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd", []byte("REAL DISC BYTES"))
	discDir := filepath.Dir(disc)

	man := platform.LoadManifest()
	man.Record(discDir, platform.ManifestFolder, 77)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	if evicted, reason := EvictToStub(cfg, m3u); evicted || reason != "stub" {
		t.Fatalf("EvictToStub = (%v, %q), want (false, \"stub\")", evicted, reason)
	}
	if _, err := os.Stat(discDir); !os.IsNotExist(err) {
		t.Errorf("owned disc dir not swept under marked stub name: stat err = %v", err)
	}
}

// TestEvictStubM3URefusesUnownedDir: same shape but the folder is NOT in the
// manifest (user's own folder / manifest-less card) — every byte must survive.
func TestEvictStubM3URefusesUnownedDir(t *testing.T) {
	cfg, base := strandedEnv(t)

	m3u := writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA).m3u", nil)
	disc := writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd", []byte("USERS OWN BYTES"))

	if evicted, reason := EvictToStub(cfg, m3u); evicted || reason != "stub" {
		t.Fatalf("EvictToStub = (%v, %q), want (false, \"stub\")", evicted, reason)
	}
	got, err := os.ReadFile(disc)
	if err != nil || string(got) != "USERS OWN BYTES" {
		t.Errorf("unowned folder touched: %q err=%v", got, err)
	}
}
