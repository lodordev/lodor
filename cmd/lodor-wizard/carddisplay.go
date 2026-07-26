package main

// carddisplay.go — the launch-card DISPLAY-lane split (launch-card-v2 Phase B).
//
// The card composes every screen on a fixed W×H (720×480) canvas. Presentation differs by
// device lane:
//   RAW-FB lane (Miyoo / H700): ui.Framebuffer + presentCanvas (unchanged — the /dev/fb0
//       blit path; fb_linux.go is byte-untouched).
//   SDL lane (NextUI: tg5040 / tg5050 / my355): ui.SDLHelper — /dev/fb0 is a dead scanout
//       surface there, so a prebuilt SDL2 helper owns the panel and we pipe RGB565 frames.
// INPUT is ui.EvdevSource on BOTH lanes (openInput) — the SDL helper reads no input.
//
// sdlLaneActive decides the lane by env (set by the NextUI pre-launch hook): LODOR_HOST_OS=
// nextui, OR an explicit LODOR_FBHELPER path. Everything else stays the proven raw-fb path,
// so no existing device changes behavior.

import (
	"fmt"
	"os"

	"lodor/ui"
)

// sdlLaneActive reports whether this launch should present through the SDL helper instead of
// raw /dev/fb0. NextUI (tg5040/tg5050/my355) is the SDL lane; everything else is raw-fb.
func sdlLaneActive() bool {
	if os.Getenv("LODOR_FORCE_RAWFB") == "1" {
		return false // escape hatch for diagnosis
	}
	if hostOS() == "nextui" {
		return true
	}
	// An explicitly-provided helper path also selects the lane (spike/manual runs).
	return os.Getenv("LODOR_FBHELPER") != ""
}

// inputKeyMap selects the per-platform evdev keymap for EvdevSource, keyed on LODOR_HOST_OS
// (each lane's pre-launch hook exports it — muos/knulli/lodoros/nextui). The physical A/B
// evdev codes are NOT the same across lanes, so shipping one global map swaps A/B on three of
// them:
//
//	nextui (tg5040/tg5050/my355, SDL lane) -> ui.NextUIKeyMap()  — Nintendo layout, A=305/B=304,
//	    TrimUI shoulder block. Also selected when the SDL lane is forced by LODOR_FBHELPER
//	    (spike/manual runs), so display and input never disagree.
//	muos + knulli (Anbernic H700 family)   -> ui.H700KeyMap()    — Nintendo layout, A=305/B=304
//	    [certain, sun50i-h700 DTS], but STANDARD BTN_ Start/Select/shoulders (315/314/310/311).
//	    Fixes the same A/B-swap bug the TrimUI had — before this the H700 raw-fb card used the
//	    Miyoo map and A launched the game instead of opening States/Saves.
//	everything else (LodorOS miyoomini/my355, unset) -> ui.DefaultKeyMap() — Xbox/SDL layout,
//	    A=304/B=305. UNCHANGED, so every existing Miyoo device stays byte-identical.
func inputKeyMap() ui.Keymap {
	if sdlLaneActive() {
		// The SDL lane covers TWO different input layouts — key on the host, not the lane:
		//   nextui (tg5040/tg5050/my355) -> Nintendo A/B swap, BTN_ codes -> NextUIKeyMap()
		//   lodoros miyoomini (SDL helper) -> Miyoo KEYBOARD scancodes (A=57, B=29, Select=97,
		//       D-pad 103-106). NextUIKeyMap DROPS 29/97, so B + Select would be dead — the
		//       default map is the one that covers the Miyoo keyboard layout. (Bug 2026-07-24:
		//       every miyoomini SDL launch was silently getting the NextUI map.)
		if hostOS() == "nextui" {
			return ui.NextUIKeyMap()
		}
		return ui.DefaultKeyMap()
	}
	switch hostOS() {
	case "muos", "knulli":
		return ui.H700KeyMap()
	}
	return ui.DefaultKeyMap()
}

// sdlPanelSize returns the panel dimensions for the SDL helper. Defaults to the TrimUI Smart
// Pro 1280×720; LODOR_SPIKE_W/H (shared with the spike) override for other panels. The card
// canvas is composed at W×H and scaled to this before Present, so the card fills the screen.
func sdlPanelSize() (int, int) {
	return envInt("LODOR_SPIKE_W", 1280), envInt("LODOR_SPIKE_H", 720)
}

// canvasPresenter is the SDL-helper surface presentToHelper needs (satisfied by
// *ui.SDLHelper; a fake in tests). Kept minimal so the scale seam is unit-testable.
type canvasPresenter interface {
	Present(*ui.Canvas) error
	Xres() int
	Yres() int
}

// presentToHelper scales the fixed-W×H card canvas up to the panel and pushes it to the SDL
// helper — the SDL-lane analogue of presentCanvas(fb, c). Matching sizes blit 1:1; a smaller
// card canvas is aspect-fit scaled to the panel, never corner-clipped.
func presentToHelper(h canvasPresenter, c *ui.Canvas) error {
	pw, ph := h.Xres(), h.Yres()
	if pw < 1 || ph < 1 || (pw == c.W && ph == c.H) {
		return h.Present(c)
	}
	native := ui.NewCanvas(pw, ph) // black letterbox bars
	native.BlitScaled(c, 0, 0, pw, ph)
	return h.Present(native)
}

// openCardSDLHelper spawns lodor-fbhelper for the card. Returns nil (with a logged reason)
// on any failure so the caller degrades to "launch anyway" — a helper problem must never
// block the game.
func (w *wizard) openCardSDLHelper() *ui.SDLHelper {
	helper := spikeHelperPath() // same resolver: LODOR_FBHELPER, sibling, or PATH
	pw, ph := sdlPanelSize()
	// Pass the platform keymap so the helper forwards input (its SDL grabs the keypad; a separate
	// evdev reader is starved). rawLog = the wizard-input.log backstop, so forwarded #btn lines and
	// the #surface line are captured for the on-device field log.
	h, err := ui.OpenSDLHelper(helper, pw, ph, w.openInputRawLog(), nil, inputKeyMap())
	if err != nil {
		fmt.Println("LAUNCHCARD lane=sdl helper=fail err=" + err.Error())
		return nil
	}
	w.phaseFB(pw, ph, 16) // field logs show the real SDL panel (RGB565)
	return h
}
