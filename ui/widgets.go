package ui

import "strings"

// Higher-level widgets for the onboarding wizard: a Theme, a Menu (list select), an
// on-screen Keyboard (text entry - Wi-Fi password stays muOS's job, but server URL,
// pairing code, and device name are entered here), and chrome (header/footer/message/
// progress) helpers. Immediate-mode: the wizard owns the loop, calls Draw, feeds Buttons.

// Theme holds the palette + text scales tuned for the 720×480 H700 panel.
type Theme struct {
	Bg, Panel, Accent, Text, Dim, Good, Bad, Warn Color
	TitleScale, BodyScale, SmallScale             int
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
		// Amber — the sync-status "queued / offline / needs-attention" tone (never the alarming
		// red Bad; offline is a normal state). Used by the exit save-sync splash.
		Warn: 0xfbbf24,

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
	// Title: step the scale down before ellipsizing (a long game name reads better one size
	// smaller than chopped), then measure — DrawText clips silently. barH stays pinned to
	// TitleScale so a shrunk title never moves the content rect; center the smaller text in it.
	inner := c.W - 48
	tsc := FitScale(title, inner, t.TitleScale, t.BodyScale)
	c.DrawText(24, (barH-glyphH*tsc)/2, FitText(title, inner, tsc), t.Text, tsc)
	// Footer hint bar. The hint is a control legend — every token is something the user needs
	// ("Start: OK"), so it WRAPS to a second line rather than being ellipsized away. The bar
	// grows to match and the content rect shrinks with it, which is what keeps the same legend
	// honest on a 640-wide panel as on the 720 one it was written for.
	lines := wrapLegend(hint, inner, t.SmallScale)
	if len(lines) > 2 { // pathological: keep the bar sane, ellipsize the tail
		lines = append(lines[:1], FitText(strings.Join(lines[1:], legendSep), inner, t.SmallScale))
	}
	lineStep := glyphH*t.SmallScale + 8
	footH := 16 + len(lines)*glyphH*t.SmallScale + (len(lines)-1)*8
	fy := c.H - footH
	c.FillRect(0, fy, c.W, footH, t.Panel)
	for i, ln := range lines {
		c.DrawText(24, fy+8+i*lineStep, ln, t.Dim, t.SmallScale)
	}
	return 24, barH + 20, inner, fy - (barH + 20) - 12
}

// legendSep is the gap the footer legend puts between control tokens ("A: type" /
// "B: delete"). Wrapping must not collapse it — plain word-wrap turns the legend into an
// unreadable run-on ("D-pad: move A: type B: delete"), so wrapLegend breaks on WHOLE tokens.
const legendSep = "   "

