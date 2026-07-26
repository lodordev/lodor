package main

// watchdog.go — the "never trap the user" guard (launch-card-v2 Phase A fix, 2026-07-12).
//
// The Smart Pro flash locked up: the SDL lane displayed a frame but took ZERO input, and
// there was no way off the screen — the device was wedged. That must be IMPOSSIBLE. Every
// interactive card/spike loop arms a watchdog: if no button arrives for idleTimeout, the
// watchdog logs one line and hard-exits the process (os.Exit(0)). Because it is a plain
// timer goroutine that owns its own exit, it fires EVEN IF input is completely dead — so a
// future input regression can never again trap the user on a frozen screen; the game/menu
// just proceeds. Real presses call kick() to disarm-and-rearm.
//
// os.Exit(0) is deliberate: a launch is never blocked (exit 0), and skipping deferred
// fb/helper Close() is safe — the SDL helper exits on its stdin EOF / our process death and
// never blanks the panel, and the OS reaps fds. The guarantee (always escapes) outranks
// tidy teardown.
//
// PAUSE/RESUME (lodor#63, 2026-07-12): the auto-exit is os.Exit(0) — it runs NO deferreds and
// (no Setpgid/Pdeathsig) leaves a running engine child orphaned. If it fires while a sub-view
// is mid-write on a state-mutating engine op (--restore-save / --pull-state / --download /
// --evict), the game launches and opens the SAME save the orphaned child is still writing =>
// a corrupted save. So a mutating op brackets itself in pause()/resume(): pause stops the
// timer AND raises a depth flag the fire path re-checks UNDER THE LOCK, so even a fire that
// races past timer.Stop() is suppressed; resume re-arms a fresh idle window (a kick), so the
// "never trap the user" guarantee is only ever deferred across a bounded op, never dropped.

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// defaultCardIdleTimeout bounds how long the card/spike waits on a silent user before it
// self-exits. 30s: long enough to read the card and decide, short enough that a walk-away
// (or a dead-input regression) frees the device fast. Overridable via LODOR_CARD_IDLE_SECS
// (0 or unset => default; a hostile value falls back to the default).
const defaultCardIdleTimeout = 30 * time.Second

func cardIdleTimeout() time.Duration {
	if n := envInt("LODOR_CARD_IDLE_SECS", 0); n > 0 {
		return time.Duration(n) * time.Second
	}
	return defaultCardIdleTimeout
}

// watchdog fires exit after idle with no kick(). Concurrency-safe: kick() may be called from
// the loop goroutine while the timer fires on its own.
type watchdog struct {
	mu      sync.Mutex
	timer   *time.Timer
	timeout time.Duration
	tag     string // log context, e.g. "card" / "spike"
	fired   bool
	paused  int // pause depth: >0 => idle timer disarmed for an in-flight mutating op (lodor#63)
}

// newWatchdog arms a watchdog that runs onExpire once after timeout with no kick. If onExpire
// is nil, the default action (log + os.Exit(0)) is used. Returns the armed watchdog.
func newWatchdog(tag string, timeout time.Duration, onExpire func()) *watchdog {
	w := &watchdog{timeout: timeout, tag: tag}
	if onExpire == nil {
		onExpire = func() {
			fmt.Printf("LAUNCHCARD watchdog=fired tag=%s idle=%s action=auto-exit\n", tag, timeout)
			os.Exit(0)
		}
	}
	w.mu.Lock()
	w.timer = time.AfterFunc(timeout, func() {
		w.mu.Lock()
		// paused re-check closes the race where the timer fires just as a mutating op
		// begins: pause() stops the timer, but an already-scheduled callback can still
		// run — swallow it here so os.Exit(0) never lands mid-write (lodor#63).
		if w.fired || w.paused > 0 {
			w.mu.Unlock()
			return
		}
		w.fired = true
		w.mu.Unlock()
		onExpire()
	})
	w.mu.Unlock()
	return w
}

// kick resets the idle clock on a real press. No-op once fired.
func (w *watchdog) kick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired || w.timer == nil {
		return
	}
	w.timer.Reset(w.timeout)
}

// pause disarms the idle timer for the duration of an in-flight state-mutating engine op so
// the auto-exit can't fire mid-write (lodor#63). Re-entrant via a depth counter — nested
// pauses are safe. No-op once fired (the process is already on its way out).
func (w *watchdog) pause() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired {
		return
	}
	w.paused++
	if w.timer != nil {
		w.timer.Stop()
	}
}

// resume undoes one pause(). When the last pause is lifted it re-arms a fresh idle window
// (an implicit kick — the op just finished, so the user gets the full timeout again).
// No-op once fired.
func (w *watchdog) resume() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired || w.paused == 0 {
		return
	}
	w.paused--
	if w.paused == 0 && w.timer != nil {
		w.timer.Reset(w.timeout)
	}
}

// stop disarms the watchdog (card is exiting on its own terms — a chosen Play/Back).
func (w *watchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fired = true
	if w.timer != nil {
		w.timer.Stop()
	}
}
