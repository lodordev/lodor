//go:build !onion && !muos && !knulli && !android && !lodorandroid

package main

// migrateLegacyM3U coverage (lodor#7, local-only .m3u + dot-hidden disc folder):
// existing cards in the field carry FULL-LIST playlists that still reference 0-byte
// stub discs — the exact shape the shipped LodorOS launcher refuses to launch (the
// hardware-verified never-launches regression) — AND a VISIBLE non-dot per-game
// disc folder that MinUI lists as a second, browsable entry beside the .m3u (the
// hardware-confirmed UX bug). On the next touch (any fetch mode) those must
// normalize OFFLINE:
//
//   1. THE LIVE 3-OF-4 CARD: full 4-disc .m3u, discs 1-3 real, disc 4 a stub →
//      folder renamed "<Game>/" → ".<Game>/", m3u rewritten to the 3 present discs
//      as ".<Game>/…" lines (order kept), manifest canonical list seeded with all
//      4 dot lines (the old playlist IS the full set), census reads 3/4.
//      CRLF lines tolerated (hand-edited/Windows-touched playlists). Idempotent.
//   2. UNOWNED playlist + folder (user's own .m3u, merge mode): never rewritten,
//      never renamed, never seeded — THE ONE RULE.
//   3. ALL-STUBS legacy playlist: dot-migrates, then normalizes to the 0-byte stub
//      shape the launch path already owns; canonical list seeded first.
//   4. ALREADY LOCAL-ONLY (every listed disc real) with no manifest list: playlist
//      dot-rewritten only, canonical list seeded from the dot lines.
//   5. STRANDED-BEHIND-A-STUB: a 0-byte .m3u with an owned legacy folder of discs
//      still dot-migrates the folder (the coming download must FIND those discs at
//      the dot path, not re-pull them).
//   6. BOTH LAYOUTS PRESENT: merge, dot copy wins; legacy remainder swept.

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
	m3u, oldDir := migGame(t, base, b.String(), discs, map[int]bool{0: true, 1: true, 2: true}, true)
	dotDir := filepath.Join(filepath.Dir(oldDir), "."+filepath.Base(oldDir))

	migrateLegacyM3U(m3u)

	// Dot-folder migration (lodor#7 UX fix): the visible legacy folder is GONE,
	// every disc file (real 1-3 + stub 4) now lives under the dot-hidden name.
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("legacy non-dot disc folder still present after migration (err %v)", err)
	}
	for _, d := range discs {
		if _, err := os.Stat(filepath.Join(dotDir, d)); err != nil {
			t.Errorf("disc %q not found under the dot folder: %v", d, err)
		}
	}
	want := ".Legacy Quest (USA)/" + discs[0] + "\n" +
		".Legacy Quest (USA)/" + discs[1] + "\n" +
		".Legacy Quest (USA)/" + discs[2] + "\n"
	if got, err := os.ReadFile(m3u); err != nil || string(got) != want {
		t.Errorf("migrated m3u = %q (err %v), want the 3 present discs as dot lines (LF, order kept)", string(got), err)
	}
	man := platform.LoadManifest()
	if !man.OwnsKind(dotDir, platform.ManifestFolder) {
		t.Errorf("manifest folder ownership did not follow the dot rename")
	}
	if man.Owns(oldDir) {
		t.Errorf("manifest still owns the legacy non-dot folder path")
	}
	e, ok := man.Entry(m3u)
	if !ok || len(e.Discs) != 4 {
		t.Fatalf("canonical list not seeded from the old playlist: %+v ok=%v", e, ok)
	}
	for i, d := range discs {
		if e.Discs[i] != ".Legacy Quest (USA)/"+d {
			t.Errorf("seeded discs[%d] = %q, want %q", i, e.Discs[i], ".Legacy Quest (USA)/"+d)
		}
	}
	if total, present := catalog.RomDiscCompleteness(man, m3u); total != 4 || present != 3 {
		t.Errorf("census after migration = %d/%d, want 3/4", present, total)
	}
	// Idempotent: a second touch changes neither playlist nor layout nor manifest.
	before, _ := os.ReadFile(m3u)
	migrateLegacyM3U(m3u)
	after, _ := os.ReadFile(m3u)
	if string(before) != string(after) {
		t.Errorf("second migration touch changed the playlist")
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("second touch resurrected the legacy folder (err %v)", err)
	}
	man = platform.LoadManifest()
	if e2, ok2 := man.Entry(m3u); !ok2 || len(e2.Discs) != 4 || e2.Discs[0] != e.Discs[0] {
		t.Errorf("second touch changed the canonical list: %+v", e2)
	}
}

func TestMigrateLegacyM3U_NeverTouchesUnownedPlaylist(t *testing.T) {
	base := migEnv(t)
	discs := []string{"Legacy Quest (USA) (Disc 1).chd", "Legacy Quest (USA) (Disc 2).chd"}
	body := "Legacy Quest (USA)/" + discs[0] + "\n" + "Legacy Quest (USA)/" + discs[1] + "\n"
	m3u, oldDir := migGame(t, base, body, discs, map[int]bool{0: true}, false /* NOT manifest-owned */)

	migrateLegacyM3U(m3u)

	if got, err := os.ReadFile(m3u); err != nil || string(got) != body {
		t.Errorf("user's own playlist rewritten by migration: %q (err %v)", string(got), err)
	}
	if fi, err := os.Stat(oldDir); err != nil || !fi.IsDir() {
		t.Errorf("user's own disc folder moved by migration (err %v)", err)
	}
	if e, ok := platform.LoadManifest().Entry(m3u); ok {
		t.Errorf("migration invented a manifest entry for a user playlist: %+v", e)
	}
}

