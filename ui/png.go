package ui

import (
	"image"
	"image/color"
	"image/png"
	"os"
)

// SavePNG writes the canvas to a PNG file. This is the OFF-HARDWARE verification backend:
// every wizard screen can be rendered and visually inspected without an RG34XX. Stdlib
// image/png only.
func (c *Canvas) SavePNG(path string) error {
	img := image.NewRGBA(image.Rect(0, 0, c.W, c.H))
	for y := 0; y < c.H; y++ {
		for x := 0; x < c.W; x++ {
			r, g, b := c.Pix[y*c.W+x].rgb()
			img.SetRGBA(x, y, color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 0xff})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
