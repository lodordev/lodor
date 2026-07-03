package ui

// Text rendering with the embedded 8×8 bitmap font (font8x8.go). Glyphs scale by an
// integer factor so text is crisp on the 720×480 H700 panel. bit0 of each font row is
// the LEFTMOST pixel (font8x8 is LSB-first).

const glyphW, glyphH = 8, 8

// DrawChar draws one ASCII glyph at (x,y) scaled by s. Pixels are drawn only where the
// glyph is set (transparent background).
func (c *Canvas) DrawChar(x, y int, ch byte, col Color, s int) {
	if s < 1 {
		s = 1
	}
	if int(ch) >= len(Font8x8) {
		ch = '?'
	}
	g := Font8x8[ch]
	for row := 0; row < glyphH; row++ {
		bits := g[row]
		for colBit := 0; colBit < glyphW; colBit++ {
			if bits&(1<<uint(colBit)) != 0 {
				c.FillRect(x+colBit*s, y+row*s, s, s, col)
			}
		}
	}
}

// TextWidth returns the pixel width of s rendered at scale sc (1px tracking between glyphs).
func TextWidth(text string, sc int) int {
	if sc < 1 {
		sc = 1
	}
	if len(text) == 0 {
		return 0
	}
	return len(text)*(glyphW*sc+sc) - sc
}

// DrawText draws a string at (x,y) at scale sc with 1*sc px between glyphs. Returns the x
// just past the last glyph.
func (c *Canvas) DrawText(x, y int, text string, col Color, sc int) int {
	if sc < 1 {
		sc = 1
	}
	cx := x
	for i := 0; i < len(text); i++ {
		c.DrawChar(cx, y, text[i], col, sc)
		cx += glyphW*sc + sc
	}
	return cx
}

// DrawTextCentered draws text horizontally centered in [x, x+w) at vertical y.
func (c *Canvas) DrawTextCentered(x, y, w int, text string, col Color, sc int) {
	tw := TextWidth(text, sc)
	c.DrawText(x+(w-tw)/2, y, text, col, sc)
}

// DrawTextWrapped draws text word-wrapped within maxW, returning the y past the last line.
func (c *Canvas) DrawTextWrapped(x, y, maxW int, text string, col Color, sc int) int {
	lineH := glyphH*sc + 4*sc
	word := ""
	line := ""
	flush := func() {
		if line != "" {
			c.DrawText(x, y, line, col, sc)
			y += lineH
			line = ""
		}
	}
	emit := func(w string) {
		if w == "" {
			return
		}
		try := w
		if line != "" {
			try = line + " " + w
		}
		if TextWidth(try, sc) <= maxW {
			line = try
		} else {
			flush()
			line = w
		}
	}
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch == '\n' {
			emit(word)
			word = ""
			flush()
			continue
		}
		if ch == ' ' {
			emit(word)
			word = ""
			continue
		}
		word += string(ch)
	}
	emit(word)
	flush()
	return y
}
