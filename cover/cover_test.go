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

// makePNG builds a w×h solid-color PNG for scaler tests.
func makePNG(w, h int, c color.Color) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// TestMediaPath locks the NextUI convention: .media/<basename-without-ext>.png beside
// the ROM, for both single-file and multi-file (.m3u) ROM paths.
func TestMediaPath(t *testing.T) {
	cases := map[string]string{
		"/mnt/SDCARD/Roms/GB/Tetris (USA).gb":   "/mnt/SDCARD/Roms/GB/.media/Tetris (USA).png",
		"/mnt/SDCARD/Roms/PS/Final Fantasy.m3u": "/mnt/SDCARD/Roms/PS/.media/Final Fantasy.png",
		"/mnt/SDCARD/Roms/FC/Game.no.dots.nes":  "/mnt/SDCARD/Roms/FC/.media/Game.no.dots.png",
	}
	for in, want := range cases {
		if got := MediaPath(in); got != want {
			t.Errorf("MediaPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestScaleDown verifies an OVERSIZED cover (the large 705x700 fallback) is reduced so
// its long edge == MaxEdge while aspect ratio is preserved, and the output is a valid PNG.
func TestScaleDown(t *testing.T) {
	raw := makePNG(705, 700, color.NRGBA{R: 200, G: 100, B: 50, A: 255})
	out, err := scalePNG(raw, MaxEdge)
	if err != nil {
		t.Fatalf("scalePNG: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode scaled: %v", err)
	}
	if cfg.Width != MaxEdge {
		t.Errorf("scaled width = %d, want %d", cfg.Width, MaxEdge)
	}
	wantH := 700 * MaxEdge / 705
	if cfg.Height != wantH {
		t.Errorf("scaled height = %d, want %d", cfg.Height, wantH)
	}
}

// TestPassThrough verifies a cover already within MaxEdge (RomM's 282x280 small.png)
// is returned UNMODIFIED — same bytes, no re-encode.
func TestPassThrough(t *testing.T) {
	raw := makePNG(282, 280, color.NRGBA{R: 200, G: 100, B: 50, A: 255})
	out, err := scalePNG(raw, MaxEdge)
	if err != nil {
		t.Fatalf("scalePNG: %v", err)
	}
	if !bytes.Equal(out, raw) {
		t.Errorf("within-bounds cover was re-encoded (got %d bytes, src %d) — want pass-through", len(out), len(raw))
	}
}

// TestNoUpscale verifies a cover already smaller than MaxEdge is NOT enlarged.
func TestNoUpscale(t *testing.T) {
	raw := makePNG(64, 60, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	out, err := scalePNG(raw, MaxEdge)
	if err != nil {
		t.Fatalf("scalePNG: %v", err)
	}
	cfg, _, _ := image.DecodeConfig(bytes.NewReader(out))
	if cfg.Width != 64 || cfg.Height != 60 {
		t.Errorf("upscaled to %dx%d, want 64x60", cfg.Width, cfg.Height)
	}
}

// fakeDL is a coverDownloader stub returning canned bytes/errors.
type fakeDL struct {
	data []byte
	err  error
}

func (f fakeDL) DownloadCover(string) ([]byte, error) { return f.data, f.err }

// TestFetchAndSave covers the graceful contract: no-cover, skip-existing, and a
// successful save writing the .media PNG.
func TestFetchAndSave(t *testing.T) {
	dir := t.TempDir()
	romPath := filepath.Join(dir, "Roms", "GB", "Tetris.gb")
	_ = os.MkdirAll(filepath.Dir(romPath), 0o755)

	// no cover path -> OutcomeNoCover, nothing written
	if out, err := FetchAndSave(fakeDL{}, "", romPath); out != OutcomeNoCover || err != nil {
		t.Fatalf("no-cover: got out=%v err=%v", out, err)
	}
	if _, err := os.Stat(MediaPath(romPath)); err == nil {
		t.Fatalf("no-cover wrote a file")
	}

	// successful save
	raw := makePNG(282, 280, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	if out, err := FetchAndSave(fakeDL{data: raw}, "/assets/x/cover/small.png", romPath); out != OutcomeSaved || err != nil {
		t.Fatalf("save: got out=%v err=%v", out, err)
	}
	if !Exists(romPath) {
		t.Fatalf("cover not written")
	}

	// second call -> skip-existing
	if out, _ := FetchAndSave(fakeDL{data: raw}, "/assets/x/cover/small.png", romPath); out != OutcomeSkipped {
		t.Fatalf("expected skip-existing, got %v", out)
	}

	// download error -> OutcomeError, non-nil err, no panic
	romPath2 := filepath.Join(dir, "Roms", "GB", "Other.gb")
	if out, err := FetchAndSave(fakeDL{err: bytes.ErrTooLarge}, "/assets/y/cover/small.png", romPath2); out != OutcomeError || err == nil {
		t.Fatalf("error case: got out=%v err=%v", out, err)
	}
}
