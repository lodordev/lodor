package main

// launchcard.go — the Handoff launch gate (task #24, Jonathan 2026-07-07:
// "the game manager is a crutch — make the launch hook interactive").
//
// Called by a lane's pre-launch hook, reachability-gated. Probes FIRST with
// zero fb/input cost; the card appears ONLY when the server actually has
// something newer than this device (his call: silent instant pass-through
// otherwise). Both probes are content-based — no clock trust:
//
//	saves:  --list-saves LOCAL= trailer == "older"  (content-hash lineage)
//	states: --list-states rows with compat=1 AND known=0 (a compatible server
//	        state this device's ledger has never seen = made elsewhere = news)
//
// Anything unreachable or failed probes quiet. Exit is ALWAYS 0 — a launch is
// never blocked, and the hook's outer timeout bounds a user who walks away.

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

func (w *wizard) launchCard(path string) int {
	savesOut, sErr := w.runEngine("--list-saves", path)
	saveNewer := sErr == nil && listSavesLocal(savesOut) == "older"
	statesOut, stErr := w.runEngine("--list-states", path)
	best, hasState := newestUnknownCompatState(statesOut)
	if stErr != nil {
		hasState = false
	}
	if !saveNewer && !hasState {
		fmt.Println("LAUNCHCARD news=0")
		return 0
	}

	// Something is genuinely newer — the card earns its screen time. Any UI
	// failure from here degrades to "launch anyway", loudly in the log.
	fb, err := ui.OpenFramebuffer("/dev/fb0")
	if err != nil {
		fmt.Println("LAUNCHCARD news=1 shown=0 reason=no-fb")
		return 0
	}
	defer fb.Close()
	in, err := w.openInput()
	if err != nil {
		fmt.Println("LAUNCHCARD news=1 shown=0 reason=no-input")
		return 0
	}
	defer in.Close()
	w.in = in
	dumpPath := os.Getenv("LODOR_FB_DUMP")
	draw := func(c *ui.Canvas) {
		fb.Flush(c)
		if dumpPath != "" {
			_ = fb.SavePNG(dumpPath)
		}
	}
	btn := func() ui.Button { return <-in.Buttons() }

	name := filepath.Base(path)
	var info []string
	if pt := playtimeLineFor(name); pt != "" {
		info = append(info, pt)
	}
	if saveNewer {
		info = append(info, "A newer save is on your server.")
	}
	if hasState {
		info = append(info, "Newer state: "+best.label)
	}
	var opts []string
	if hasState {
		opts = append(opts, "Continue from that state")
	}
	if saveNewer {
		opts = append(opts, "Pull the newer save")
	}
	opts = append(opts, "Just play")

	m := &ui.Menu{Items: opts}
	choice := -1
	for {
		c := ui.NewCanvas(W, H)
		x, y, ww, hh := w.t.Frame(c, name, "Up/Down: move   A: select   B: just play")
		yy := y
		for _, ln := range info {
			c.DrawText(x, yy, ln, w.t.Text, w.t.BodyScale)
			yy += 34
		}
		m.Draw(c, w.t, x, yy+16, ww, hh-(yy-y)-16)
		draw(c)
		b := btn()
		if b == ui.BtnBack {
			break
		}
		if m.Handle(b) {
			choice = m.Selected()
			break
		}
	}
	act := "Just play"
	if choice >= 0 {
		act = opts[choice]
	}
	switch act {
	case "Continue from that state":
		w.working(draw, "Placing state...")
		pout, perr := w.runEngine("--pull-state", path, "--state-id", best.id)
		if exitCode(perr) == 0 && ui.ResultToken(pout, "placedstate") == "1" {
			w.flash(draw, "State placed - load it from the in-game menu.", w.t.Good)
			fmt.Println("LAUNCHCARD news=1 action=pull-state placed=1")
		} else {
			w.flash(draw, "Couldn't place the state - launching anyway.", w.t.Bad)
			fmt.Println("LAUNCHCARD news=1 action=pull-state placed=0 reason=" + ui.ResultToken(pout, "reason"))
		}
	case "Pull the newer save":
		w.working(draw, "Pulling your newer save...")
		sout, serr := w.runEngine("--sync-save", path)
		if exitCode(serr) == 0 && ui.ResultToken(sout, "pulled") == "1" {
			w.flash(draw, "Save pulled.", w.t.Good)
			fmt.Println("LAUNCHCARD news=1 action=pull-save pulled=1")
		} else {
			w.flash(draw, "Couldn't pull the save - launching with your local one.", w.t.Bad)
			fmt.Println("LAUNCHCARD news=1 action=pull-save pulled=0")
		}
	default:
		fmt.Println("LAUNCHCARD news=1 action=play")
	}
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
