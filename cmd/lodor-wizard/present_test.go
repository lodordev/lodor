package main

// present_test.go — off-hardware proof of the panel-scaling fix (launch-card-v2,
// TrimUI 2026-07-11 field bug: the 720×480 card blitted 1:1 into the top-left
// corner of the 1280×720 Smart Pro panel). Renders the SAME full card model the
// hook path builds (fxSaves/fxStates fixtures + a stand-in cover) through
// presentCanvas against a file-backed framebuffer (LODOR_FB_DEV/LODOR_FB_GEOM
// seam) at BOTH the Smart Pro panel (1280×720, up-scale + pillarbox) and the
// Miyoo/H700 panel (640×480, down-scale + letterbox), and reads the blitted
// frame back via SavePNG. Set LODOR_FBTEST_OUT=<dir> to keep the PNGs for
// eyeballing (the reflash gate); unset, they land in the test tempdir.

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"lodor/ui"
)

// fbFor opens a file-backed framebuffer at the given geometry.
func fbFor(t *testing.T, geom string) *ui.Framebuffer {
	t.Helper()
	t.Setenv("LODOR_FB_DEV", filepath.Join(t.TempDir(), "fb.raw"))
	t.Setenv("LODOR_FB_GEOM", geom)
	fb, err := ui.OpenFramebuffer("/dev/fb0") // dev overridden by LODOR_FB_DEV
	if err != nil {
		t.Fatalf("OpenFramebuffer(%s): %v", geom, err)
	}
	return fb
}

func fbtestOut(t *testing.T, name string) string {
	t.Helper()
	if d := os.Getenv("LODOR_FBTEST_OUT"); d != "" {
		return filepath.Join(d, name)
	}
	return filepath.Join(t.TempDir(), name)
}

// fullCardModel is the shared fixture card: saves + states + cover, playtime,
// multi-disc — every hero line populated so the dump is worth eyeballing.
func fullCardModel() (*cardModel, *ui.Canvas) {
	m := buildCardModel("/roms/PS/Legend of Dragoon.m3u", fxSaves, nil, fxStates, nil,
		true, 1, 3, "Played 4h 12m across 7 sessions")
	cov := ui.NewCanvas(120, 160) // stand-in box art: framed two-tone block
	cov.Clear(0x2266aa)
	cov.FillRect(10, 10, 100, 140, 0xddaa33)
	return m, cov
}

