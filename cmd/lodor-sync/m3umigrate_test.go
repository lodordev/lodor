//go:build !onion && !muos && !knulli && !android && !lodorandroid

package main

// migrateLegacyM3U coverage (lodor#7, local-only .m3u): existing cards in the field
// carry FULL-LIST playlists that still reference 0-byte stub discs — the exact
// shape the shipped LodorOS launcher refuses to launch (the hardware-verified
// never-launches regression). On the next touch (any fetch mode) those must
// normalize OFFLINE to the local-only contract:
//
//   1. THE LIVE 3-OF-4 CARD: full 4-disc .m3u, discs 1-3 real, disc 4 a stub →
//      m3u rewritten to the 3 present discs (order kept), manifest canonical list
//      seeded with all 4 (the old playlist IS the full set), census reads 3/4.
//      CRLF lines tolerated (hand-edited/Windows-touched playlists).
//   2. UNOWNED playlist (user's own .m3u, merge mode): never rewritten, never
//      seeded — THE ONE RULE.
//   3. ALL-STUBS legacy playlist: normalizes to the 0-byte stub shape the launch
//      path already owns; canonical list seeded first.
//   4. ALREADY LOCAL-ONLY (every listed disc real) with no manifest list: playlist
//      untouched, canonical list seeded from it (census manifest-first from then on).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lodor/catalog"
	"lodor/platform"
)

// migEnv builds an offline card env (no server — migration must never need one).
func migEnv(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_HOST_OS", "nextui")
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "Tools", "tg5040", "Lodor.pak"))
	if err := os.MkdirAll(filepath.Join(base, "Tools", "tg5040", "Lodor.pak"), 0o755); err != nil {
		t.Fatal(err)
	}
	return base
}

