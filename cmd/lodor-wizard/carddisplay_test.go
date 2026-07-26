package main

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"lodor/ui"
)

// TestInputKeyMapPerLane proves the wizard picks the RIGHT evdev keymap per lane, keyed on the
// same LODOR_HOST_OS / LODOR_FBHELPER signal the display split uses (launch-card-v2 fleet
// rollout). We probe with codes that DISTINGUISH the three maps:
//   - code 304: Confirm on default (Miyoo) AND h700 (hw-corrected 2026-07-21), Back on nextui
//   - code 308: L1 on nextui and h700 (both wear the TrimUI-style shoulder block), None on default
//   - code 315: Start on default only; None on h700 (real R2 there) and nextui
// so a single map identity is pinned by the (304,308,315) triple.
func TestInputKeyMapPerLane(t *testing.T) {
	for _, k := range []string{"LODOR_HOST_OS", "LODOR_FBHELPER", "LODOR_FORCE_RAWFB"} {
		t.Setenv(k, "")
	}
	cases := []struct {
		name                      string
		hostOS, helper            string
		want304, want308, want315 ui.Button
	}{
		// muOS + Knulli -> H700 map (hw-corrected): 304=Confirm (no swap), 308=L1, 315=None (real R2).
		{"muos->h700", "muos", "", ui.BtnConfirm, ui.BtnL1, ui.BtnNone},
		{"knulli->h700", "knulli", "", ui.BtnConfirm, ui.BtnL1, ui.BtnNone},
		// NextUI (SDL lane) -> NextUI map: 304=Back, 308=L1, 315=None.
		{"nextui->nextui", "nextui", "", ui.BtnBack, ui.BtnL1, ui.BtnNone},
		// LodorOS (miyoomini/my355) + unset -> default map: 304=Confirm (unchanged), 315=Start.
		{"lodoros->default", "lodoros", "", ui.BtnConfirm, ui.BtnNone, ui.BtnStart},
		{"unset->default", "", "", ui.BtnConfirm, ui.BtnNone, ui.BtnStart},
		// The SDL lane keys the map on HOST, not merely on the lane (fix 2026-07-24): nextui keeps
		// the NextUI map even with an explicit helper; miyoomini (lodoros) on the SDL helper takes
		// the DEFAULT map — its buttons are Miyoo KEYBOARD scancodes (B=29, Select=97), which
		// NextUIKeyMap drops. Getting NextUIKeyMap here was the rc7 "B/Select dead" bug.
		{"nextui+helper->nextui", "nextui", "/pak/lodor-fbhelper", ui.BtnBack, ui.BtnL1, ui.BtnNone},
		{"miyoomini-sdl->default", "lodoros", "/pak/lodor-fbhelper", ui.BtnConfirm, ui.BtnNone, ui.BtnStart},
	}
	for _, c := range cases {
		t.Setenv("LODOR_HOST_OS", c.hostOS)
		t.Setenv("LODOR_FBHELPER", c.helper)
		km := inputKeyMap() // Keymap is code->Button; direct lookup mirrors decodeEventWith's EV_KEY path
		if got := km[304]; got != c.want304 {
			t.Errorf("%s: 304 = %v, want %v", c.name, got, c.want304)
		}
		if got := km[308]; got != c.want308 {
			t.Errorf("%s: 308 = %v, want %v", c.name, got, c.want308)
		}
		if got := km[315]; got != c.want315 {
			t.Errorf("%s: 315 = %v, want %v", c.name, got, c.want315)
		}
	}
}

