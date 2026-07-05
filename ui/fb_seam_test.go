//go:build linux

package ui

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// TestFileBackedFramebuffer exercises the off-hardware fb test seam end to end: LODOR_FB_DEV
// overrides the device to a plain file, the synthesized geometry drives a real mmap + blit,
// and SavePNG reads the actually-blitted bytes back through the device pixel format. The
// production path (env unset -> /dev/fb0 + real ioctls) is untouched by this.
func TestFileBackedFramebuffer(t *testing.T) {
	dir := t.TempDir()
	fbFile := filepath.Join(dir, "fb.raw")
	t.Setenv("LODOR_FB_DEV", fbFile)
	t.Setenv("LODOR_FB_GEOM", "720x480x32") // the REAL RG34XX panel (not 640 — see field log 2026-07-04)

	fb, err := OpenFramebuffer("/dev/fb0") // dev arg is overridden by LODOR_FB_DEV
	if err != nil {
		t.Fatalf("OpenFramebuffer (file-backed): %v", err)
	}
	defer fb.Close()
	if fb.Xres() != 720 || fb.Yres() != 480 || fb.Bpp() != 32 {
		t.Fatalf("geometry = %dx%d %dbpp, want 720x480 32bpp", fb.Xres(), fb.Yres(), fb.Bpp())
	}

	// Blit a known pixel and read it back through pack/unpack (exact at 32bpp).
	c := NewCanvas(fb.Xres(), fb.Yres())
	const red = Color(0xFF0000)
	c.Set(10, 20, red)
	fb.Flush(c)

	pngPath := filepath.Join(dir, "frame.png")
	if err := fb.SavePNG(pngPath); err != nil {
		t.Fatalf("SavePNG: %v", err)
	}
	f, err := os.Open(pngPath)
	if err != nil {
		t.Fatalf("open png: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 720 || b.Dy() != 480 {
		t.Fatalf("png bounds = %v, want 720x480", b)
	}
	r, g, bl, _ := img.At(10, 20).RGBA()
	if r>>8 != 0xFF || g>>8 != 0x00 || bl>>8 != 0x00 {
		t.Fatalf("blitted pixel = (%d,%d,%d), want (255,0,0) — pack/Flush/mmap/unpack roundtrip broken", r>>8, g>>8, bl>>8)
	}
}

// TestFileBackedFramebufferGeomOverride confirms LODOR_FB_GEOM drives a different panel
// (incl. a 16bpp RGB565 backend), covering the non-default bitfield path.
func TestFileBackedFramebufferGeomOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LODOR_FB_DEV", filepath.Join(dir, "fb.raw"))
	t.Setenv("LODOR_FB_GEOM", "320x240x16")
	fb, err := OpenFramebuffer("/dev/fb0")
	if err != nil {
		t.Fatalf("OpenFramebuffer 16bpp: %v", err)
	}
	defer fb.Close()
	if fb.Xres() != 320 || fb.Yres() != 240 || fb.Bpp() != 16 {
		t.Fatalf("geometry = %dx%d %dbpp, want 320x240 16bpp", fb.Xres(), fb.Yres(), fb.Bpp())
	}
	c := NewCanvas(fb.Xres(), fb.Yres())
	c.Set(5, 5, Color(0xFFFFFF))
	fb.Flush(c) // must not panic / SIGBUS (file sized to smem_len)
}

// TestFramebufferPageFlipPan reproduces the RG34XX display-handoff bug (2026-07-04) off-hardware:
// the H700 fbdev is DOUBLE-BUFFERED (yres_virtual == 2*yres) and muOS shows a page via
// FBIOPAN_DISPLAY. When muOS hands the app the screen with the panel panned to PAGE 1
// (yoffset == yres), a Flush that only writes page 0 lands OFF-SCREEN — the menu is drawn but
// the panel keeps scanning muOS's stale "Loading Application" frame. SavePNG (which reads page 0,
// where we wrote) can't catch this — the gap that let the bug ship. The fix: Flush must pan the
// page it drew (0) to be the page displayed. This asserts that pan intent explicitly.
func TestFramebufferPageFlipPan(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LODOR_FB_DEV", filepath.Join(dir, "fb.raw"))
	t.Setenv("LODOR_FB_GEOM", "720x480x32")
	t.Setenv("LODOR_FB_YVIRT", "960") // two stacked 480px pages — double-buffered panel
	t.Setenv("LODOR_FB_YOFF", "480")  // muOS left the panel scanning PAGE 1 at handoff

	fb, err := OpenFramebuffer("/dev/fb0")
	if err != nil {
		t.Fatalf("OpenFramebuffer (page-flip): %v", err)
	}
	defer fb.Close()
	if fb.YresVirtual() != 960 {
		t.Fatalf("yres_virtual = %d, want 960 (double-buffer not modeled)", fb.YresVirtual())
	}
	if fb.PanYOffset() != 480 {
		t.Fatalf("starting yoffset = %d, want 480 (test must open ON page 1 to be meaningful)", fb.PanYOffset())
	}

	c := NewCanvas(fb.Xres(), fb.Yres())
	c.Set(3, 4, Color(0xFF0000))
	fb.Flush(c)

	// THE regression guard: after blitting page 0, Flush must have panned the panel to yoffset=0
	// so the drawn page is the shown page. If this is 480 (unchanged), a page-flipped panel would
	// scan out the WRONG page and the frame is invisible — exactly the on-device symptom.
	if got := fb.LastPan(); got != 0 {
		t.Fatalf("after Flush, panel still panned to yoffset=%d; Flush must FBIOPAN_DISPLAY to the page it drew (0) — page-flip display-handoff regression", got)
	}

	// And the pixel really is on page 0 where we drew it.
	pngPath := filepath.Join(dir, "frame.png")
	if err := fb.SavePNG(pngPath); err != nil {
		t.Fatalf("SavePNG: %v", err)
	}
	f, err := os.Open(pngPath)
	if err != nil {
		t.Fatalf("open png: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	r, g, b, _ := img.At(3, 4).RGBA()
	if r>>8 != 0xFF || g>>8 != 0x00 || b>>8 != 0x00 {
		t.Fatalf("page-0 pixel = (%d,%d,%d), want (255,0,0)", r>>8, g>>8, b>>8)
	}
}
