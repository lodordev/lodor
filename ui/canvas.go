// Package ui is a pure-Go, CGO-free, stdlib-only immediate-mode UI for Lodor handhelds
// (muOS wizard, and any future fb0 front-end). CANONICAL COPY: integrations/onionos/menu
// vendors a copy of this package (lodor-menu builds outside the engine module); dedupe
// against this one at foldback.
// It renders to an in-memory Canvas which a backend blits to /dev/fb0 (on device) or
// dumps to PNG (off-hardware verification). Input comes from /dev/input/event* parsed
// with stdlib only. No SDL, no cgo - preserving the Lodor engine's CGO_ENABLED=0,
// stdlib-only invariant (Grout's SDL+gabagool path was rejected for exactly that reason).
package ui

// Color is a packed 0xRRGGBB value (the high byte is ignored).
type Color uint32

func (c Color) rgb() (r, g, b uint32) {
	return uint32(c>>16) & 0xff, uint32(c>>8) & 0xff, uint32(c) & 0xff
}

// Canvas is a software framebuffer: row-major RGB pixels, one uint32 (0xRRGGBB) each.
type Canvas struct {
	W, H int
	Pix  []Color
}

// NewCanvas allocates a w×h canvas (all black).
func NewCanvas(w, h int) *Canvas {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return &Canvas{W: w, H: h, Pix: make([]Color, w*h)}
}

// Set writes one pixel, bounds-checked (out-of-range is a no-op).
func (c *Canvas) Set(x, y int, col Color) {
	if x < 0 || y < 0 || x >= c.W || y >= c.H {
		return
	}
	c.Pix[y*c.W+x] = col
}

// Clear fills the whole canvas with col.
func (c *Canvas) Clear(col Color) {
	for i := range c.Pix {
		c.Pix[i] = col
	}
}

// FillRect fills the rectangle (x,y,w,h), clipped to the canvas.
func (c *Canvas) FillRect(x, y, w, h int, col Color) {
	x0, y0 := x, y
	x1, y1 := x+w, y+h
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > c.W {
		x1 = c.W
	}
	if y1 > c.H {
		y1 = c.H
	}
	for yy := y0; yy < y1; yy++ {
		row := yy * c.W
		for xx := x0; xx < x1; xx++ {
			c.Pix[row+xx] = col
		}
	}
}

// Rect draws a 1px outline rectangle.
func (c *Canvas) Rect(x, y, w, h int, col Color) {
	c.FillRect(x, y, w, 1, col)
	c.FillRect(x, y+h-1, w, 1, col)
	c.FillRect(x, y, 1, h, col)
	c.FillRect(x+w-1, y, 1, h, col)
}

// RectThick draws an outline of the given thickness (inset inward).
func (c *Canvas) RectThick(x, y, w, h, t int, col Color) {
	for i := 0; i < t; i++ {
		c.Rect(x+i, y+i, w-2*i, h-2*i, col)
	}
}
