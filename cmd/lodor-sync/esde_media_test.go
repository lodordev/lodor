package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lodor/platform"
)

// tinyPNG encodes a small solid-color PNG so cover.Dim has real bytes to transform.
func tinyPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeOwnedROM drops roms/<system>/<base> plus its .media/<stem>.png cover.
func writeOwnedROM(t *testing.T, romsRoot, system, base string, cover []byte) {
	t.Helper()
	dir := filepath.Join(romsRoot, system)
	if err := os.MkdirAll(filepath.Join(dir, ".media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base), []byte("ROM"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cover != nil {
		stem := base[:len(base)-len(filepath.Ext(base))]
		if err := os.WriteFile(filepath.Join(dir, ".media", stem+".png"), cover, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconcileFrontendMedia(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LODOR_PAK_DIR", filepath.Join(tmp, "pak"))
	romsRoot := filepath.Join(tmp, "roms")

	art := tinyPNG(t, color.NRGBA{200, 60, 60, 255})
	// ✓ on-device owned game with a cover → normal cover + clean title.
	dl := platform.MarkerOnDevice + "Zelda (USA).z64"
	writeOwnedROM(t, romsRoot, "n64", dl, art)
	// ✘ cloud stub owned game with a cover → DIMMED cover.
	stub := platform.MarkerCloud + "Mario (USA).sfc"
	writeOwnedROM(t, romsRoot, "snes", stub, art)
	// A user's own ROM (no marker) → left entirely alone.
	writeOwnedROM(t, romsRoot, "n64", "MyHomebrew.z64", art)

	esde := frontendTarget{"esde", filepath.Join(tmp, "ES-DE")}
	cocoon := frontendTarget{"cocoon", filepath.Join(tmp, "Cocoon")}
	targets := []frontendTarget{esde, cocoon}

	man := platform.LoadManifest()
	games := scanOwnedGames(romsRoot)
	placed, gamelists, pruned := reconcileFrontendMedia(games, targets, man)
	// 2 covered owned games × 2 targets = 4 covers; 2 systems × 2 targets = 4 gamelists.
	if placed != 4 || gamelists != 4 || pruned != 0 {
		t.Fatalf("first run: placed=%d gamelists=%d pruned=%d, want 4/4/0", placed, gamelists, pruned)
	}

	// Downloaded game: NORMAL cover (bytes == source) in both targets.
	for _, tgt := range targets {
		p := filepath.Join(tgt.coversDir("n64"), platform.MarkerOnDevice+"Zelda (USA).png")
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: downloaded cover missing: %v", tgt.name, err)
		}
		if !bytes.Equal(got, art) {
			t.Errorf("%s: downloaded cover should be unmodified (normal)", tgt.name)
		}
	}
	// Stub game: DIMMED cover (valid PNG, bytes != source).
	stubCover := filepath.Join(esde.coversDir("snes"), platform.MarkerCloud+"Mario (USA).png")
	dim, err := os.ReadFile(stubCover)
	if err != nil {
		t.Fatalf("stub cover missing: %v", err)
	}
	if bytes.Equal(dim, art) {
		t.Errorf("stub cover should be dimmed, not identical to source")
	}
	if _, derr := png.Decode(bytes.NewReader(dim)); derr != nil {
		t.Errorf("dimmed cover is not a valid PNG: %v", derr)
	}
	// Unowned ROM: no cover placed anywhere.
	if _, err := os.Stat(filepath.Join(esde.coversDir("n64"), "MyHomebrew.png")); !os.IsNotExist(err) {
		t.Errorf("unowned ROM cover was placed — ownership leak")
	}

	// Clean title: gamelist <name> is marker-stripped, <path> keeps the marker.
	gl, err := os.ReadFile(esde.gamelistPath("n64"))
	if err != nil {
		t.Fatalf("gamelist missing: %v", err)
	}
	body := string(gl)
	if !strings.Contains(body, "<name>Zelda (USA)</name>") {
		t.Errorf("gamelist <name> not clean:\n%s", body)
	}
	if strings.Contains(body, "<name>"+platform.MarkerOnDevice) {
		t.Errorf("marker leaked into gamelist <name>:\n%s", body)
	}
	if !strings.Contains(body, "<path>./"+platform.MarkerOnDevice+"Zelda (USA).z64</path>") {
		t.Errorf("gamelist <path> should keep the marked on-disk name:\n%s", body)
	}

	// Idempotent: a second unchanged run writes nothing.
	if p, g, pr := reconcileFrontendMedia(scanOwnedGames(romsRoot), targets, man); p != 0 || g != 0 || pr != 0 {
		t.Fatalf("idempotent run: placed=%d gamelists=%d pruned=%d, want 0/0/0", p, g, pr)
	}

	// Removal: the downloaded ROM leaves → its covers prune in BOTH targets; a
	// user-scraped cover in the same tree is untouched.
	userScraped := filepath.Join(esde.coversDir("n64"), "SomeUserGame.png")
	if err := os.WriteFile(userScraped, []byte("USER"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(romsRoot, "n64", dl)); err != nil {
		t.Fatal(err)
	}
	_, _, pruned = reconcileFrontendMedia(scanOwnedGames(romsRoot), targets, man)
	if pruned != 2 {
		t.Fatalf("after removal: pruned=%d, want 2 (one per target)", pruned)
	}
	if _, err := os.Stat(filepath.Join(esde.coversDir("n64"), platform.MarkerOnDevice+"Zelda (USA).png")); !os.IsNotExist(err) {
		t.Errorf("orphaned cover not pruned")
	}
	if _, err := os.Stat(userScraped); err != nil {
		t.Errorf("user-scraped cover was pruned — must never touch non-Lodor media")
	}
}
