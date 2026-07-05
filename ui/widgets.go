package ui

import "strings"

// Higher-level widgets for the onboarding wizard: a Theme, a Menu (list select), an
// on-screen Keyboard (text entry - Wi-Fi password stays muOS's job, but server URL,
// pairing code, and device name are entered here), and chrome (header/footer/message/
// progress) helpers. Immediate-mode: the wizard owns the loop, calls Draw, feeds Buttons.

// Theme holds the palette + text scales tuned for the 720×480 H700 panel.
type Theme struct {
	Bg, Panel, Accent, Text, Dim, Good, Bad Color
	TitleScale, BodyScale, SmallScale       int
}

// DefaultTheme - dark with a RomM-ish violet accent.
func DefaultTheme() Theme {
	return Theme{
		Bg:     0x101018,
		Panel:  0x1c1c2c,
		Accent: 0x8b5cf6,
		Text:   0xf0f0f5,
		Dim:    0x8888a0,
		Good:   0x4ade80,
		Bad:    0xf87171,

		TitleScale: 4,
		BodyScale:  2,
		SmallScale: 2,
	}
}

// Frame draws the standard background + title bar + footer hint bar, returning the
// content rect (x,y,w,h) between them for the screen body.
func (t Theme) Frame(c *Canvas, title, hint string) (int, int, int, int) {
	c.Clear(t.Bg)
	// Title bar.
	barH := glyphH*t.TitleScale + 24
	c.FillRect(0, 0, c.W, barH, t.Panel)
	c.FillRect(0, barH, c.W, 3, t.Accent)
	c.DrawText(24, 12, title, t.Text, t.TitleScale)
	// Footer hint bar.
	footH := glyphH*t.SmallScale + 16
	fy := c.H - footH
	c.FillRect(0, fy, c.W, footH, t.Panel)
	c.DrawText(24, fy+8, hint, t.Dim, t.SmallScale)
	return 24, barH + 20, c.W - 48, fy - (barH + 20) - 12
}

// Menu is a vertical single-select list.
type Menu struct {
	Items []string
	sel   int
}

func (m *Menu) Selected() int  { return m.sel }
func (m *Menu) SelectedItem() string {
	if m.sel >= 0 && m.sel < len(m.Items) {
		return m.Items[m.sel]
	}
	return ""
}

// Handle moves the selection for Up/Down; returns true if Confirm was pressed.
func (m *Menu) Handle(b Button) (confirmed bool) {
	switch b {
	case BtnUp:
		if m.sel > 0 {
			m.sel--
		}
	case BtnDown:
		if m.sel < len(m.Items)-1 {
			m.sel++
		}
	case BtnConfirm, BtnStart:
		return true
	}
	return false
}

// Draw renders the menu within (x,y,w,h).
func (m *Menu) Draw(c *Canvas, t Theme, x, y, w, h int) {
	rowH := glyphH*t.BodyScale + 18
	for i, it := range m.Items {
		ry := y + i*rowH
		if ry+rowH > y+h {
			break
		}
		if i == m.sel {
			c.FillRect(x, ry, w, rowH, t.Panel)
			c.FillRect(x, ry, 6, rowH, t.Accent)
		}
		col := t.Text
		if i != m.sel {
			col = t.Dim
		}
		c.DrawText(x+22, ry+9, it, col, t.BodyScale)
	}
}

// ScrollMenu is a vertical single-select list that SCROLLS. Menu.Draw clips any row past
// its box (widgets.go), which is fine for a fixed 2-3 item choice but loses rows on a long
// list (Game Manager systems/games, the profile picker, the recent-activity feed). ScrollMenu
// keeps the selected row in view by sliding a fixed-height window over Items and draws honest
// up/down "more" markers so the user knows rows exist off-screen. Same immediate-mode contract
// as Menu: the caller owns the loop, calls Draw, feeds Handle one Button at a time.
type ScrollMenu struct {
	Items []string
	sel   int
	off   int // index of the first visible row
}

func (m *ScrollMenu) Selected() int { return m.sel }
func (m *ScrollMenu) SelectedItem() string {
	if m.sel >= 0 && m.sel < len(m.Items) {
		return m.Items[m.sel]
	}
	return ""
}

// Handle moves the selection for Up/Down (clamped, no wrap); returns true if Confirm/Start
// was pressed. Offset tracking happens in Draw (it needs the box height).
func (m *ScrollMenu) Handle(b Button) (confirmed bool) {
	switch b {
	case BtnUp:
		if m.sel > 0 {
			m.sel--
		}
	case BtnDown:
		if m.sel < len(m.Items)-1 {
			m.sel++
		}
	case BtnConfirm, BtnStart:
		return true
	}
	return false
}

