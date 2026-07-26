package main

// launchcard.go — the Handoff launch gate (task #24, expanded to the full
// per-game card by task launch-card-v2; model/render/sub-views live in
// launchcardv2.go).
//
// Called by a lane's pre-launch hook, reachability-gated. TWO TRIGGER MODES:
//
//	SMART (default): probes FIRST with zero fb/input cost; the card appears
//	ONLY when the server actually has something newer (his call: silent
//	instant pass-through otherwise). Both probes are content-based, no
//	clock trust:
//	  saves:  --list-saves LOCAL= trailer == "older" (content-hash lineage)
//	  states: --list-states rows with compat=1 AND known=0 (a compatible
//	          server state this device's ledger has never seen = news)
//
//	SUMMONED (--summoned flag or LODOR_LAUNCH_SUMMONED=1): the user asked
//	for the card (hold-to-summon / LodorOS Y) — show the FULL card
//	regardless of news, even offline (local info, honest "unreachable").
//
// Anything unreachable or failed probes: smart stays quiet; summoned shows
// what it honestly knows. Exit is ALWAYS 0 — a launch is never blocked, and
// the hook's outer timeout bounds a user who walks away.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lodor/playtime"
	"lodor/ui"
)

func (w *wizard) launchCard(path string, summoned bool) int {
	mode := "smart"
	if summoned {
		mode = "summoned"
	}
	savesOut, sErr := w.runEngine("--list-saves", path)
	statesOut, stErr := w.runEngine("--list-states", path)
	if !summoned && !cardHasNews(savesOut, sErr, statesOut, stErr) {
		fmt.Println("LAUNCHCARD mode=smart news=0")
		return 0
	}

	// The card earns its screen time (news, or the user summoned it). Any UI
	// failure from here degrades to "launch anyway", loudly in the log.
	//
	// DISPLAY LANE SPLIT (launch-card-v2 Phase B): NextUI panels (tg5040/tg5050/my355)
	// present through the SDL helper — raw /dev/fb0 is a dead scanout surface there; every
	// other device keeps the proven raw-fb blit. INPUT is ui.EvdevSource on BOTH lanes.
	dumpPath := os.Getenv("LODOR_FB_DUMP")
	var draw func(c *ui.Canvas)
	var in ui.InputSource
	if sdlLaneActive() {
		h := w.openCardSDLHelper()
		if h == nil {
			fmt.Println("LAUNCHCARD mode=" + mode + " shown=0 reason=no-sdl-helper")
			return 0
		}
		defer h.Close()
		draw = func(c *ui.Canvas) {
			_ = presentToHelper(h, c) // helper died => launch anyway (loop still bounded by watchdog)
		}
		// INPUT on the SDL lane: MinUI's SDL opens+GRABS the keypad, so a separate evdev reader is
		// starved (rc7: card rendered, zero presses, watchdog at 30s). The helper's SDL owns the
		// keypad and forwards its key presses, so the helper IS the input source here.
		if h.Buttons() == nil {
			fmt.Println("LAUNCHCARD mode=" + mode + " shown=0 reason=no-input")
			return 0
		}
		in = h
		w.logPhase("input: sdl-helper (forwarded keypad)")
	} else {
		fb, err := ui.OpenFramebuffer("/dev/fb0")
		if err != nil {
			// Name the sub-cause (open vs FBIOGET ioctl vs mmap) — the bare "no-fb" cost four RCs of
			// blind guessing on the SSD202D. err is otherwise swallowed here.
			fmt.Println("LAUNCHCARD mode=" + mode + " shown=0 reason=no-fb err=" + err.Error())
			return 0
		}
		defer fb.Close()
		w.phaseFB(fb.Xres(), fb.Yres(), fb.Bpp()) // real panel in the field log (TrimUI 1280x720 fix)
		draw = func(c *ui.Canvas) {
			presentCanvas(fb, c) // panel-scaled — NEVER raw Flush (TrimUI corner-render bug)
			if dumpPath != "" {
				_ = fb.SavePNG(dumpPath)
			}
		}
		ei, err := w.openInput() // raw-fb lanes: the wizard's own evdev reader (no SDL grab)
		if err != nil {
			fmt.Println("LAUNCHCARD mode=" + mode + " shown=0 reason=no-input")
			return 0
		}
		in = ei
	}
	defer in.Close()
	w.in = in

	// WATCHDOG (never trap the user): the Smart Pro locked up when input died with no way
	// off the card. Now, no press for the idle timeout self-exits the process (os.Exit(0) =>
	// the game launches). btn() kicks it on every real press; it is armed even if input is
	// completely dead, so a frozen screen is impossible.
	wd := newWatchdog("card", cardIdleTimeout(), nil)
	defer wd.stop()
	w.wd = wd        // sub-views pause/resume this around mutating engine ops (lodor#63)
	defer func() { w.wd = nil }()
	btn := func() ui.Button {
		b := <-in.Buttons()
		wd.kick()
		return b
	}

	present, total := cardDiscCounts(path)
	m := buildCardModel(path, savesOut, sErr, statesOut, stErr,
		isDownloaded(path), present, total, playtimeLineFor(filepath.Base(path)))
	var cov *ui.Canvas
	if p := cardCoverPath(path); p != "" {
		cov, _ = ui.LoadImageCanvas(p) // nil on decode failure -> placeholder
	}
	w.runCardLoop(m, cov, draw, btn)
	evicted := "0"
	if m.evicted {
		evicted = "1"
	}
	fmt.Println("LAUNCHCARD mode=" + mode + " shown=1 action=play evicted=" + evicted)
	return 0
}

