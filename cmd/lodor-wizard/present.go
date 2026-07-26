package main

// present.go — the ONE canvas->framebuffer presentation seam (launch-card-v2
// TrimUI fix). The wizard composes every screen on a fixed W×H (720×480)
// canvas; real panels are NOT all 720×480 (TrimUI Smart Pro = 1280×720, and
// fb.Flush clips a smaller canvas into the panel's top-left corner — the
// 2026-07-11 flash test rendered the card as a small square up there).
// presentCanvas is the single flush path: identity on a matching panel,
// aspect-FIT (letterboxed on black, centered, nearest-neighbor — keeps the
// 8×8 font crisp) onto a panel-native canvas everywhere else. Handles both
// up-scale (1280×720) and down-scale (640×480) panels.

import "lodor/ui"

// presentCanvas flushes the composed canvas to the framebuffer, scaled to the
// panel. ALL wizard flush sites must route through here — a raw fb.Flush of a
// W×H canvas is the TrimUI corner-render bug.
func presentCanvas(fb *ui.Framebuffer, c *ui.Canvas) {
	if fb == nil || c == nil {
		return
	}
	pw, ph := fb.Xres(), fb.Yres()
	if pw < 1 || ph < 1 || (pw == c.W && ph == c.H) {
		fb.Flush(c) // matching (or unknowable) panel: the unchanged 720×480 path
		return
	}
	native := ui.NewCanvas(pw, ph) // black — the letterbox bars
	native.BlitScaled(c, 0, 0, pw, ph)
	fb.Flush(native)
}