// smRowH is the per-row height ScrollMenu and Menu share.
func smRowH(t Theme) int { return glyphH*t.BodyScale + 18 }

// VisibleRows reports how many rows fit in a box of height h (>=1). Exposed for tests.
func (m *ScrollMenu) VisibleRows(t Theme, h int) int {
	v := h / smRowH(t)
	if v < 1 {
		v = 1
	}
	return v
}

// Draw renders the visible window within (x,y,w,h), scrolling so the selection stays on
// screen, and draws ^ / v markers when items exist above / below the window.
func (m *ScrollMenu) Draw(c *Canvas, t Theme, x, y, w, h int) {
	rowH := smRowH(t)
	vis := m.VisibleRows(t, h)
	if m.sel < m.off {
		m.off = m.sel
	}
	if m.sel >= m.off+vis {
		m.off = m.sel - vis + 1
	}
	if m.off < 0 {
		m.off = 0
	}
	end := m.off + vis
	if end > len(m.Items) {
		end = len(m.Items)
	}
	for i := m.off; i < end; i++ {
		ry := y + (i-m.off)*rowH
		if i == m.sel {
			c.FillRect(x, ry, w, rowH, t.Panel)
			c.FillRect(x, ry, 6, rowH, t.Accent)
		}
		col := t.Dim
		if i == m.sel {
			col = t.Text
		}
		c.DrawText(x+22, ry+9, m.Items[i], col, t.BodyScale)
	}
	if m.off > 0 {
		c.DrawText(x+w-14, y+2, "^", t.Accent, t.BodyScale)
	}
	if end < len(m.Items) {
		c.DrawText(x+w-14, y+(vis-1)*rowH+9, "v", t.Accent, t.BodyScale)
	}
}

// ParseEngineResult condenses an engine mode's combined output into ONE honest line for a
// result screen. It prefers a structured trailer (RESULT/MIRROR/CONTINUE) or an explicit
// reason= line; failing that it returns the last non-empty line. It never fabricates a
// success message — an empty return means the caller should fall back to its own default
// (feedback_no_fake_ui_state: show the engine's real words, not an invented "OK").
func ParseEngineResult(output string) string {
	var last, structured string
	for _, ln := range strings.Split(output, "\n") {
		s := strings.TrimSpace(ln)
		if s == "" {
			continue
		}
		last = s
		if strings.HasPrefix(s, "RESULT ") || strings.HasPrefix(s, "MIRROR ") || strings.HasPrefix(s, "CONTINUE ") {
			structured = s
		}
	}
	if structured != "" {
		return structured
	}
	return last
}

// ResultToken extracts the value of key=<value> from an engine RESULT/reason line, or "" if
// the token is absent. Value is read up to the next space (tokens are space-separated).
func ResultToken(output, key string) string {
	for _, f := range strings.Fields(output) {
		if strings.HasPrefix(f, key+"=") {
			return f[len(key)+1:]
		}
	}
	return ""
}

// Keyboard is an on-screen text-entry grid driven by the d-pad + Confirm.
// Cancelled reports that the user chose BACK (the grid's BACK key) to leave the
// step without committing - the caller uses it to navigate one step backward, so
// the user is never trapped in a text field (blocker #170).
type Keyboard struct {
	Prompt    string
	Text      string
	Cancelled bool
	row       int
	col       int
	shift     bool
}

var kbGrid = [][]string{
	{"1", "2", "3", "4", "5", "6", "7", "8", "9", "0"},
	{"q", "w", "e", "r", "t", "y", "u", "i", "o", "p"},
	{"a", "s", "d", "f", "g", "h", "j", "k", "l", ":"},
	{"z", "x", "c", "v", "b", "n", "m", ".", "-", "/"},
	{"@", "_", "~", "?", "=", "&", "%", "+", "#", "*"},
	{"SHIFT", "SPACE", "DEL", "BACK", "OK"},
}

func upper(s string) string {
	if len(s) == 1 && s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0] - 32)
	}
	return s
}

