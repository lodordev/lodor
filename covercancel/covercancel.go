// Package covercancel is the tiny cross-process cancel signal for the box-art cover
// fetch — the ONLY thing it governs. Box art is cosmetic; nothing here ever touches a
// game, save, or state file. A bug in this package can, at worst, cancel a cover fetch
// too eagerly or too late — never lose data.
//
// Mechanism: a sentinel FILE. The launcher (C) touches it when the user presses B to
// cancel a "Refresh box art / Refresh Library" run; the engine's cover loop stats it
// BETWEEN covers and stops promptly, and — for the cover HTTP request already in flight
// — derives a context.Context that is cancelled the instant the sentinel appears, so a
// slow-radio download aborts instead of waiting out the network. A file (not a signal or
// a pipe) is used because the C launcher runs the engine as a detached/backgrounded
// child through a shell wrapper: a file is the one channel both sides trivially share,
// and `touch`/`rm` are atomic enough for a boolean.
//
// The path lives in /tmp (tmpfs, wiped on reboot) so a stale sentinel can never survive
// to cancel a future run.
package covercancel

import (
	"context"
	"os"
	"time"
)

// Path is the sentinel file both sides agree on. The launcher touches it on B-press
// (Lodor_runWithProgress) and removes it before/after each run; the engine stats it.
const Path = "/tmp/lodor-cover-cancel"

// pathOverride lets tests redirect the sentinel to a temp file so they never touch the
// real /tmp path or race a concurrent run. Empty in production — path() returns Path.
var pathOverride string

func path() string {
	if pathOverride != "" {
		return pathOverride
	}
	return Path
}

// pollInterval is how often Watch re-stats the sentinel to cancel an in-flight request.
// 200ms is well under the "reads as a hang" threshold on the slow Miyoo radio while
// costing nothing (a single stat of a tmpfs path).
const pollInterval = 200 * time.Millisecond

// Requested reports whether a cancel has been signalled (the sentinel exists). Cheap
// enough to call between every cover. A stat error other than "exists" is treated as
// "not cancelled" — we never abort a cosmetic fetch on an ambiguous filesystem hiccup.
func Requested() bool {
	_, err := os.Stat(path())
	return err == nil
}

// Clear removes the sentinel. The engine calls this at the start of a cover-bearing run
// so a leftover sentinel from a previous cancel can't instantly abort a fresh run; the
// launcher also clears it around each run. Best-effort — a missing file is success.
func Clear() {
	_ = os.Remove(path())
}

// Request creates the sentinel (used by tests and any Go-side caller that wants to
// cancel). Best-effort; the file's mere existence is the signal, contents are ignored.
func Request() error {
	f, err := os.OpenFile(path(), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// WithSignal returns a child context that is cancelled when the PARENT is cancelled OR
// the cancel sentinel appears (whichever comes first), plus a cancel func the caller
// MUST defer to stop the watcher goroutine and release resources. This is how an
// in-flight cover download becomes interruptible: the HTTP request runs under this
// context, so the moment the launcher touches the sentinel the request's transport
// aborts the connection instead of blocking on the slow radio.
//
// If the sentinel already exists when called, the returned context is cancelled
// immediately (no request is started).
func WithSignal(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if Requested() {
		cancel()
		return ctx, cancel
	}
	go func() {
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return // parent cancelled / request finished — nothing to watch
			case <-t.C:
				if Requested() {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}