// flash draws one message frame and holds it briefly — post-action feedback
// that never demands a button press (the game is about to take the screen).
func (w *wizard) flash(draw func(*ui.Canvas), body string, col ui.Color) {
	c := ui.NewCanvas(W, H)
	x, y, _, _ := w.t.Frame(c, "Lodor", "")
	c.DrawText(x, y, body, col, w.t.BodyScale)
	draw(c)
	time.Sleep(1200 * time.Millisecond)
}

// listSavesLocal extracts --list-saves' single-field LOCAL= trailer
// (none|current|older|unpushed); "" when absent.
func listSavesLocal(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "LOCAL=") && !strings.Contains(ln, "\t") {
			return strings.TrimPrefix(ln, "LOCAL=")
		}
	}
	return ""
}

type stateCandidate struct {
	id, label string
	age       int64
}

// newestUnknownCompatState picks the newest compatible server state this
// device's ledger doesn't know (compat=1 known=0) from --list-states output.
func newestUnknownCompatState(out string) (stateCandidate, bool) {
	var best stateCandidate
	found := false
	for _, ln := range strings.Split(out, "\n") {
		if !strings.HasPrefix(ln, "LISTSTATE ") {
			continue
		}
		kv := parseStateLine(ln)
		if kv["id"] == "" || kv["compat"] != "1" || kv["known"] != "0" {
			continue
		}
		age, err := strconv.ParseInt(kv["age"], 10, 64)
		if err != nil {
			age = 1 << 62
		}
		if !found || age < best.age {
			slot := "Slot " + kv["slot"]
			if kv["slot"] == "auto" {
				slot = "Auto-resume"
			}
			best = stateCandidate{id: kv["id"], age: age,
				label: slot + " - " + humanAge(age) + " - " + originLabel(kv["origin"])}
			found = true
		}
	}
	return best, found
}

// playtimeLineFor renders "Played 4h 12m across N sessions" from the local
// playtime roll-up (totals.tsv: key \t rom \t secs \t plays \t last_utc).
// Best-effort: no file / no row / zero time = no line.
func playtimeLineFor(romBase string) string {
	data, err := os.ReadFile(playtime.TotalsTSVPath())
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(data), "\n") {
		f := strings.Split(ln, "\t")
		if len(f) < 4 || f[1] != romBase {
			continue
		}
		secs, _ := strconv.ParseInt(f[2], 10, 64)
		plays, _ := strconv.ParseInt(f[3], 10, 64)
		if secs <= 0 {
			return ""
		}
		line := "Played " + humanDur(secs)
		if plays == 1 {
			return line + " in 1 session"
		}
		return line + " across " + strconv.FormatInt(plays, 10) + " sessions"
	}
	return ""
}

func humanDur(secs int64) string {
	h, m := secs/3600, (secs%3600)/60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return "under a minute"
	}
}
