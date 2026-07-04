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
	t.Setenv("LODOR_FB_GEOM", "640x480x32")

	fb, err := OpenFramebuffer("/dev/fb0") // dev arg is overridden by LODOR_FB_DEV
	if err != nil {
		t.Fatalf("OpenFramebuffer (file-backed): %v", err)
	}
	defer fb.Close()
	if fb.Xres() != 640 || fb.Yres() != 480 || fb.Bpp() != 32 {
		t.Fatalf("geometry = %dx%d %dbpp, want 640x480 32bpp", fb.Xres(), fb.Yres(), fb.Bpp())
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
	if b := img.Bounds(); b.Dx() != 640 || b.Dy() != 480 {
		t.Fatalf("png bounds = %v, want 640x480", b)
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
