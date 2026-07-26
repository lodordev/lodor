package ui

// Cover-art loading for the launch card (task launch-card-v2). Stdlib image/png
// only — the engine's CGO-free invariant holds. Decoding is PANIC-SAFE: box art
// is untrusted card content (user-replaceable, FAT32-corruptible) and it is
// drawn on the LAUNCH path, so a hostile or truncated file must degrade to the
// text placeholder, never crash the launch.

import (
	"fmt"
	"image"
	_ "image/png" // the .media convention is PNG (BLUEPRINT §11)
	"os"
)

// LoadImageCanvas decodes an image file into a Canvas. Any failure — missing
// file, bad magic, truncated stream, or a decoder panic — returns an error the
// caller renders as a placeholder. Alpha is composited over black (the card's
// hero box is dark; covers are opaque in practice).
func LoadImageCanvas(path string) (c *Canvas, err error) {
	defer func() {
		if r := recover(); r != nil {
			c, err = nil, fmt.Errorf("image decode panic: %v", r)
		}
	}()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	c = NewCanvas(b.Dx(), b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			// Premultiplied 16-bit → composite over black → 8-bit packed RGB.
			_ = a
			c.Set(x-b.Min.X, y-b.Min.Y,
				Color((r>>8)<<16|(g>>8)<<8|(bl>>8)))
		}
	}
	return c, nil
}

// BlitScaled draws src into the (x,y,w,h) box of c, scaled to FIT (aspect
// preserved, centered in the box), nearest-neighbor. Nearest is right for
// pixel-art box covers on a 720×480 panel and stays stdlib-only. Degenerate
// sources or boxes are a no-op — never a divide-by-zero on the launch path.
func (c *Canvas) BlitScaled(src *Canvas, x, y, w, h int) {
	if src == nil || src.W < 1 || src.H < 1 || w < 1 || h < 1 {
		return
	}
	// Fit: one scale factor, the smaller of the two axis ratios.
	dw, dh := w, src.H*w/src.W
	if dh > h {
		dh, dw = h, src.W*h/src.H
	}
	if dw < 1 || dh < 1 {
		return
	}
	ox, oy := x+(w-dw)/2, y+(h-dh)/2
	for dy := 0; dy < dh; dy++ {
		sy := dy * src.H / dh
		for dx := 0; dx < dw; dx++ {
			sx := dx * src.W / dw
			c.Set(ox+dx, oy+dy, src.Pix[sy*src.W+sx])
		}
	}
}