// TestInputKeyMapCompleteness (test-coverage gap #3): TestInputKeyMapPerLane pins WHICH map each
// lane gets and a few distinguishing codes — but not that a lane's map actually REACHES every
// logical button the launch card drives. A lane silently missing Confirm, Back, or a d-pad
// direction would leave the user unable to navigate or select on that device (a soft-brick of
// the card), and the per-lane test would still pass. This asserts each shipped lane's map is
// COMPLETE for the card's control surface.
//
// The card consumes (grep Btn* in launchcard*.go): Up/Down/Left/Right, Confirm, Back, Start,
// L1, R1. Select is NOT consumed by the card, so it is not required (though default/h700 carry
// it). A map missing any REQUIRED button fails here.
func TestInputKeyMapCompleteness(t *testing.T) {
	// Required at minimum: the four d-pad directions + Confirm + Back.
	coreNav := []ui.Button{ui.BtnUp, ui.BtnDown, ui.BtnLeft, ui.BtnRight, ui.BtnConfirm, ui.BtnBack}
	// Also used by the card: Start (play shortcut) and the L1/R1 action-row shoulders.
	cardExtra := []ui.Button{ui.BtnStart, ui.BtnL1, ui.BtnR1}
	required := append(append([]ui.Button{}, coreNav...), cardExtra...)

	reaches := func(km ui.Keymap, b ui.Button) bool {
		for _, v := range km {
			if v == b {
				return true
			}
		}
		return false
	}
	name := func(b ui.Button) string {
		switch b {
		case ui.BtnUp:
			return "Up"
		case ui.BtnDown:
			return "Down"
		case ui.BtnLeft:
			return "Left"
		case ui.BtnRight:
			return "Right"
		case ui.BtnConfirm:
			return "Confirm"
		case ui.BtnBack:
			return "Back"
		case ui.BtnStart:
			return "Start"
		case ui.BtnSelect:
			return "Select"
		case ui.BtnL1:
			return "L1"
		case ui.BtnR1:
			return "R1"
		}
		return "?"
	}

	lanes := []struct {
		lane string
		km   ui.Keymap
	}{
		{"default", ui.DefaultKeyMap()},
		{"nextui", ui.NextUIKeyMap()},
		{"h700", ui.H700KeyMap()},
	}
	for _, l := range lanes {
		for _, b := range required {
			if !reaches(l.km, b) {
				t.Errorf("lane %q keymap is missing REQUIRED button %s — the card cannot be driven on that device", l.lane, name(b))
			}
		}
	}

	// my355 / Miyoo Flip (LODOR_HOST_OS unset or "lodoros", raw-fb lane) is UNVERIFIED on-device:
	// its evdev codes have not been confirmed by evtest (see main.go wizard-input.log capture).
	// Contract today: it falls to defaultKeyMap (the Miyoo/SDL Xbox layout). Assert that exact
	// fallback so a future lane split can't silently reroute it, and flag the on-device gap.
	// VERIFIED on-device: PENDING — confirm my355/Flip physical A/B + d-pad + shoulder codes via
	//                      the wizard-input.log evtest capture, then give it its own lane+map here.
	for _, k := range []string{"LODOR_HOST_OS", "LODOR_FBHELPER", "LODOR_FORCE_RAWFB"} {
		t.Setenv(k, "")
	}
	for _, host := range []string{"lodoros", ""} {
		t.Setenv("LODOR_HOST_OS", host)
		got := inputKeyMap()
		// Same map identity: probe the A/B-distinguishing code 304 (Confirm on default, Back on
		// the Nintendo-layout maps) plus 315 (Start on default, unmapped on nextui).
		if got[304] != ui.BtnConfirm || got[315] != ui.BtnStart {
			t.Errorf("my355/Flip host=%q must fall to defaultKeyMap (304=Confirm,315=Start); got 304=%v 315=%v",
				host, got[304], got[315])
		}
	}
}

// TestSDLLaneActive: the NextUI host and an explicit helper path select the SDL lane; a raw
// host and the force-rawfb escape hatch do not.
func TestSDLLaneActive(t *testing.T) {
	// clean env for a deterministic table
	for _, k := range []string{"LODOR_HOST_OS", "LODOR_FBHELPER", "LODOR_FORCE_RAWFB"} {
		t.Setenv(k, "")
	}

	cases := []struct {
		hostOS, helper, force string
		want                  bool
	}{
		{"nextui", "", "", true},              // NextUI => SDL
		{"", "/pak/lodor-fbhelper", "", true}, // explicit helper => SDL
		{"muos", "", "", false},               // muOS raw-fb device
		{"", "", "", false},                   // bare / LodorOS raw-fb
		{"nextui", "", "1", false},            // force-rawfb escape hatch wins
	}
	for _, c := range cases {
		t.Setenv("LODOR_HOST_OS", c.hostOS)
		t.Setenv("LODOR_FBHELPER", c.helper)
		t.Setenv("LODOR_FORCE_RAWFB", c.force)
		if got := sdlLaneActive(); got != c.want {
			t.Errorf("sdlLaneActive(host=%q helper=%q force=%q) = %v, want %v",
				c.hostOS, c.helper, c.force, got, c.want)
		}
	}
}

// TestSDLPanelSize: defaults to 1280x720, overridable via LODOR_SPIKE_W/H.
func TestSDLPanelSize(t *testing.T) {
	t.Setenv("LODOR_SPIKE_W", "")
	t.Setenv("LODOR_SPIKE_H", "")
	if w, h := sdlPanelSize(); w != 1280 || h != 720 {
		t.Errorf("default panel = %dx%d, want 1280x720", w, h)
	}
	t.Setenv("LODOR_SPIKE_W", "640")
	t.Setenv("LODOR_SPIKE_H", "480")
	if w, h := sdlPanelSize(); w != 640 || h != 480 {
		t.Errorf("override panel = %dx%d, want 640x480", w, h)
	}
}

