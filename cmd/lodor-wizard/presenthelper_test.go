package main

import (
	"testing"

	"lodor/ui"
)

// fakePresenter records the canvas dimensions it was handed — a stand-in for *ui.SDLHelper.
type fakePresenter struct {
	w, h    int
	gotW    int
	gotH    int
	presents int
}

func (f *fakePresenter) Present(c *ui.Canvas) error { f.gotW, f.gotH = c.W, c.H; f.presents++; return nil }
func (f *fakePresenter) Xres() int                  { return f.w }
func (f *fakePresenter) Yres() int                  { return f.h }

// TestPresentToHelperScalesUp: a 720x480 card canvas on a 1280x720 panel must be presented
// at the panel size (scaled + letterboxed), not corner-clipped at 720x480.
func TestPresentToHelperScalesUp(t *testing.T) {
	fp := &fakePresenter{w: 1280, h: 720}
	card := ui.NewCanvas(720, 480)
	if err := presentToHelper(fp, card); err != nil {
		t.Fatal(err)
	}
	if fp.gotW != 1280 || fp.gotH != 720 {
		t.Errorf("presented %dx%d, want panel-sized 1280x720", fp.gotW, fp.gotH)
	}
}

// TestPresentToHelperMatch: matching sizes present 1:1 (no scale canvas allocated).
func TestPresentToHelperMatch(t *testing.T) {
	fp := &fakePresenter{w: 720, h: 480}
	card := ui.NewCanvas(720, 480)
	if err := presentToHelper(fp, card); err != nil {
		t.Fatal(err)
	}
	if fp.gotW != 720 || fp.gotH != 480 {
		t.Errorf("presented %dx%d, want 1:1 720x480", fp.gotW, fp.gotH)
	}
}