// dumpAndCheck presents the canvas, saves the PNG, and asserts the aspect-fit
// contract: panel-sized frame, pure-black letterbox bars, real content inside
// the fitted rect. cx/cy/cw/ch is the expected content rect.
func dumpAndCheck(t *testing.T, fb *ui.Framebuffer, c *ui.Canvas, out string, cx, cy, cw, ch int) {
	t.Helper()
	presentCanvas(fb, c)
	if err := fb.SavePNG(out); err != nil {
		t.Fatalf("SavePNG(%s): %v", out, err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	b := img.Bounds()
	if b.Dx() != fb.Xres() || b.Dy() != fb.Yres() {
		t.Fatalf("frame = %dx%d, want the panel %dx%d", b.Dx(), b.Dy(), fb.Xres(), fb.Yres())
	}
	black := func(x, y int) bool {
		r, g, bl, _ := img.At(x, y).RGBA()
		return r == 0 && g == 0 && bl == 0
	}
	// Letterbox bars must be untouched black (sampled, all four possible bars).
	for _, p := range [][2]int{
		{0, 0}, {cx - 1, fb.Yres() - 1}, {cx + cw, 0}, {fb.Xres() - 1, fb.Yres() - 1}, // pillar bars
		{fb.Xres() / 2, cy - 1}, {fb.Xres() / 2, cy + ch}, // letter bars
	} {
		x, y := p[0], p[1]
		if x < 0 || y < 0 || x >= fb.Xres() || y >= fb.Yres() {
			continue // that bar doesn't exist at this geometry
		}
		if cx <= x && x < cx+cw && cy <= y && y < cy+ch {
			continue // sample fell inside the content rect (bar absent)
		}
		if !black(x, y) {
			t.Fatalf("letterbox pixel (%d,%d) not black — scale/offset wrong", x, y)
		}
	}
	// The fitted rect must carry real content (text/panels), not emptiness.
	nonBlack := 0
	for y := cy; y < cy+ch; y += 4 {
		for x := cx; x < cx+cw; x += 4 {
			if !black(x, y) {
				nonBlack++
			}
		}
	}
	if nonBlack < 500 {
		t.Fatalf("content rect suspiciously empty (%d non-black samples) — card did not scale in", nonBlack)
	}
}

// TestPresentCanvasSmartPro1280x720: the Smart Pro panel. 720×480 (3:2) into
// 1280×720 (16:9) aspect-fits to 1080×720 centered — 100px pillar bars, full
// height. The PNG at LODOR_FBTEST_OUT/lcard-fbtest-1280x720.png is the
// eyeball artifact for the reflash decision.
func TestPresentCanvasSmartPro1280x720(t *testing.T) {
	fb := fbFor(t, "1280x720x32")
	defer fb.Close()
	m, cov := fullCardModel()
	dumpAndCheck(t, fb, renderCard(ui.DefaultTheme(), m, cov),
		fbtestOut(t, "lcard-fbtest-1280x720.png"), 100, 0, 1080, 720)
}

// TestPresentCanvasMiyoo640x480: the smaller-panel regression leg. 720×480
// into 640×480 fits to 640×426 centered — 27px top/bottom bars.
func TestPresentCanvasMiyoo640x480(t *testing.T) {
	fb := fbFor(t, "640x480x32")
	defer fb.Close()
	m, cov := fullCardModel()
	dumpAndCheck(t, fb, renderCard(ui.DefaultTheme(), m, cov),
		fbtestOut(t, "lcard-fbtest-640x480.png"), 0, 27, 640, 426)
}

// TestPresentCanvasIdentity720x480: a matching panel takes the unchanged
// direct-Flush path — byte-identical to pre-fix rendering (LodorOS/knulli
// lanes must not shift by a pixel).
func TestPresentCanvasIdentity720x480(t *testing.T) {
	fb := fbFor(t, "720x480x32")
	defer fb.Close()
	m, cov := fullCardModel()
	c := renderCard(ui.DefaultTheme(), m, cov)
	presentCanvas(fb, c)
	out := fbtestOut(t, "lcard-fbtest-720x480.png")
	if err := fb.SavePNG(out); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	// Exact roundtrip at 32bpp: every canvas pixel must land unscaled.
	for _, p := range [][2]int{{0, 0}, {359, 239}, {719, 479}, {17, 401}} {
		r, g, b, _ := img.At(p[0], p[1]).RGBA()
		got := ui.Color((r>>8)<<16 | (g>>8)<<8 | (b >> 8))
		want := c.Pix[p[1]*c.W+p[0]]
		if got != want {
			t.Fatalf("identity path shifted pixel %v: got %06x want %06x", p, got, want)
		}
	}
}

// TestPresentCanvasStatesSubView1280x720: a SUB-VIEW frame scales too — the
// sub-views share the card's draw closure, so one captured States frame at the
// Smart Pro geometry proves the whole card session presents panel-scaled.
func TestPresentCanvasStatesSubView1280x720(t *testing.T) {
	fb := fbFor(t, "1280x720x32")
	defer fb.Close()
	w := &wizard{t: ui.DefaultTheme()}
	m, _ := fullCardModel()
	out := fbtestOut(t, "lcard-fbtest-states-1280x720.png")
	var frames int
	draw := func(c *ui.Canvas) {
		presentCanvas(fb, c)
		frames++
	}
	btn := func() ui.Button { return ui.BtnBack } // one frame, then leave
	w.cardStatesView(m, draw, btn)
	if frames < 1 {
		t.Fatal("states view drew no frame")
	}
	if err := fb.SavePNG(out); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1280 || b.Dy() != 720 {
		t.Fatalf("states frame = %v, want 1280x720", b)
	}
	nonBlack := 0
	for y := 0; y < 720; y += 4 {
		for x := 100; x < 1180; x += 4 {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r|g|bl != 0 {
				nonBlack++
			}
		}
	}
	if nonBlack < 500 {
		t.Fatalf("states sub-view frame suspiciously empty (%d non-black samples)", nonBlack)
	}
}
