package ui

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadImageCanvasRoundTrip: SavePNG then LoadImageCanvas returns the same
// pixels — the .media cover convention (PNG) decodes into a Canvas verbatim.
func TestLoadImageCanvasRoundTrip(t *testing.T) {
	src := NewCanvas(8, 4)
	src.Clear(0x123456)
	src.Set(3, 2, 0xff0000)
	p := filepath.Join(t.TempDir(), "cover.png")
	if err := src.SavePNG(p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadImageCanvas(p)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.W != 8 || got.H != 4 {
		t.Fatalf("size = %dx%d", got.W, got.H)
	}
	if got.Pix[2*8+3] != 0xff0000 || got.Pix[0] != 0x123456 {
		t.Fatalf("pixels: %06x %06x", got.Pix[2*8+3], got.Pix[0])
	}
}

// TestLoadImageCanvasBadFile: missing, non-PNG, and truncated-PNG inputs all
// return errors — never panic, never a half-decoded fake success. This is the
// launch-path guarantee: a corrupt cover degrades to the placeholder.
func TestLoadImageCanvasBadFile(t *testing.T) {
	if _, err := LoadImageCanvas(filepath.Join(t.TempDir(), "absent.png")); err == nil {
		t.Fatal("missing file must error")
	}
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.png")
	if err := os.WriteFile(junk, []byte("not a png at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImageCanvas(junk); err == nil {
		t.Fatal("junk bytes must error")
	}
	// Truncated: valid header, chopped body.
	full := NewCanvas(32, 32)
	fp := filepath.Join(dir, "full.png")
	if err := full.SavePNG(fp); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(fp)
	trunc := filepath.Join(dir, "trunc.png")
	if err := os.WriteFile(trunc, b[:len(b)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImageCanvas(trunc); err == nil {
		t.Fatal("truncated stream must error")
	}
}

// TestBlitScaledFitsAndCenters: a 2:1 source into a square box scales to the
// box width, centers vertically, and never writes outside the box.
func TestBlitScaledFitsAndCenters(t *testing.T) {
	src := NewCanvas(20, 10)
	src.Clear(0x00ff00)
	dst := NewCanvas(60, 60)
	dst.Clear(0x000000)
	dst.BlitScaled(src, 10, 10, 40, 40)
	// Scaled to 40x20, centered: y in [20,40), x in [10,50).
	if dst.Pix[25*60+12] != 0x00ff00 {
		t.Fatal("inside the fitted rect must be source-colored")
	}
	if dst.Pix[12*60+12] != 0 || dst.Pix[45*60+12] != 0 {
		t.Fatal("letterbox bands must stay untouched")
	}
	if dst.Pix[25*60+5] != 0 || dst.Pix[25*60+55] != 0 {
		t.Fatal("outside the box must stay untouched")
	}
	// Degenerate inputs are no-ops, not panics.
	dst.BlitScaled(nil, 0, 0, 10, 10)
	dst.BlitScaled(NewCanvas(1, 1), 0, 0, 0, 0)
}

// TestShoulderButtons: the new logical L1/R1 buttons exist, stringify, and the
// evdev keymap routes BTN_TL/BTN_TR to them (launch card action-row movement).
func TestShoulderButtons(t *testing.T) {
	if BtnL1.String() != "L1" || BtnR1.String() != "R1" {
		t.Fatalf("String: %q %q", BtnL1.String(), BtnR1.String())
	}
	if defaultKeyMap[310] != BtnL1 || defaultKeyMap[311] != BtnR1 {
		t.Fatal("BTN_TL/BTN_TR must map to L1/R1")
	}
}
