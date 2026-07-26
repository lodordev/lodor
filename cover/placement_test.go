// Placement tests: the ES-DE frontend-media path shape, the dim styling, the
// skip-existing/force contract at an explicit destination, and offline in-place
// dimming (evict). The NextUI-convention behavior is covered by cover_test.go —
// these prove the new seam without touching it.
package cover

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// testPNG encodes a solid-color image so brightness assertions are exact.
func testPNG(t *testing.T, c color.NRGBA, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// meanRed decodes a PNG and returns the mean red channel — enough to tell a dimmed
// solid-color cover from a bright one.
func meanRed(t *testing.T, raw []byte) int {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	var sum, n int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, _, _, _ := img.At(x, y).RGBA()
			sum += int(r >> 8)
			n++
		}
	}
	return sum / n
}

type placeDL struct{ body []byte }

func (d placeDL) DownloadCover(string) ([]byte, error) { return d.body, nil }

// TestEsdeCoverPathShape: <root>/<system-folder>/covers/<stem>.png, keyed on the
// ROM's parent folder and filename stem — including the .m3u multi-disc anchor.
func TestEsdeCoverPathShape(t *testing.T) {
	got := EsdeCoverPath("/sd/ES-DE/downloaded_media", "/sd/ROMs/gba/Golden Sun (USA).gba")
	want := filepath.Join("/sd/ES-DE/downloaded_media", "gba", "covers", "Golden Sun (USA).png")
	if got != want {
		t.Fatalf("single-file: got %q want %q", got, want)
	}
	got = EsdeCoverPath("/sd/ES-DE/downloaded_media", "/sd/ROMs/psx/Thousand Arms (USA).m3u")
	want = filepath.Join("/sd/ES-DE/downloaded_media", "psx", "covers", "Thousand Arms (USA).png")
	if got != want {
		t.Fatalf("m3u: got %q want %q", got, want)
	}
}

// TestFetchAndPlaceBrightVsDim: the same source cover placed bright then dim — the
// dim copy must be substantially darker, both at the explicit destination.
func TestFetchAndPlaceBrightVsDim(t *testing.T) {
	dir := t.TempDir()
	src := testPNG(t, color.NRGBA{R: 200, G: 180, B: 160, A: 255}, 40, 40)
	dl := placeDL{body: src}

	bright := filepath.Join(dir, "sys", "covers", "Game.png")
	if out, err := FetchAndPlace(dl, "cv", Placement{Dest: bright}, false); out != OutcomeSaved || err != nil {
		t.Fatalf("bright place: out=%v err=%v", out, err)
	}
	dim := filepath.Join(dir, "sys", "covers", "Game2.png")
	if out, err := FetchAndPlace(dl, "cv", Placement{Dest: dim, Dim: true}, false); out != OutcomeSaved || err != nil {
		t.Fatalf("dim place: out=%v err=%v", out, err)
	}

	brightRaw, _ := os.ReadFile(bright)
	dimRaw, _ := os.ReadFile(dim)
	mb, md := meanRed(t, brightRaw), meanRed(t, dimRaw)
	if mb != 200 {
		t.Fatalf("bright mean=%d want 200 (pass-through)", mb)
	}
	// 200 * 45% = 90; allow ±2 for rounding.
	if md < 88 || md > 92 {
		t.Fatalf("dim mean=%d want ~90", md)
	}
}

// TestFetchAndPlaceSkipAndForce: an existing destination is skipped without a
// network call unless force — force overwrites (the dim→bright transition).
func TestFetchAndPlaceSkipAndForce(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "covers", "Game.png")
	dl := placeDL{body: testPNG(t, color.NRGBA{R: 100, A: 255}, 8, 8)}
	if out, _ := FetchAndPlace(dl, "cv", Placement{Dest: dest}, false); out != OutcomeSaved {
		t.Fatalf("first place: %v", out)
	}
	if out, _ := FetchAndPlace(dl, "cv", Placement{Dest: dest}, false); out != OutcomeSkipped {
		t.Fatalf("second place should skip: %v", out)
	}
	dl2 := placeDL{body: testPNG(t, color.NRGBA{R: 220, A: 255}, 8, 8)}
	if out, _ := FetchAndPlace(dl2, "cv", Placement{Dest: dest}, true); out != OutcomeSaved {
		t.Fatalf("forced place: %v", out)
	}
	raw, _ := os.ReadFile(dest)
	if m := meanRed(t, raw); m != 220 {
		t.Fatalf("forced overwrite mean=%d want 220", m)
	}
}

// TestDimFileInPlace: offline dim of an already-placed cover (evict) darkens the
// file atomically at the same path.
func TestDimFileInPlace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Game.png")
	if err := os.WriteFile(p, testPNG(t, color.NRGBA{R: 200, G: 200, B: 200, A: 255}, 16, 16), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := DimFileInPlace(p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if m := meanRed(t, raw); m < 88 || m > 92 {
		t.Fatalf("dimmed mean=%d want ~90", m)
	}
}

// TestDimPreservesAlpha: transparent pixels stay transparent; opaque alpha is kept.
func TestDimPreservesAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 100, G: 100, B: 100, A: 255})
	img.SetNRGBA(1, 0, color.NRGBA{}) // fully transparent
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out, err := dimPNG(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, a0 := got.At(0, 0).RGBA()
	_, _, _, a1 := got.At(1, 0).RGBA()
	if a0>>8 != 255 {
		t.Fatalf("opaque alpha lost: %d", a0>>8)
	}
	if a1 != 0 {
		t.Fatalf("transparent pixel gained alpha: %d", a1)
	}
}