func TestMigrateLegacyM3U_AllStubsBecomesStubShape(t *testing.T) {
	base := migEnv(t)
	discs := []string{"Legacy Quest (USA) (Disc 1).chd", "Legacy Quest (USA) (Disc 2).chd"}
	body := "Legacy Quest (USA)/" + discs[0] + "\n" + "Legacy Quest (USA)/" + discs[1] + "\n"
	m3u, oldDir := migGame(t, base, body, discs, nil /* every disc a stub */, true)

	migrateLegacyM3U(m3u)

	if fi, err := os.Stat(m3u); err != nil || fi.Size() != 0 {
		t.Errorf("all-stubs playlist should normalize to the 0-byte stub shape (size=%v err=%v)", fi, err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("legacy folder not dot-migrated on the all-stubs card (err %v)", err)
	}
	e, ok := platform.LoadManifest().Entry(m3u)
	if !ok || len(e.Discs) != 2 {
		t.Fatalf("canonical list not seeded before the stub-shape rewrite: %+v ok=%v", e, ok)
	}
	if e.Discs[0] != ".Legacy Quest (USA)/"+discs[0] {
		t.Errorf("seeded discs[0] = %q, want the dot line", e.Discs[0])
	}
}

func TestMigrateLegacyM3U_SeedsAlreadyLocalOnly(t *testing.T) {
	base := migEnv(t)
	discs := []string{"Legacy Quest (USA) (Disc 1).chd", "Legacy Quest (USA) (Disc 2).chd"}
	body := "Legacy Quest (USA)/" + discs[0] + "\n" + "Legacy Quest (USA)/" + discs[1] + "\n"
	m3u, _ := migGame(t, base, body, discs, map[int]bool{0: true, 1: true}, true)

	migrateLegacyM3U(m3u)

	// The only change is the dot prefix (folder hidden); the local-only pass then
	// leaves the complete playlist alone.
	want := ".Legacy Quest (USA)/" + discs[0] + "\n" + ".Legacy Quest (USA)/" + discs[1] + "\n"
	if got, err := os.ReadFile(m3u); err != nil || string(got) != want {
		t.Errorf("complete playlist = %q (err %v), want dot lines only", string(got), err)
	}
	e, ok := platform.LoadManifest().Entry(m3u)
	if !ok || len(e.Discs) != 2 || e.Discs[0] != ".Legacy Quest (USA)/"+discs[0] {
		t.Errorf("canonical list not seeded (dot) from a complete playlist: %+v ok=%v", e, ok)
	}
}

// The stranded-behind-a-stub shape: an interrupted LEGACY download left a 0-byte
// .m3u stub and real discs in the visible non-dot folder. The dot leg must still
// migrate the folder (the coming downloadMultiDiscCore computes the DOT discDir and
// must FIND those bytes — not re-pull them and strand the old folder forever).
func TestMigrateLegacyM3U_DotMigratesBehindStub(t *testing.T) {
	base := migEnv(t)
	discs := []string{"Legacy Quest (USA) (Disc 1).chd", "Legacy Quest (USA) (Disc 2).chd"}
	m3u, oldDir := migGame(t, base, "", discs, map[int]bool{0: true}, true) // 0-byte stub m3u
	dotDir := filepath.Join(filepath.Dir(oldDir), "."+filepath.Base(oldDir))

	migrateLegacyM3U(m3u)

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("legacy folder not migrated behind a stub .m3u (err %v)", err)
	}
	if fi, err := os.Stat(filepath.Join(dotDir, discs[0])); err != nil || fi.Size() == 0 {
		t.Errorf("real disc bytes did not move to the dot folder (err %v)", err)
	}
	if fi, err := os.Stat(m3u); err != nil || fi.Size() != 0 {
		t.Errorf("stub .m3u should stay a 0-byte stub (size=%v err=%v)", fi, err)
	}
	if !platform.LoadManifest().OwnsKind(dotDir, platform.ManifestFolder) {
		t.Errorf("manifest folder ownership did not follow the dot rename")
	}
}

// Both layouts on card (a fresh dot download beside a stale legacy folder, or a
// crash between rename and rewrite): MERGE, dot copy wins, legacy remainder swept.
func TestMigrateLegacyM3U_MergesPreferringDot(t *testing.T) {
	base := migEnv(t)
	discs := []string{"Legacy Quest (USA) (Disc 1).chd", "Legacy Quest (USA) (Disc 2).chd"}
	body := "Legacy Quest (USA)/" + discs[0] + "\n" + "Legacy Quest (USA)/" + discs[1] + "\n"
	m3u, oldDir := migGame(t, base, body, discs, map[int]bool{0: true, 1: true}, true)
	dotDir := filepath.Join(filepath.Dir(oldDir), "."+filepath.Base(oldDir))
	if err := os.MkdirAll(dotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The dot folder already holds its own disc 1 — that copy must WIN.
	if err := os.WriteFile(filepath.Join(dotDir, discs[0]), []byte("DOTCOPY"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrateLegacyM3U(m3u)

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("legacy folder not swept after merge (err %v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(dotDir, discs[0])); err != nil || string(got) != "DOTCOPY" {
		t.Errorf("merge did not prefer the dot copy: %q (err %v)", string(got), err)
	}
	if fi, err := os.Stat(filepath.Join(dotDir, discs[1])); err != nil || fi.Size() == 0 {
		t.Errorf("disc missing from the dot folder was not moved in (err %v)", err)
	}
	want := ".Legacy Quest (USA)/" + discs[0] + "\n" + ".Legacy Quest (USA)/" + discs[1] + "\n"
	if got, err := os.ReadFile(m3u); err != nil || string(got) != want {
		t.Errorf("m3u after merge = %q (err %v), want dot lines", string(got), err)
	}
}