// TestWatchdogFiresWhenIdle: with no kick, the watchdog runs its onExpire exactly once.
func TestWatchdogFiresWhenIdle(t *testing.T) {
	var fired int32
	done := make(chan struct{})
	newWatchdog("test", 40*time.Millisecond, func() {
		atomic.AddInt32(&fired, 1)
		close(done)
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never fired")
	}
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("fired %d times, want 1", got)
	}
}

// TestWatchdogKickDefersExpiry: kicking keeps the watchdog from firing while active input
// continues; it fires only after the kicks stop.
func TestWatchdogKickDefersExpiry(t *testing.T) {
	var fired int32
	wd := newWatchdog("test", 60*time.Millisecond, func() { atomic.AddInt32(&fired, 1) })
	// kick every 20ms for 180ms — must NOT fire during that window
	for i := 0; i < 9; i++ {
		time.Sleep(20 * time.Millisecond)
		wd.kick()
	}
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("watchdog fired %d times while being kicked", got)
	}
	// stop kicking -> it fires
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("watchdog fired %d times after kicks stopped, want 1", got)
	}
}

// TestWatchdogStopDisarms: a card that exits on its own (chosen Play) must not later fire.
func TestWatchdogStopDisarms(t *testing.T) {
	var fired int32
	wd := newWatchdog("test", 40*time.Millisecond, func() { atomic.AddInt32(&fired, 1) })
	wd.stop()
	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Errorf("stopped watchdog still fired %d times", got)
	}
}

// TestWatchdogPauseSuppressesFireDuringMutatingOp (lodor#63): while a state-mutating engine
// op is in flight, the sub-view wraps it in pause()/resume(). The watchdog MUST NOT fire
// (os.Exit(0) mid-write orphans the engine child and corrupts the save), even when the op
// runs LONGER than the idle timeout — the whole point of the bug. After resume(), a fresh
// idle window must still fire on a genuine walk-away so the "never trap the user" guarantee
// is only deferred, never dropped.
func TestWatchdogPauseSuppressesFireDuringMutatingOp(t *testing.T) {
	var fired int32
	wd := newWatchdog("test", 40*time.Millisecond, func() { atomic.AddInt32(&fired, 1) })

	// A mutating op longer than the timeout: pause, "write" for 3x the idle window, resume.
	wd.pause()
	time.Sleep(120 * time.Millisecond) // 3x timeout — would have fired several times unguarded
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("watchdog fired %d times during a paused (in-flight) mutating op", got)
	}
	wd.resume()

	// resume() re-armed a fresh idle window — a genuine walk-away must still fire exactly once.
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("watchdog fired %d times after resume + idle, want 1", got)
	}
}

// TestWatchdogPauseNested (lodor#63): pause/resume is depth-counted, so a mutating op nested
// inside another (or paranoidly double-wrapped) stays disarmed until the OUTER resume. Only
// when the depth returns to zero does the idle window re-arm and fire.
func TestWatchdogPauseNested(t *testing.T) {
	var fired int32
	wd := newWatchdog("test", 40*time.Millisecond, func() { atomic.AddInt32(&fired, 1) })
	wd.pause()
	wd.pause()
	wd.resume() // depth back to 1 — still disarmed
	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("watchdog fired while still nested-paused (depth 1)")
	}
	wd.resume() // depth 0 — re-armed
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("watchdog fired %d times after outer resume, want 1", got)
	}
}

// TestWatchdogResumeReArmsCleanlyAfterKick (lodor#63): resume() must not double-arm or race
// with an intervening kick — after resume the timer behaves like a freshly kicked watchdog.
func TestWatchdogResumeReArmsCleanlyAfterKick(t *testing.T) {
	var fired int32
	wd := newWatchdog("test", 60*time.Millisecond, func() { atomic.AddInt32(&fired, 1) })
	wd.pause()
	wd.resume()
	// Keep it alive with kicks past two idle windows — resume left a normal armed timer.
	for i := 0; i < 6; i++ {
		time.Sleep(20 * time.Millisecond)
		wd.kick()
	}
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("watchdog fired %d times while kicked after resume", got)
	}
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("watchdog fired %d times after kicks stopped, want 1", got)
	}
}

// TestCardIdleTimeout: default 30s, overridable, hostile value falls back.
func TestCardIdleTimeout(t *testing.T) {
	t.Setenv("LODOR_CARD_IDLE_SECS", "")
	if got := cardIdleTimeout(); got != 30*time.Second {
		t.Errorf("default = %v, want 30s", got)
	}
	t.Setenv("LODOR_CARD_IDLE_SECS", "10")
	if got := cardIdleTimeout(); got != 10*time.Second {
		t.Errorf("override = %v, want 10s", got)
	}
	t.Setenv("LODOR_CARD_IDLE_SECS", "-5")
	if got := cardIdleTimeout(); got != 30*time.Second {
		t.Errorf("hostile value = %v, want fallback 30s", got)
	}
	_ = os.Environ
}
