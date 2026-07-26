package main

// sdlspike.go — the Phase A hardware spike (launch-card-v2). NOT the real card; a trivial
// interactive proof that the CGO-free wizard can DISPLAY frames through lodor-fbhelper AND
// take BUTTON input on the SDL-only panel (TrimUI tg5040/tg5050) where raw /dev/fb0 is black.
//
// THE 2026-07-12 FIX: the first spike DISPLAYED but INPUT was dead and the device LOCKED UP.
// Root cause: SDL's joystick/keyboard layer saw none of the TrimUI's buttons (zero "#code="
// lines), while the wizard's Go EvdevSource reads /dev/input on this exact device fine. So
// the spike now uses the LANE SPLIT: DISPLAY via lodor-fbhelper (Present), INPUT via
// ui.EvdevSource (the proven path). And it arms a WATCHDOG so it can NEVER trap the user —
// after an idle with no input it self-exits, even if input somehow fails again.
//
// Loop: fill the whole panel a color, draw the last-pressed button name + a legend, push the
// frame; A->red, B->blue, UP/DOWN/LEFT/RIGHT cycle more colors; MENU/START exits; and if no
// button arrives for the idle timeout the watchdog exits on its own. Every evdev event
// (raw type/code/val + decoded name) is tee'd to LODOR_SPIKE_LOG so the flash records the
// device's REAL evdev button mapping.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"lodor/ui"
)

// spikeColors keyed by the last logical button. 0xRRGGBB.
var spikeColors = map[ui.Button]ui.Color{
	ui.BtnConfirm: 0xC0182B, // A -> red
	ui.BtnBack:    0x1B4DC0, // B -> blue
	ui.BtnUp:      0x159A3A, // Up -> green
	ui.BtnDown:    0xC8A21A, // Down -> amber
	ui.BtnLeft:    0x7A1FB0, // Left -> purple
	ui.BtnRight:   0x0FA3A3, // Right -> teal
	ui.BtnL1:      0x555555, // L1 -> gray
	ui.BtnR1:      0xAAAAAA, // R1 -> light gray
}

// sdlSpike runs the interactive SDL-helper proof. panelW/H default to the TrimUI Smart Pro
// (1280x720) but can be overridden with LODOR_SPIKE_W / LODOR_SPIKE_H. Returns a process
// exit code. Always exits cleanly (helper teardown never blanks the panel; watchdog guards
// against a wedged screen).
func sdlSpike() int {
	pw := envInt("LODOR_SPIKE_W", 1280)
	ph := envInt("LODOR_SPIKE_H", 720)

	helper := spikeHelperPath()
	logPath := os.Getenv("LODOR_SPIKE_LOG")
	var logW *os.File
	if logPath != "" {
		logW, _ = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	var rawLog *os.File = logW // tee the helper #surface diag + the evdev codes here
	if rawLog != nil {
		defer rawLog.Close()
		fmt.Fprintf(rawLog, "=== sdl-spike start helper=%s panel=%dx%d ===\n", helper, pw, ph)
	}

	// DISPLAY: lodor-fbhelper (SDL surface). The spike keeps its OWN evdev input reader, so pass
	// km=nil (no input forwarding) — the helper just tees #surface and (harmlessly) #btn lines.
	h, err := ui.OpenSDLHelper(helper, pw, ph, rawLog, nil, nil)
	if err != nil {
		fmt.Printf("SDLSPIKE shown=0 reason=helper-start-failed err=%v\n", err)
		return 1
	}
	defer h.Close()

	// INPUT: the proven Go evdev reader, logging every raw event to the spike log so we
	// capture the device's TRUE numeric codes (SDL never saw them). The spike runs on the
	// tg5040/NextUI (SDL) lane, so it decodes name= against the NextUI map (inputKeyMap) —
	// but the raw code=/val= fields are map-independent, so the [believe] codes 306-311 are
	// still captured verbatim for on-device evtest confirmation.
	in, err := ui.NewEvdevSourceLoggedFor(rawLog, inputKeyMap())
	if err != nil {
		// No input devices — still don't wedge: paint one frame, let the watchdog exit us.
		fmt.Printf("SDLSPIKE shown=0 reason=no-evdev err=%v\n", err)
	} else {
		defer in.Close()
		if rawLog != nil {
			fmt.Fprintf(rawLog, "#input open %d evdev devices\n", in.Count())
		}
	}

	// WATCHDOG: never trap the user. No press for the idle timeout => self-exit(0).
	wd := newWatchdog("spike", cardIdleTimeout(), nil)
	defer wd.stop()

	last := "(none)"
	frames := 0
	draw := func(col ui.Color) error {
		c := ui.NewCanvas(pw, ph)
		c.Clear(col)
		sc := ph / 90
		if sc < 3 {
			sc = 3
		}
		white := ui.Color(0xFFFFFF)
		c.DrawTextCentered(0, ph/6, pw, "LODOR SDL SPIKE", white, sc)
		c.DrawTextCentered(0, ph/2-8*sc, pw, "LAST BUTTON:", white, sc)
		c.DrawTextCentered(0, ph/2+2*sc, pw, last, white, sc*2)
		c.DrawTextCentered(0, ph-ph/6, pw, "A=RED B=BLUE DPAD=COLORS  MENU/START=EXIT", white, sc)
		return h.Present(c)
	}

	// first paint (red).
	cur := spikeColors[ui.BtnConfirm]
	if err := draw(cur); err != nil {
		fmt.Printf("SDLSPIKE shown=0 reason=first-present-failed err=%v\n", err)
		return 1
	}
	frames++

	// If evdev failed to open, we have no button channel; hold the frame and let the
	// watchdog free the device rather than spin.
	if in == nil {
		select {} // watchdog fires os.Exit(0) — guaranteed escape, never a lock
	}

	for b := range in.Buttons() {
		wd.kick() // real input — reset the idle clock
		last = b.String()
		if b == ui.BtnStart { // MENU/START => exit (mapEventName/evdev both map these to Start)
			fmt.Printf("SDLSPIKE shown=1 frames=%d exit=menu last=%s\n", frames, last)
			return 0
		}
		if col, ok := spikeColors[b]; ok {
			cur = col
		}
		if err := draw(cur); err != nil {
			fmt.Printf("SDLSPIKE shown=1 frames=%d reason=present-failed err=%v\n", frames, err)
			return 1
		}
		frames++
	}
	// Buttons channel closed (source died) without an exit chord.
	fmt.Printf("SDLSPIKE shown=1 frames=%d exit=input-closed last=%s\n", frames, last)
	return 0
}

// spikeHelperPath resolves lodor-fbhelper: LODOR_FBHELPER, else next to this binary, else
// bare name on PATH.
func spikeHelperPath() string {
	if p := os.Getenv("LODOR_FBHELPER"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "lodor-fbhelper")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return "lodor-fbhelper"
}

// envInt reads a positive integer env var, else def. Non-positive / unparsable => def.
func envInt(key string, def int) int {
	if s := os.Getenv(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return def
}
