package ui

import "testing"

func TestKeyboardTyping(t *testing.T) {
	k := &Keyboard{}
	// type "1" (row0,col0)
	k.row, k.col = 0, 0
	if k.Handle(BtnConfirm) {
		t.Fatal("typing a key should not finish")
	}
	if k.Text != "1" {
		t.Fatalf("after '1' got %q", k.Text)
	}
	// SHIFT on (control row 5, col0), then 'q' -> 'Q'
	k.row, k.col = 5, 0
	k.Handle(BtnConfirm) // toggles shift
	k.row, k.col = 1, 0
	k.Handle(BtnConfirm)
	if k.Text != "1Q" {
		t.Fatalf("after shift+q got %q want 1Q", k.Text)
	}
	// Select inserts a space
	k.Handle(BtnSelect)
	if k.Text != "1Q " {
		t.Fatalf("after space got %q", k.Text)
	}
	// Back deletes
	k.Handle(BtnBack)
	if k.Text != "1Q" {
		t.Fatalf("after backspace got %q", k.Text)
	}
	// DEL key (row5,col2) also deletes
	k.row, k.col = 5, 2
	k.Handle(BtnConfirm)
	if k.Text != "1" {
		t.Fatalf("after DEL got %q", k.Text)
	}
	// OK key (row5,col4) finishes without cancelling
	k.row, k.col = 5, 4
	if !k.Handle(BtnConfirm) {
		t.Fatal("OK should finish")
	}
	if k.Cancelled {
		t.Fatal("OK must not set Cancelled")
	}
}

func TestKeyboardBackKeyCancels(t *testing.T) {
	k := &Keyboard{Text: "abc"}
	// BACK key (row5,col3) finishes AND flags cancellation so the caller backs out.
	k.row, k.col = 5, 3
	if !k.Handle(BtnConfirm) {
		t.Fatal("BACK should finish the keyboard")
	}
	if !k.Cancelled {
		t.Fatal("BACK must set Cancelled so the caller navigates back")
	}
}

func TestKeyboardColClampOnRowChange(t *testing.T) {
	k := &Keyboard{}
	k.row, k.col = 0, 9 // last col of a 10-wide row
	k.Handle(BtnDown)   // move to a row that may be shorter
	if k.col >= len(kbGrid[k.row]) {
		t.Fatalf("col %d out of range for row %d (len %d)", k.col, k.row, len(kbGrid[k.row]))
	}
}

func TestMenuNavigation(t *testing.T) {
	m := &Menu{Items: []string{"a", "b", "c"}}
	if m.Selected() != 0 {
		t.Fatal("starts at 0")
	}
	m.Handle(BtnDown)
	m.Handle(BtnDown)
	if m.Selected() != 2 {
		t.Fatalf("after 2 downs sel=%d", m.Selected())
	}
	m.Handle(BtnDown) // clamp at last
	if m.Selected() != 2 {
		t.Fatalf("should clamp at last, sel=%d", m.Selected())
	}
	m.Handle(BtnUp)
	if m.Selected() != 1 {
		t.Fatalf("after up sel=%d", m.Selected())
	}
	if !m.Handle(BtnConfirm) {
		t.Fatal("Confirm should report true")
	}
	if m.SelectedItem() != "b" {
		t.Fatalf("SelectedItem=%q want b", m.SelectedItem())
	}
}

func TestScrollMenuNavigationAndWindow(t *testing.T) {
	items := make([]string, 30)
	for i := range items {
		items[i] = "row"
	}
	m := &ScrollMenu{Items: items}
	if m.Selected() != 0 {
		t.Fatal("starts at 0")
	}
	for i := 0; i < 40; i++ {
		m.Handle(BtnDown) // clamp at last
	}
	if m.Selected() != 29 {
		t.Fatalf("down should clamp at 29, got %d", m.Selected())
	}
	// Drawing into a short box must scroll the window so the selection stays visible.
	th := DefaultTheme()
	c := NewCanvas(400, 120) // ~3 rows tall
	m.Draw(c, th, 0, 0, 400, 120)
	vis := m.VisibleRows(th, 120)
	if m.off > m.sel || m.sel >= m.off+vis {
		t.Fatalf("selection %d not in visible window [%d,%d)", m.sel, m.off, m.off+vis)
	}
	if !m.Handle(BtnConfirm) {
		t.Fatal("Confirm should report true")
	}
}

func TestParseEngineResult(t *testing.T) {
	out := "some log noise\nMIRROR created=3 covers=2\nRESULT pushed=1 total=1 stuck=0\n"
	if got := ParseEngineResult(out); got != "RESULT pushed=1 total=1 stuck=0" {
		t.Fatalf("structured RESULT preferred, got %q", got)
	}
	if got := ParseEngineResult("just one line\n"); got != "just one line" {
		t.Fatalf("fallback to last line, got %q", got)
	}
	if got := ParseEngineResult("   \n\n"); got != "" {
		t.Fatalf("blank output must yield empty (no fabricated OK), got %q", got)
	}
	if got := ResultToken("RESULT pushed=2 stuck=1", "stuck"); got != "1" {
		t.Fatalf("ResultToken stuck=%q want 1", got)
	}
	if got := ResultToken("RESULT ra_logged_in=1 ra_user=neo", "ra_user"); got != "neo" {
		t.Fatalf("ResultToken ra_user=%q want neo", got)
	}
	if got := ResultToken("RESULT paired=0", "username"); got != "" {
		t.Fatalf("absent token must be empty, got %q", got)
	}
}

func TestTextWidth(t *testing.T) {
	if got := TextWidth("", 2); got != 0 {
		t.Fatalf("empty width %d want 0", got)
	}
	// "AB" at scale 1: 2*(8+1) - 1 = 17
	if got := TextWidth("AB", 1); got != 17 {
		t.Fatalf("AB width %d want 17", got)
	}
}

func TestCanvasFillRectBounds(t *testing.T) {
	c := NewCanvas(4, 4)
	c.FillRect(-2, -2, 3, 3, 0xffffff) // partially off top-left
	if c.Pix[0] != 0xffffff {
		t.Fatal("(0,0) should be filled")
	}
	c.FillRect(10, 10, 5, 5, 0xff0000) // fully off-canvas: no panic, no change
}