// Handle processes one button. Returns done=true when OK is pressed (Text is final).
func (k *Keyboard) Handle(b Button) (done bool) {
	switch b {
	case BtnUp:
		if k.row > 0 {
			k.row--
		}
	case BtnDown:
		if k.row < len(kbGrid)-1 {
			k.row++
		}
	case BtnLeft:
		if k.col > 0 {
			k.col--
		}
	case BtnRight:
		if k.col < len(kbGrid[k.row])-1 {
			k.col++
		}
	case BtnBack:
		k.backspace()
	case BtnSelect:
		k.Text += " "
	case BtnConfirm:
		return k.activate()
	case BtnStart:
		return true
	}
	if k.col >= len(kbGrid[k.row]) {
		k.col = len(kbGrid[k.row]) - 1
	}
	return false
}

func (k *Keyboard) backspace() {
	if len(k.Text) > 0 {
		k.Text = k.Text[:len(k.Text)-1]
	}
}

func (k *Keyboard) activate() (done bool) {
	key := kbGrid[k.row][k.col]
	switch key {
	case "SHIFT":
		k.shift = !k.shift
	case "SPACE":
		k.Text += " "
	case "DEL":
		k.backspace()
	case "BACK":
		k.Cancelled = true
		return true
	case "OK":
		return true
	default:
		if k.shift {
			key = upper(key)
		}
		k.Text += key
	}
	return false
}

// Draw renders the prompt, the current text, and the key grid within (x,y,w,h).
func (k *Keyboard) Draw(c *Canvas, t Theme, x, y, w, h int) {
	c.DrawText(x, y, k.Prompt, t.Dim, t.SmallScale)
	// Text field.
	fy := y + glyphH*t.SmallScale + 10
	fieldH := glyphH*t.BodyScale + 16
	c.FillRect(x, fy, w, fieldH, t.Panel)
	c.Rect(x, fy, w, fieldH, t.Accent)
	shown := k.Text
	if shown == "" {
		c.DrawText(x+10, fy+8, "_", t.Dim, t.BodyScale)
	} else {
		c.DrawText(x+10, fy+8, shown+"_", t.Text, t.BodyScale)
	}
	// Grid. Cells advance by their ACTUAL width so wide control keys (SHIFT/SPACE/DEL/OK)
	// don't overlap their neighbours; single-char rows stay uniform.
	gy := fy + fieldH + 18
	cellH := glyphH*t.BodyScale + 14
	const gap = 6
	for r, rowKeys := range kbGrid {
		cx := x
		cy := gy + r*(cellH+gap)
		for cc, key := range rowKeys {
			label := key
			if k.shift {
				label = upper(key)
			}
			wCell := TextWidth(label, t.BodyScale) + 20
			if wCell < 52 {
				wCell = 52
			}
			bg := t.Panel
			tcol := t.Text
			if r == k.row && cc == k.col {
				bg = t.Accent
				tcol = 0x101018
			}
			c.FillRect(cx, cy, wCell, cellH, bg)
			c.DrawText(cx+(wCell-TextWidth(label, t.BodyScale))/2, cy+7, label, tcol, t.BodyScale)
			cx += wCell + gap
		}
	}
}

// Message draws a centered title + wrapped body in the content area; used for welcome,
// status, error, and done screens. Honest: callers pass real state only.
func (t Theme) Message(c *Canvas, title, body string, bodyColor Color) {
	x, y, w, _ := t.Frame(c, "Lodor Setup", "A: continue   B: back")
	c.DrawTextCentered(x, y+10, w, title, t.Accent, t.TitleScale-1)
	t.DrawTextWrappedAt(c, x, y+10+glyphH*(t.TitleScale-1)+30, w, body, bodyColor, t.BodyScale)
}

// DrawTextWrappedAt is Theme sugar over Canvas.DrawTextWrapped.
func (t Theme) DrawTextWrappedAt(c *Canvas, x, y, w int, s string, col Color, sc int) int {
	return c.DrawTextWrapped(x, y, w, s, col, sc)
}

// Progress draws a labeled progress bar (0..100) plus a phase line. Used during mirror/
// download. pct<0 renders an indeterminate (full-dim) bar.
func (t Theme) Progress(c *Canvas, title, phase string, pct int) {
	x, y, w, _ := t.Frame(c, "Lodor Setup", "please wait...")
	c.DrawText(x, y+10, title, t.Text, t.BodyScale)
	by := y + 10 + glyphH*t.BodyScale + 24
	barH := 28
	c.Rect(x, by, w, barH, t.Dim)
	if pct < 0 {
		c.FillRect(x+2, by+2, w-4, barH-4, t.Panel)
	} else {
		if pct > 100 {
			pct = 100
		}
		c.FillRect(x+2, by+2, (w-4)*pct/100, barH-4, t.Accent)
	}
	if phase != "" {
		c.DrawText(x, by+barH+20, phase, t.Dim, t.SmallScale)
	}
}
