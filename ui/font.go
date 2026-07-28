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

// FitText returns s shortened with a trailing "..." so it renders no wider than maxW at
// scale sc; text that already fits comes back untouched. maxW <= 0 means "unbounded".
//
// Canvas.Set is bounds-checked, so DrawText clips an over-wide string SILENTLY — no error,
// no marker, just missing characters. Every single-line string the chrome draws must
// therefore be measured against the box it lives in. The field bug that forced this
// (lodor-muos#2): a 51-character pairing-code hint drawn on the fixed 720px compose canvas
// lost its last 12 characters, so the setup screen read "Settings > De" on EVERY device.
// The launch card already carried a private copy of this helper (task #73, same bug class);
// this is the shared one the chrome uses.
func FitText(s string, maxW, sc int) string {
	if maxW <= 0 || TextWidth(s, sc) <= maxW {
		return s
	}
	const ell = "..."
	for n := len(s) - 1; n >= 0; n-- {
		if TextWidth(s[:n]+ell, sc) <= maxW {
			return s[:n] + ell
		}
	}
	// Not even "..." fits: the longest bare prefix that does.
	for n := len(s); n >= 0; n-- {
		if TextWidth(s[:n], sc) <= maxW {
			return s[:n]
		}
	}
	return ""
}

// FitTextTail is FitText anchored to the END of s ("...our-server.example" rather than
// "https://my-really..."). Live text entry uses it: while typing a server URL the caret and
// the characters just entered are the ones that must stay on screen.
func FitTextTail(s string, maxW, sc int) string {
	if maxW <= 0 || TextWidth(s, sc) <= maxW {
		return s
	}
	const ell = "..."
	for n := 1; n <= len(s); n++ {
		if TextWidth(ell+s[n:], sc) <= maxW {
			return ell + s[n:]
		}
	}
	for n := 0; n <= len(s); n++ {
		if TextWidth(s[n:], sc) <= maxW {
			return s[n:]
		}
	}
	return ""
}

// FitScale picks the largest scale in [min, max] at which s fits maxW, falling back to min
// when nothing fits (the caller then ellipsizes at min). Used for the title bar, where a
// long name is better rendered a size smaller than chopped in half — the bar height stays
// pinned to the theme's TitleScale either way, so the layout below never moves.
func FitScale(s string, maxW, max, min int) int {
	if min < 1 {
		min = 1
	}
	for sc := max; sc > min; sc-- {
		if TextWidth(s, sc) <= maxW {
			return sc
		}
	}
	return min
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

// WrapText breaks text into lines that each render no wider than maxW at scale sc, splitting
// on spaces and honoring '\n'. A single word longer than maxW still gets its own line — it is
// ellipsized at draw time rather than silently overflowing.
func WrapText(text string, maxW, sc int) []string {
	var lines []string
	word, line := "", ""
	flush := func() {
		if line != "" {
			lines = append(lines, line)
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
		switch ch := text[i]; ch {
		case '\n':
			emit(word)
			word = ""
			flush()
		case ' ':
			emit(word)
			word = ""
		default:
			word += string(ch)
		}
	}
	emit(word)
	flush()
	return lines
}

// LineHeight is the baseline-to-baseline step DrawTextWrapped uses at scale sc.
func LineHeight(sc int) int { return glyphH*sc + 4*sc }

// WrappedHeight is the height DrawTextWrapped will consume — the measure-only twin of the
// draw call, so a caller can lay out around wrapped text without rendering it first.
func WrappedHeight(text string, maxW, sc int) int {
	return len(WrapText(text, maxW, sc)) * LineHeight(sc)
}

// DrawTextWrapped draws text word-wrapped within maxW, returning the y past the last line.
// Each line is additionally ellipsized to maxW so an unbreakable word (a long URL) is
// visibly cut with "..." instead of silently running off the canvas.
func (c *Canvas) DrawTextWrapped(x, y, maxW int, text string, col Color, sc int) int {
	lineH := LineHeight(sc)
	for _, line := range WrapText(text, maxW, sc) {
		c.DrawText(x, y, FitText(line, maxW, sc), col, sc)
		y += lineH
	}
	return y
}
