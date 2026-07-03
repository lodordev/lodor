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
	// OK key (row5,col3) finishes
	k.row, k.col = 5, 3
	if !k.Handle(BtnConfirm) {
		t.Fatal("OK should finish")
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