// wrapLegend packs a footer legend into lines no wider than maxW, splitting only at the
// token separator and preserving it. A single token wider than the bar is left for FitText.
func wrapLegend(hint string, maxW, sc int) []string {
	tokens := strings.Split(hint, legendSep)
	var lines []string
	line := ""
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		try := tok
		if line != "" {
			try = line + legendSep + tok
		}
		if TextWidth(try, sc) <= maxW {
			line = try
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
		line = tok
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// Menu is a vertical single-select list.
type Menu struct {
	Items []string
	sel   int
}

func (m *Menu) Selected() int { return m.sel }
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
		c.DrawText(x+22, ry+9, FitText(it, w-32, t.BodyScale), col, t.BodyScale)
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
		c.DrawText(x+22, ry+9, FitText(m.Items[i], w-44, t.BodyScale), col, t.BodyScale)
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
	Hint      string // optional guidance line above the prompt (lodor#40); "" = none
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

// kbLayout is the keyboard's vertical layout, derived once so Draw and gridBottom can never
// disagree. Hint and Prompt WRAP rather than ellipsize — both are instructions ("In RomM on
// your computer: Settings > Devices > Pair"), and a truncated instruction is worse than a
// two-line one — so the field and grid must track however many lines they actually took.
type kbLayout struct {
	hintY, promptY int // wrapped-text origins
	fieldY, fieldH int
	gridY          int
	cellH, gridGap int
	gridBottom     int
}

func (k *Keyboard) layout(t Theme, y, w, h int) kbLayout {
	bottom := y + h
	l := kbLayout{}
	l.hintY = y
	if k.Hint != "" {
		y += WrappedHeight(k.Hint, w, t.SmallScale)
	}
	l.promptY = y
	y += WrappedHeight(k.Prompt, w, t.SmallScale)
	l.fieldY = y + 2
	l.fieldH = glyphH*t.BodyScale + 16
	// The grid takes whatever vertical room the wrapped text left it. A two-line hint plus a
	// two-line prompt plus a two-line footer (all of which happen on a 640-wide panel) costs
	// ~70px the 720×480 design never budgeted, so squeeze the paddings — in order of least
	// harm: the gap above the grid, then between rows, then the cells' own padding — rather
	// than let the last key row slide under the footer bar.
	rows := len(kbGrid)
	pad, gap, cellH := 18, 6, glyphH*t.BodyScale+14
	minCellH := glyphH*t.BodyScale + 6
	avail := bottom - (l.fieldY + l.fieldH)
shrink:
	for pad+rows*cellH+(rows-1)*gap > avail {
		switch {
		case pad > 8:
			pad--
		case gap > 2:
			gap--
		case cellH > minCellH:
			cellH--
		default:
			// Genuinely out of room (a panel shorter than the design allows). Stop shrinking
			// and let gridBottom report the overrun honestly rather than fake a fit.
			break shrink
		}
	}
	l.cellH, l.gridGap = cellH, gap
	l.gridY = l.fieldY + l.fieldH + pad
	l.gridBottom = l.gridY + rows*cellH + (rows-1)*gap
	return l
}

// gridBottom is the y just past the last key row — the value the layout guard asserts stays
// inside the content box (a wrapped hint pushes the grid down toward the footer bar).
func (k *Keyboard) gridBottom(t Theme, y, w, h int) int { return k.layout(t, y, w, h).gridBottom }

// Draw renders the prompt, the current text, and the key grid within (x,y,w,h).
func (k *Keyboard) Draw(c *Canvas, t Theme, x, y, w, h int) {
	l := k.layout(t, y, w, h)
	if k.Hint != "" {
		c.DrawTextWrapped(x, l.hintY, w, k.Hint, t.Dim, t.SmallScale)
	}
	c.DrawTextWrapped(x, l.promptY, w, k.Prompt, t.Dim, t.SmallScale)
	// Text field. Long entries (a full server URL) scroll from the RIGHT so the caret and
	// the characters just typed stay visible.
	fy, fieldH := l.fieldY, l.fieldH
	c.FillRect(x, fy, w, fieldH, t.Panel)
	c.Rect(x, fy, w, fieldH, t.Accent)
	shown := k.Text
	if shown == "" {
		c.DrawText(x+10, fy+8, "_", t.Dim, t.BodyScale)
	} else {
		c.DrawText(x+10, fy+8, FitTextTail(shown+"_", w-20, t.BodyScale), t.Text, t.BodyScale)
	}
	// Grid. Cells advance by their ACTUAL width so wide control keys (SHIFT/SPACE/DEL/OK)
	// don't overlap their neighbours; single-char rows stay uniform.
	gy, cellH, gap := l.gridY, l.cellH, l.gridGap
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
	c.DrawTextCentered(x, y+10, w, FitText(title, w, t.TitleScale-1), t.Accent, t.TitleScale-1)
	t.DrawTextWrappedAt(c, x, y+10+glyphH*(t.TitleScale-1)+30, w, body, bodyColor, t.BodyScale)
}

// DrawTextWrappedAt is Theme sugar over Canvas.DrawTextWrapped.
func (t Theme) DrawTextWrappedAt(c *Canvas, x, y, w int, s string, col Color, sc int) int {
	return c.DrawTextWrapped(x, y, w, s, col, sc)
}

// Progress draws a labeled progress bar (0..100) plus a phase line. Used during mirror/
// download. pct<0 renders an indeterminate (full-dim) bar.
func (t Theme) Progress(c *Canvas, title, phase string, pct int) {
	t.ProgressHint(c, title, phase, pct, "please wait...")
}

// ProgressHint is Progress with a caller-supplied hint line (lodor#42: the wizard's
// cancelable long-op screens show "B: stop" — an honest affordance — instead of the
// passive "please wait..."). Rendering is otherwise byte-identical to Progress.
func (t Theme) ProgressHint(c *Canvas, title, phase string, pct int, hint string) {
	x, y, w, _ := t.Frame(c, "Lodor Setup", hint)
	c.DrawText(x, y+10, FitText(title, w, t.BodyScale), t.Text, t.BodyScale)
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
		c.DrawText(x, by+barH+20, FitText(phase, w, t.SmallScale), t.Dim, t.SmallScale)
	}
}
