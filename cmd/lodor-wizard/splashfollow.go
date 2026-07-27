package main

// splashfollow.go — the LIVE download overlay for the launch path.
//
// Why this exists: the muOS launch override painted a single static frame and never
// updated it, so a download that was working but slow looked identical to a hang. On
// 2026-07-26 a 16 MB GBA fetch took 113 s at 145 KB/s (the same card had done 2.0 MB/s
// four minutes after the daemon settled) and the screen showed one unchanging line the
// whole time. The engine was already publishing real byte-level progress; nothing was
// reading it.
//
// Contract: hold /dev/fb0 open, repaint on a ~1 s tick from the engine's side-channels,
// and exit on ANY of: the stop file appearing, stdin closing, or the hard cap. The exits
// are deliberately redundant — a follower that outlived its download would keep the
// framebuffer and block the stock launcher handoff, which is worse than no overlay.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"lodor/ui"
)

// followTick is the repaint interval. Fast enough to read as alive, slow enough to cost
// nothing next to a transfer (one small canvas blit per second).
const followTick = 1 * time.Second

// followMaxDuration bounds a stranded follower. Longer than any plausible ROM fetch on a
// bad link; short enough that a bug cannot hold the panel indefinitely.
const followMaxDuration = 30 * time.Minute

// readBytes parses the engine's "<done>/<total>" byte side-channel. Returns 0,0 when the
// file is absent or malformed — the caller then renders percent only, never a fake size.
func readBytes() (int64, int64) {
	b, err := os.ReadFile(sideChannelFile("dl-bytes"))
	if err != nil {
		return 0, 0
	}
	parts := strings.SplitN(strings.TrimSpace(string(b)), "/", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	done, err1 := strconv.ParseInt(parts[0], 10, 64)
	total, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || done < 0 || total < 0 {
		return 0, 0
	}
	return done, total
}

// sideChannelFile mirrors the engine's side-channel location rule (LODOR_PROGRESS_DIR,
// else /tmp) so the overlay reads exactly what the engine wrote.
func sideChannelFile(name string) string {
	if d := os.Getenv("LODOR_PROGRESS_DIR"); d != "" {
		return d + "/" + name
	}
	return "/tmp/" + name
}

// humanBytes renders a byte count at one decimal place. Deliberately simple: this is a
// progress caption on a 720x480 panel, not a report.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// transferStatus composes the live caption. PURE (no fb, no clock) so the formatting and
// the rate maths are unit-testable without hardware.
//
// Honesty rules, in order:
//   - no bytes yet -> percent only (or "starting…" when there is not even a percent), so
//     the overlay never invents a size it does not have;
//   - rate only once at least a second has passed AND bytes have moved — a rate computed
//     over a sub-second window is noise;
//   - elapsed is always shown: it is the one number that proves the screen is live.
func transferStatus(pct int, done, total int64, elapsed time.Duration) string {
	var parts []string
	if pct >= 0 {
		parts = append(parts, fmt.Sprintf("%d%%", pct))
	}
	if total > 0 {
		parts = append(parts, humanBytes(done)+" / "+humanBytes(total))
	} else if done > 0 {
		parts = append(parts, humanBytes(done))
	}
	secs := elapsed.Seconds()
	if secs >= 1 && done > 0 {
		parts = append(parts, humanBytes(int64(float64(done)/secs))+"/s")
	}
	if secs >= 1 {
		parts = append(parts, fmt.Sprintf("%ds", int(secs)))
	}
	if len(parts) == 0 {
		return "starting..."
	}
	return strings.Join(parts, "  -  ")
}

// splashFollow renders the splash and keeps it current until told to stop.
// Returns 1 only when the framebuffer could not be opened (same contract as --splash);
// a normal stop is 0.
func (w *wizard) splashFollow(title, body, tone, stopPath string) int {
	fb, err := ui.OpenFramebuffer("/dev/fb0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "splash-follow: no framebuffer:", err)
		return 1
	}
	defer fb.Close()

	// stdin closing is the primary stop signal: it fires even if the caller is killed
	// without cleaning up its stop file.
	stdinClosed := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		for {
			if _, rErr := os.Stdin.Read(buf); rErr != nil {
				close(stdinClosed)
				return
			}
		}
	}()

	start := time.Now()
	deadline := start.Add(followMaxDuration)
	for {
		pct := readPct()
		done, total := readBytes()
		// A phase update from the engine (e.g. "Verifying...") outranks the caller's
		// static body: it is the more specific truth about what is happening now.
		line := body
		if ph := readPhase(); ph != "" {
			line = ph
		}
		w.paintSplash(fb, title, line, tone, transferStatus(pct, done, total, time.Since(start)))

		if stopPath != "" {
			if _, sErr := os.Stat(stopPath); sErr == nil {
				return 0
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "splash-follow: hit the duration cap, releasing the framebuffer")
			return 0
		}
		select {
		case <-stdinClosed:
			return 0
		case <-time.After(followTick):
		}
	}
}
