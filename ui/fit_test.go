package ui

// fit_test.go — the guard for lodor-muos#2 ("parts of the UI gets cut off", RG35XXH,
// 2026-07-26). Canvas.Set is bounds-checked, so DrawText drops over-wide text SILENTLY:
// the pairing-code hint and the keyboard footer were both wider than the 720px compose
// canvas and had been rendering truncated ("Settings > De", "BAC") on EVERY device since
// they were written. The wizard's --capture mode dumped those frames all along, but it
// dumps PNGs for a human to eyeball — nothing measured them. These tests do.

import "testing"

func TestFitTextEllipsizesToWidth(t *testing.T) {
	const sc = 2
	long := "In RomM on your computer: Settings > Devices > Pair"
	for _, maxW := range []int{672, 592, 300, 60, 19, 10, 1} {
		got := FitText(long, maxW, sc)
		if w := TextWidth(got, sc); w > maxW {
			t.Errorf("FitText(maxW=%d) = %q, width %d > %d", maxW, got, w, maxW)
		}
	}
	if got := FitText(long, 0, sc); got != long {
		t.Errorf("maxW<=0 must pass through unchanged, got %q", got)
	}
	if got := FitText("short", 672, sc); got != "short" {
		t.Errorf("text that fits must be untouched, got %q", got)
	}
	if got := FitText(long, 300, sc); len(got) < 4 || got[len(got)-3:] != "..." {
		t.Errorf("ellipsis missing: %q", got)
	}
}

func TestFitTextTailKeepsTheEnd(t *testing.T) {
	const sc = 2
	url := "https://romm.my-really-long-hostname.example.com/library_"
	got := FitTextTail(url, 300, sc)
	if w := TextWidth(got, sc); w > 300 {
		t.Fatalf("FitTextTail = %q, width %d > 300", got, w)
	}
	// The caret end is what the user is typing at — it must survive.
	if got[len(got)-1] != '_' {
		t.Fatalf("tail fit dropped the end of the string: %q", got)
	}
	if got[:3] != "..." {
		t.Fatalf("tail fit missing leading ellipsis: %q", got)
	}
}

func TestFitScaleStepsDownBeforeGivingUp(t *testing.T) {
	// 27 chars at scale 4 is 968px — too wide for a 672px bar; scale 2 (484px) fits.
	if sc := FitScale("Sonic The Hedgehog (USA).md", 672, 4, 2); sc != 3 {
		// scale 3: 27*27-3 = 726 > 672, so the answer must be 2.
		if sc != 2 {
			t.Fatalf("FitScale = %d, want 2 (scale 3 is 726px, over the 672px bar)", sc)
		}
	}
	if sc := FitScale("Lodor Setup", 672, 4, 2); sc != 4 {
		t.Fatalf("a title that already fits must keep TitleScale, got %d", sc)
	}
	if sc := FitScale("wildly beyond any scale at all, even the smallest one here", 40, 4, 2); sc != 2 {
		t.Fatalf("nothing fits -> floor at min, got %d", sc)
	}
}

// ---- whole-frame guards: no ink may reach the canvas edge -------------------------------

// inkAtRightEdge reports the rows where a foreground (text) color reaches the last column.
// The chrome's bars are Panel/Accent and legitimately span the full width; only glyph ink
// at the edge means a string was silently chopped.
func inkAtRightEdge(c *Canvas, t Theme) []int {
	ink := map[Color]bool{t.Text: true, t.Dim: true, t.Good: true, t.Bad: true, t.Warn: true}
	var rows []int
	for y := 0; y < c.H; y++ {
		if ink[c.Pix[y*c.W+c.W-1]] {
			rows = append(rows, y)
		}
	}
	return rows
}

// panels covers the fleet's real geometries: the RG34XX 3:2 panel the wizard was designed
// on, the 4:3 640×480 panel the bug was reported from (RG35XXH / Miyoo), and the TrimUI
// Smart Pro. A screen must be clean on all of them.
var panels = []struct {
	name string
	w, h int
}{
	{"720x480-rg34xx", 720, 480},
	{"640x480-rg35xxh", 640, 480},
	{"1280x720-smartpro", 1280, 720},
}

func TestFrameNeverClipsTitleOrHint(t *testing.T) {
	th := DefaultTheme()
	// The real strings the wizard passes, longest first.
	cases := []struct{ title, hint string }{
		{"Lodor Setup", "D-pad: move   A: type   B: delete   BACK: go back   Start: OK"},
		{"Lodor", "D-pad: move   A: type   B: delete   BACK: cancel   Start: OK"},
		{"Sonic The Hedgehog (USA).md", "Up/Down: move   A: select   B: back"},
		{"Legend of Dragoon (Disc 1 of 4) (USA) (Rev 1).m3u", "Up/Down: move   A: select   B: exit"},
	}
	for _, p := range panels {
		for _, tc := range cases {
			c := NewCanvas(p.w, p.h)
			th.Frame(c, tc.title, tc.hint)
			if rows := inkAtRightEdge(c, th); len(rows) > 0 {
				t.Errorf("%s: Frame(%q, %q) clipped at the right edge, rows %v",
					p.name, tc.title, tc.hint, rows)
			}
		}
	}
}

func TestKeyboardNeverClipsAndFitsAboveTheFooter(t *testing.T) {
	th := DefaultTheme()
	const pairCodeHint = "In RomM on your computer: Settings > Devices > Pair"
	cases := []*Keyboard{
		{Prompt: "Enter your RomM pairing code:", Text: "X7K2", Hint: pairCodeHint},
		{Prompt: "Enter a RomM pairing code for the new profile:", Hint: pairCodeHint},
		{Prompt: "Enter your RomM server address:", Text: "https://romm.a-very-long-hostname.example.com/api/v1"},
		{Prompt: "Name this device:", Text: "RG35XXH"},
	}
	for _, p := range panels {
		for _, kb := range cases {
			c := NewCanvas(p.w, p.h)
			x, y, w, h := th.Frame(c, "Lodor Setup",
				"D-pad: move   A: type   B: delete   BACK: go back   Start: OK")
			kb.Draw(c, th, x, y, w, h)
			if rows := inkAtRightEdge(c, th); len(rows) > 0 {
				t.Errorf("%s: Keyboard(%q) clipped at the right edge, rows %v", p.name, kb.Prompt, rows)
			}
			// The grid must also stay inside the content box — a wrapped hint pushes it down,
			// and running under the footer bar is the same bug on the other axis.
			if bot := kb.gridBottom(th, y, w, h); bot > y+h {
				t.Errorf("%s: Keyboard(%q) grid bottom %d overruns the content box (%d)",
					p.name, kb.Prompt, bot, y+h)
			}
		}
	}
}