// migGame lays down a legacy multi-disc game: an OWNED full-list .m3u (content as
// given) plus disc files — real bytes for indexes in realDiscs, 0-byte stubs
// otherwise. Returns the m3u path and the disc dir.
func migGame(t *testing.T, base, m3uBody string, discs []string, realDiscs map[int]bool, owned bool) (string, string) {
	t.Helper()
	psDir := filepath.Join(base, "Roms", "PlayStation (PS)")
	discDir := filepath.Join(psDir, "Legacy Quest (USA)")
	if err := os.MkdirAll(discDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m3u := filepath.Join(psDir, "Legacy Quest (USA).m3u")
	if err := os.WriteFile(m3u, []byte(m3uBody), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, d := range discs {
		var body []byte
		if realDiscs[i] {
			body = []byte("DISC" + d)
		}
		if err := os.WriteFile(filepath.Join(discDir, d), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if owned {
		man := platform.LoadManifest()
		man.Record(m3u, platform.ManifestDownload, 4242)
		man.Record(discDir, platform.ManifestFolder, 4242)
		if err := man.Save(); err != nil {
			t.Fatal(err)
		}
	}
	return m3u, discDir
}

func TestMigrateLegacyM3U_ThreeOfFourCard(t *testing.T) {
	base := migEnv(t)
	discs := []string{
		"Legacy Quest (USA) (Disc 1).chd",
		"Legacy Quest (USA) (Disc 2).chd",
		"Legacy Quest (USA) (Disc 3).chd",
		"Legacy Quest (USA) (Disc 4).chd",
	}
	// CRLF playlist — the parser must stay CRLF-tolerant.
	var b strings.Builder
	for _, d := range discs {
		b.WriteString("Legacy Quest (USA)/" + d + "\r\n")
	}
	m3u, _ := migGame(t, base, b.String(), discs, map[int]bool{0: true, 1: true, 2: true}, true)

	migrateLegacyM3U(m3u)

	want := "Legacy Quest (USA)/" + discs[0] + "\n" +
		"Legacy Quest (USA)/" + discs[1] + "\n" +
		"Legacy Quest (USA)/" + discs[2] + "\n"
	if got, err := os.ReadFile(m3u); err != nil || string(got) != want {
		t.Errorf("migrated m3u = %q (err %v), want the 3 present discs (LF, order kept)", string(got), err)
	}
	man := platform.LoadManifest()
	e, ok := man.Entry(m3u)
	if !ok || len(e.Discs) != 4 {
		t.Fatalf("canonical list not seeded from the old playlist: %+v ok=%v", e, ok)
	}
	for i, d := range discs {
		if e.Discs[i] != "Legacy Quest (USA)/"+d {
			t.Errorf("seeded discs[%d] = %q, want %q", i, e.Discs[i], "Legacy Quest (USA)/"+d)
		}
	}
	if total, present := catalog.RomDiscCompleteness(man, m3u); total != 4 || present != 3 {
		t.Errorf("census after migration = %d/%d, want 3/4", present, total)
	}
	// Idempotent: a second touch changes nothing.
	before, _ := os.ReadFile(m3u)
	migrateLegacyM3U(m3u)
	after, _ := os.ReadFile(m3u)
	if string(before) != string(after) {
		t.Errorf("second migration touch changed the playlist")
	}
}

func TestMigrateLegacyM3U_NeverTouchesUnownedPlaylist(t *testing.T) {
	base := migEnv(t)
	discs := []string{"Legacy Quest (USA) (Disc 1).chd", "Legacy Quest (USA) (Disc 2).chd"}
	body := "Legacy Quest (USA)/" + discs[0] + "\n" + "Legacy Quest (USA)/" + discs[1] + "\n"
	m3u, _ := migGame(t, base, body, discs, map[int]bool{0: true}, false /* NOT manifest-owned */)

	migrateLegacyM3U(m3u)

	if got, err := os.ReadFile(m3u); err != nil || string(got) != body {
		t.Errorf("user's own playlist rewritten by migration: %q (err %v)", string(got), err)
	}
	if e, ok := platform.LoadManifest().Entry(m3u); ok {
		t.Errorf("migration invented a manifest entry for a user playlist: %+v", e)
	}
}

func TestMigrateLegacyM3U_AllStubsBecomesStubShape(t *testing.T) {
	base := migEnv(t)
	discs := []string{"Legacy Quest (USA) (Disc 1).chd", "Legacy Quest (USA) (Disc 2).chd"}
	body := "Legacy Quest (USA)/" + discs[0] + "\n" + "Legacy Quest (USA)/" + discs[1] + "\n"
	m3u, _ := migGame(t, base, body, discs, nil /* every disc a stub */, true)

	migrateLegacyM3U(m3u)

	if fi, err := os.Stat(m3u); err != nil || fi.Size() != 0 {
		t.Errorf("all-stubs playlist should normalize to the 0-byte stub shape (size=%v err=%v)", fi, err)
	}
	e, ok := platform.LoadManifest().Entry(m3u)
	if !ok || len(e.Discs) != 2 {
		t.Errorf("canonical list not seeded before the stub-shape rewrite: %+v ok=%v", e, ok)
	}
}

func TestMigrateLegacyM3U_SeedsAlreadyLocalOnly(t *testing.T) {
	base := migEnv(t)
	discs := []string{"Legacy Quest (USA) (Disc 1).chd", "Legacy Quest (USA) (Disc 2).chd"}
	body := "Legacy Quest (USA)/" + discs[0] + "\n" + "Legacy Quest (USA)/" + discs[1] + "\n"
	m3u, _ := migGame(t, base, body, discs, map[int]bool{0: true, 1: true}, true)

	migrateLegacyM3U(m3u)

	if got, err := os.ReadFile(m3u); err != nil || string(got) != body {
		t.Errorf("complete playlist rewritten: %q (err %v)", string(got), err)
	}
	e, ok := platform.LoadManifest().Entry(m3u)
	if !ok || len(e.Discs) != 2 {
		t.Errorf("canonical list not seeded from a complete playlist: %+v ok=%v", e, ok)
	}
}
