// Command lodor-wizard is the on-device onboarding UI for Lodor-MuOS. It renders to
// /dev/fb0 and reads /dev/input/event* (pure-Go, CGO-free, stdlib only - no SDL), and
// drives the headless engine (lodor-sync) for the actual RomM work. Wi-Fi entry stays
// muOS's job (Principle 1): the wizard verifies connectivity and points the user at
// muOS Settings if down, owning only the RomM-specific steps.
//
// Modes:
//
//	(default)         interactive - fb0 + evdev, runs the engine, writes config.json.
//	--capture <dir>   render every screen with representative state to PNG (off-hardware
//	                  verification). No fb, no input, no engine calls.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"lodor/buildinfo"
	"lodor/fsutil"
	"lodor/ui"
)

const W, H = 720, 480

// Menu first-paint settle (RG34XX display-handoff fix, 2026-07-04). Unlike onboarding — which
// redraws continuously as Wi-Fi/pairing animate and so naturally wins the panel — the main menu
// draws ONE frame and blocks on input. If muOS composites its "Loading Application" overlay (or
// re-pans the panel) a beat AFTER our first blit, that single frame loses and the menu is drawn
// but never shown. So on the FIRST menu paint we re-blit the same frame a handful of times over a
// short, bounded window (each Flush also re-pans page 0 to the front) to win the display, THEN
// fall through to the blocking input loop. Bounded + cheap: fixed frame count, no busy-spin.
const (
	settleFrames   = 6
	settleInterval = 110 * time.Millisecond // ~0.66s total; one-time, only at menu entry
)

// Subprocess timeouts (BUG 2b hardening): NOTHING the wizard shells may freeze the UI. The
// menu-build probe (tsAvailable) uses the SHORT ceiling and degrades to false on timeout so
// the FIRST menu paint can never block; the long TS shim ops (reconnect/up-interactive) and
// the engine get generous wedge-breaker ceilings — never a tight bound on legit long work.
const (
	tsProbeTimeout = 6 * time.Second  // menu-build probes: available/status/ip — must be quick
	tsShimTimeout  = 90 * time.Second // reconnect/up-interactive have internal ~45s waits
	engineTimeout  = 60 * time.Minute // engine safety net (downloads/mirror can be long)
	seedTimeout    = 2 * time.Minute  // lodor-seed.sh post-mirror re-seed (O(folders), seconds)
)

// timeoutErr maps a context deadline into an honest error the caller renders (no fake success).
func timeoutErr(ctx context.Context, err error, d time.Duration) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %s", d)
	}
	return err
}

type step int

const (
	stepWelcome step = iota
	stepWifi
	stepServer
	stepCode
	stepDevice
	stepValidate
	stepMirror
	stepDone
	stepError
	stepConnect  // choose Tailscale vs plain-LAN/public URL (only when Tailscale is available)
	stepTsSignin // interactive Tailscale sign-in: show the login URL, poll to connected
)

type wizard struct {
	t       ui.Theme
	server  string
	code    string
	device  string
	wifiUp  bool
	reach   bool
	auth    bool
	errMsg  string
	mirrorN string
	bin     string // path to lodor-sync
	dataDir string
	retry   step // where an error screen's "B: try again" returns to

	in           ui.InputSource // live evdev source (nil off-hardware / in the sim tests)
	tsAvail      bool           // Tailscale bundled + device capable (shim `available` == 0)
	useTailscale bool           // user chose the Tailscale path at the Connect step
	tsURL        string         // current Tailscale login URL (shown for the browser sign-in)
	tsPhase      string         // honest one-line status on the sign-in screen
	registered   bool           // lodor#39: --register-device outcome, a validate-screen row
}

// action is what a message screen reports back: advance (A/Start) or back (B). Every
// screen honours B so the user can always walk back out to muOS (blocker #170).
type action int

const (
	actAdvance action = iota
	actBack
)

const wizGlyphH = 8 // mirrors ui.glyphH for layout math

func main() {
	args := os.Args[1:]
	w := &wizard{t: ui.DefaultTheme(), device: defaultDeviceName(), server: "https://"}
	w.bin = engineBin()
	w.dataDir = dataDir()

	if len(args) >= 1 && args[0] == "--capture" {
		dir := "."
		if len(args) >= 2 {
			dir = args[1]
		}
		w.capture(dir)
		return
	}
	// --phase-selftest: emit the EXACT startup phase-line sequence (BUG 2a) to wizard.log +
	// stderr WITHOUT opening fb/input or shelling anything — so the integ harness can assert,
	// on the real built binary, that the instrumentation fires. Not the interactive UI; a
	// deterministic replay of the same phase strings the startup path uses.
	if len(args) >= 1 && args[0] == "--phase-selftest" {
		w.phaseSelftest()
		return
	}
	// --reseed: run the host launch-override seeder exactly as the post-mirror hook does
	// (#180A) and exit. Field diagnostic + harness surface: proves the wizard->seeder seam
	// on the built binary without fb/input. No fb, no engine calls.
	if len(args) >= 1 && args[0] == "--reseed" {
		w.reseedOverrides()
		return
	}
	// --splash <title> <body> [good|bad]: draw ONE full-screen message to /dev/fb0 and exit.
	// The launch override calls this for honest on-screen feedback during download-on-launch
	// (user feedback #6: "no feedback when clicking/loading a game"). No input loop, no engine
	// calls; best-effort — a non-openable fb returns non-zero for the caller to log.
	// --launch-card <rom>: the Handoff launch gate (launchcard.go). Probes first
	// with no fb/input; the interactive card appears ONLY when the server has
	// something newer. Always exits 0 — a launch is never blocked.
	if len(args) >= 2 && args[0] == "--launch-card" {
		os.Exit(w.launchCard(args[1]))
	}
	if len(args) >= 1 && args[0] == "--splash" {
		title, body, tone := "", "", ""
		if len(args) >= 2 {
			title = args[1]
		}
		if len(args) >= 3 {
			body = args[2]
		}
		if len(args) >= 4 {
			tone = args[3]
		}
		os.Exit(w.splash(title, body, tone))
	}
	w.runInteractive()
}

// splash draws one full-screen message to /dev/fb0 and returns immediately (no input loop).
// Honest by construction (feedback_no_fake_ui_state): callers pass real state only, and a
// framebuffer that can't be opened yields a non-zero exit the caller logs — never a fake
// success. tone picks the body color/footer: "good", "bad" (returning to menu), or default.
func (w *wizard) splash(title, body, tone string) int {
	fb, err := ui.OpenFramebuffer("/dev/fb0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "splash: no framebuffer:", err)
		return 1
	}
	defer fb.Close()
	cw, ch := fb.Xres(), fb.Yres()
	if cw < 1 || ch < 1 {
		cw, ch = W, H
	}
	c := ui.NewCanvas(cw, ch)
	t := w.t
	col, hint := t.Text, "please wait..."
	switch tone {
	case "good":
		col = t.Good
	case "bad":
		col, hint = t.Bad, "returning to menu"
	}
	x, y, ww, _ := t.Frame(c, "Lodor", hint)
	c.DrawTextCentered(x, y+10, ww, title, t.Accent, t.TitleScale-1)
	t.DrawTextWrappedAt(c, x, y+10+wizGlyphH*(t.TitleScale-1)+30, ww, body, col, t.BodyScale)
	fb.Flush(c)
	return 0
}

// dataDir resolves the app working directory (config.json, catalog-index.json,
// pending queue): LODOR_PAK_DIR — the engine's own PakDir() convention — with
// LODOR_DATA_DIR kept as a back-compat alias for the original muOS app scripts;
// empty means the process CWD (the scripts cd into the data dir before exec).
func dataDir() string {
	if d := os.Getenv("LODOR_PAK_DIR"); d != "" {
		return d
	}
	return os.Getenv("LODOR_DATA_DIR")
}

// defaultDeviceName seeds the "Name this device" keyboard (lodor#36) instead of the old
// hardcoded "RG34XX": board devicetree model — the exact source the shell lib's
// lodor_ensure_device uses — else hostname, else the historical fallback. The keyboard
// override always wins; this is only the preset.
func defaultDeviceName() string {
	return defaultDeviceNameFrom("/sys/firmware/devicetree/base/model")
}

// defaultDeviceNameFrom resolves the preset from a devicetree model file (NULs and
// whitespace trimmed), then hostname, then "RG34XX". Never returns "".
func defaultDeviceNameFrom(modelPath string) string {
	if b, err := os.ReadFile(modelPath); err == nil {
		if s := strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", "")); s != "" {
			return s
		}
	}
	if h, err := os.Hostname(); err == nil {
		if s := strings.TrimSpace(h); s != "" {
			return s
		}
	}
	return "RG34XX"
}

// ---- startup phase instrumentation (BUG 2a) -------------------------------------------
// The 2026-07-04 field log showed the seed finishing but NO following wizard line: the
// wizard started and never reached the menu, and the hang doesn't reproduce off-hardware.
// logPhase writes ONE immediately-flushed line at each startup step to $dataDir/wizard.log
// AND stderr (mux_launch redirects stderr into romm.log), so the NEXT hang's log pinpoints
// the exact last phase reached. Open+write+Sync+Close per line so a hard kill still keeps it.

func (w *wizard) logPhase(format string, a ...interface{}) {
	msg := time.Now().Format("2006-01-02 15:04:05") + " wizard-phase: " + fmt.Sprintf(format, a...) + "\n"
	fmt.Fprint(os.Stderr, msg) // unbuffered; lands in romm.log via mux_launch's redirect
	if w.dataDir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(w.dataDir, "wizard.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(msg)
	_ = f.Sync() // survive a hard hang/kill — the whole point of the instrumentation
	_ = f.Close()
}

// Phase emitters: shared by the interactive startup path AND phaseSelftest so the harness
// asserts the SAME strings the real path emits (no drift).
func (w *wizard) phaseStart()             { w.logPhase("wizard: start") }
func (w *wizard) phaseFB(x, y, bpp int)   { w.logPhase("fb open %dx%d %dbpp", x, y, bpp) }
func (w *wizard) phaseInput(n int)        { w.logPhase("input open %d devices", n) }
func (w *wizard) phaseConfigured(b bool)  { w.logPhase("configured=%s", yn(b)) }
func (w *wizard) phaseMenuBuild()         { w.logPhase("menu: build state") }
func (w *wizard) phaseMenuFirstDraw()     { w.logPhase("menu: first draw") }
func (w *wizard) phaseMenuAwaitingInput() { w.logPhase("menu: awaiting input") }
func (w *wizard) phaseMenuStateOK(st menuState) {
	w.logPhase("menu: state ok (pendingN=%d queueN=%d tsAvail=%v)", st.pendingN, st.queueN, st.tsAvail)
}

// phaseSelftest replays the canonical startup phase sequence (representative values) through
// the real logPhase — the harness runs it on the built binary and greps the output.
func (w *wizard) phaseSelftest() {
	w.phaseStart()
	w.phaseFB(W, H, 32)
	w.phaseInput(1)
	w.phaseConfigured(true)
	w.phaseMenuBuild()
	w.phaseMenuStateOK(menuState{})
	w.phaseMenuFirstDraw()
	w.phaseMenuAwaitingInput()
}

// engineBin locates lodor-sync next to the wizard binary (or LODOR_BIN).
func engineBin() string {
	if b := os.Getenv("LODOR_BIN"); b != "" {
		return b
	}
	self, _ := os.Executable()
	return filepath.Join(filepath.Dir(self), "lodor-sync")
}

// runEngine runs lodor-sync with args, returning combined output. Inherits the env the
// caller set (LODOR_PAK_DIR/ROMS_DIR/SSL_CERT_FILE etc.) and runs in the data dir, so
// the engine's PakDir() CWD fallback lands there even without the env.
func (w *wizard) runEngine(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.bin, args...)
	cmd.Dir = w.dataDir
	return runCaptured(ctx, cmd, engineTimeout)
}

// runCaptured runs cmd under ctx (deadline d), capturing combined stdout+stderr to a
// REAL temp file rather than CombinedOutput — the same defeated-timeout fix runShimFor
// carries (muOS 9741ace only patched the shim). CombinedOutput wires the child's stdout/
// stderr through an os.Pipe plus a copier goroutine that cmd.Wait() blocks on; if the
// engine child ever leaves a grandchild holding that write end, Wait() never returns even
// after the context has killed the direct child — the timeout is silently defeated and the
// wizard wedges. A real *os.File dup'd straight to the child has no copier goroutine, so
// Wait() returns the instant the direct child is reaped/killed; the context still kills
// ONLY the direct child. On temp-file creation failure it falls back to CombinedOutput
// (correct output, original wedge risk) rather than losing the call. The caller sets
// cmd.Dir/Stdin before calling; this owns only Stdout/Stderr.
func runCaptured(ctx context.Context, cmd *exec.Cmd, d time.Duration) (string, error) {
	tf, ferr := os.CreateTemp("", "lodor-engine-*.out")
	if ferr != nil {
		out, err := cmd.CombinedOutput()
		return string(out), timeoutErr(ctx, err, d)
	}
	defer os.Remove(tf.Name())
	cmd.Stdout, cmd.Stderr = tf, tf
	runErr := cmd.Run()
	_ = tf.Sync()
	b, _ := os.ReadFile(tf.Name())
	_ = tf.Close()
	return string(b), timeoutErr(ctx, runErr, d)
}

func resultFlag(out, key string) bool {
	for _, f := range strings.Fields(out) {
		if f == key+"=1" {
			return true
		}
	}
	return false
}

// wifiUp checks the stock muOS link state (Principle 1: we don't manage Wi-Fi).
func wifiUp() bool {
	b, err := os.ReadFile("/sys/class/net/wlan0/operstate")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "up"
}

// ---- rendering (pure: step+state -> canvas) -------------------------------------------

func (w *wizard) render(s step, kb *ui.Keyboard) *ui.Canvas {
	c := ui.NewCanvas(W, H)
	t := w.t
	switch s {
	case stepWelcome:
		t.Message(c, "Welcome", "This sets up Lodor. Your whole library appears in the games menu, each game downloads on first launch, and saves sync automatically around every session.\n\nPress A to begin, or B to exit back to "+w.hc().osName+".", t.Text)
	case stepWifi:
		if w.wifiUp {
			t.Message(c, "Wi-Fi connected", "Your handheld is online. Press A to continue, or B to go back.", t.Good)
		} else {
			t.Message(c, "Connect Wi-Fi first", w.hc().wifiOpen+" Then press A to re-check.\n\nPress B to go back (you can sync later from the Lodor app).", t.Bad)
		}
	case stepServer:
		x, y, ww, hh := t.Frame(c, "Lodor Setup", "D-pad: move   A: type   B: delete   BACK: go back   Start: OK")
		kb.Draw(c, t, x, y, ww, hh)
	case stepCode:
		x, y, ww, hh := t.Frame(c, "Lodor Setup", "D-pad: move   A: type   B: delete   BACK: go back   Start: OK")
		kb.Draw(c, t, x, y, ww, hh)
	case stepDevice:
		x, y, ww, hh := t.Frame(c, "Lodor Setup", "D-pad: move   A: type   B: delete   BACK: go back   Start: OK")
		kb.Draw(c, t, x, y, ww, hh)
	case stepValidate:
		body := validateBody(w.reach, w.auth, w.registered)
		col := t.Good
		title := "Connected!"
		if !w.reach || !w.auth {
			col = t.Bad
			title = "Couldn't connect"
		}
		t.Message(c, title, body+"\n\nPress A to continue.", col)
	case stepTsSignin:
		// Render the tailscaled login URL as a scannable QR (quiet zone + integer scale,
		// dark-on-white so any phone camera reads it) with the URL as text underneath as a
		// fallback, plus an honest status line. #172: the pure-Go QRMatrix encoder replaces
		// the old text-only screen. B cancels; the poll-to-connected loop is unchanged.
		x, y, ww, hh := t.Frame(c, "Connect via Tailscale", "B: cancel")
		c.DrawTextCentered(x, y+6, ww, "Scan to sign in", t.Accent, t.TitleScale-1)
		top := y + 6 + wizGlyphH*(t.TitleScale-1) + 16
		if w.tsURL != "" {
			if m, err := ui.QRMatrix(w.tsURL); err == nil {
				// square QR box: as tall as fits above the URL+status text (~4 text lines).
				textH := (wizGlyphH*t.BodyScale + 6) * 4
				boxH := hh - (top - y) - textH - 12
				side := boxH
				if side > ww {
					side = ww
				}
				qx := x + (ww-side)/2
				c.DrawQR(qx, top, side, side, m, 0x000000, 0xffffff)
				top += side + 10
			}
			t.DrawTextWrappedAt(c, x, top, ww, w.tsURL, t.Dim, t.SmallScale)
			top += (wizGlyphH*t.SmallScale + 6) * 2
		}
		t.DrawTextWrappedAt(c, x, top, ww, w.tsPhase, t.Text, t.BodyScale)
	case stepDone:
		t.Message(c, "All set!", "Your library is now in the games menu as stubs. Pick any game and it downloads on first launch; saves sync around every session.\n\nPress A to exit.", t.Good)
	case stepError:
		t.Message(c, "Setup error", w.errMsg+"\n\nPress A to exit, or B to go back and try again.", t.Bad)
	}
	return c
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// ---- host-OS copy + version visibility (lodor#32 / #45) --------------------------------
// The SAME wizard binary ships on muOS and Knulli, so every string that names the host
// OS or its menus comes from this table, keyed on LODOR_HOST_OS (each launcher exports
// its value; unset/unknown degrades to muos, the lane this app shipped on first).

type hostCopy struct {
	osName      string // what the user exits back to ("muOS" / "Knulli")
	wifiOpen    string // wifi-down onboarding screen: where Wi-Fi entry lives
	noWifi      string // requireOnline gate: the same fact as a one-liner
	updateVenue string // update-badge suffix: where an update installs from
}

var hostCopyTable = map[string]hostCopy{
	"muos": {
		osName:      "muOS",
		wifiOpen:    "Open muOS Settings, Network, and join your Wi-Fi.",
		noWifi:      "Connect Wi-Fi in muOS Settings, then try again.",
		updateVenue: "App Downloader",
	},
	"knulli": {
		osName:      "Knulli",
		wifiOpen:    "Open the main menu, Network Settings, and join your Wi-Fi.",
		noWifi:      "Connect Wi-Fi in Network Settings, then try again.",
		updateVenue: "GitHub zip",
	},
}

// hostOS reads LODOR_HOST_OS (launcher-exported; see mux_launch.sh / ports/Lodor.sh).
func hostOS() string { return os.Getenv("LODOR_HOST_OS") }

// hostCopyFor resolves the copy for a host-OS name, defaulting to muos.
func hostCopyFor(name string) hostCopy {
	if c, ok := hostCopyTable[name]; ok {
		return c
	}
	return hostCopyTable["muos"]
}

// hc is the wizard's resolved host copy (env read per call — cheap, and tests just Setenv).
func (w *wizard) hc() hostCopy { return hostCopyFor(hostOS()) }

// updateInstructions is the "how do I install it" update body per host OS (lodor#32):
// muOS installs via the App Downloader / Archive Manager; Knulli extracts the release
// zip over /userdata (config/pairing live outside the payload, so they are kept).
func updateInstructions(host, latest, current string) string {
	if host == "knulli" {
		return fmt.Sprintf("Lodor %s is out (you have %s).\nDownload Lodor-Knulli-%s.zip from GitHub and extract it onto /userdata over the network share - your pairing is kept.", latest, current, latest)
	}
	return fmt.Sprintf("Lodor %s is out (you have %s).\nInstall it with the muOS App Downloader,\nor grab the .muxapp from GitHub and use\nArchive Manager.", latest, current)
}

// menuTitle carries the running version in the menu header (lodor#45): offline, straight
// from buildinfo — "Lodor 0.9.7"; a dev build honestly reads "Lodor dev".
func menuTitle() string { return "Lodor " + buildinfo.Version }

// versionFlash is the one-shot "Updated to <v>" notice (lodor#45): last_seen_version is
// stamped in settings.conf and only a CHANGE from a previously-seen version announces —
// a fresh install stays quiet (nothing was updated), dev builds never stamp or flash,
// and a failed stamp skips the flash rather than repeating it every open. Returns the
// flash body, "" when there is nothing to show.
func (w *wizard) versionFlash(v string) string {
	if v == "" || v == "dev" {
		return ""
	}
	last := w.getSetting("last_seen_version")
	if last == v {
		return ""
	}
	if err := w.setSetting("last_seen_version", v); err != nil {
		return ""
	}
	if last == "" {
		return ""
	}
	return "Updated to " + v + "."
}

// validateBody is the validate screen's status block (pure for tests). lodor#39: the
// register outcome is a first-class row — an unregistered device syncs no saves, and
// the extra line says so honestly (lodor_ensure_device retries on later launches).
func validateBody(reach, auth, registered bool) string {
	body := "Server reachable: " + yn(reach) + "\nLogin accepted: " + yn(auth) + "\nDevice registered: " + yn(registered)
	if !registered {
		body += "\nSaves start syncing once this device can register - retried automatically."
	}
	return body
}

// pairCodeHint answers "where do pairing codes come from" (lodor#40), drawn above the
// prompt on every code-entry keyboard.
const pairCodeHint = "In RomM on your computer: Settings > Devices > Pair"

// pairFailure maps a failed --pair run to honest copy plus where B should return
// (lodor#31). The engine exit code is authoritative (runPair: 3 exchange-unreachable,
// 4 ran-but-errored, 6 pairing-expired) and its PAIRFAIL stderr line carries the real
// reason. rc=3 NEVER says "generate a fresh code" — the server never saw the code, so
// the code is not the problem; B goes back to the server step instead.
func pairFailure(rc int, out string) (msg string, retry step) {
	switch rc {
	case 3:
		return "Couldn't reach your server - check the address and Wi-Fi.", stepServer
	case 6:
		return "Pairing expired - generate a fresh code in RomM and try again.", stepCode
	}
	if why := pairFailLine(out); why != "" {
		return "Pairing failed: " + why + "\nGenerate a fresh code in RomM and try again.", stepCode
	}
	return "Pairing failed. Generate a fresh code in RomM and try again.", stepCode
}

// ---- lodor#35: certificate trust (self-signed home servers) -----------------------------
// A self-signed HTTPS RomM used to die at pairing with "couldn't reach your server" and
// no UI escape (the only fix was hand-editing config.json). The engine now tags a failed
// verification with reason=tls on the RESULT line; the wizard answers with a choice
// screen, and trusting re-runs --set-server with --insecure — persisting the SAME
// insecure_skip_verify config.json setting the NextUI shell wizard writes.

// certFailure reports whether a failed --pair died on certificate verification: the
// engine's machine token (reason=tls), never the human text. rc!=0 keeps a successful
// run out of the class regardless of stray tokens.
func certFailure(rc int, out string) bool {
	return rc != 0 && ui.ResultToken(out, "reason") == "tls"
}

// Certificate-trust copy (rules: plain "certificate" language, the tradeoff stated
// honestly in one line, no host-OS strings needed).
const (
	certTrustTitle  = "Couldn't verify the server"
	certTrustBody   = "Couldn't verify the server's certificate. This is normal for self-signed home servers.\n\nSkipping the check means the connection is not verified - only do this for your own server."
	certTrustOption = "Trust this server (skip certificate verification)"
)

// renderCertTrust draws the certificate-failure choice screen (pure: state -> canvas,
// shared by the interactive loop and --capture).
func (w *wizard) renderCertTrust(m *ui.Menu) *ui.Canvas {
	c := ui.NewCanvas(W, H)
	t := w.t
	x, y, ww, hh := t.Frame(c, "Lodor Setup", "Up/Down: move   A: select   B: back")
	c.DrawTextCentered(x, y+6, ww, certTrustTitle, t.Accent, t.TitleScale-1)
	top := y + 6 + wizGlyphH*(t.TitleScale-1) + 16
	bot := t.DrawTextWrappedAt(c, x, top, ww, certTrustBody, t.Text, t.BodyScale)
	m.Draw(c, t, x, bot+16, ww, hh-(bot+16-y))
	return c
}

// offerCertTrust runs the choice screen: true = the user chose to trust the server
// (skip verification); false = go back (B or the "Go back" row).
func (w *wizard) offerCertTrust(draw func(*ui.Canvas), btn func() ui.Button) bool {
	m := &ui.Menu{Items: []string{certTrustOption, "Go back"}}
	for {
		draw(w.renderCertTrust(m))
		b := btn()
		if b == ui.BtnBack {
			return false
		}
		if m.Handle(b) {
			return m.Selected() == 0
		}
	}
}

// pairServer runs the --pair exchange for the code step and maps the outcome to the
// next onboarding step (stepDevice on success; stepError with w.errMsg/w.retry set on
// failure). lodor#35: a certificate-verification failure on an https server offers
// the trust screen INSTEAD of the misleading unreachable copy; trusting re-runs
// --set-server with --insecure (persisting insecure_skip_verify) and pairs once more
// with the SAME code — a cert failure never reached RomM, so the code was not consumed.
func (w *wizard) pairServer(server, code string, draw func(*ui.Canvas), btn func() ui.Button) step {
	w.working(draw, "Pairing with your server...")
	out, err := w.runEngine("--pair", code)
	if certFailure(exitCode(err), out) && strings.HasPrefix(server, "https://") {
		if !w.offerCertTrust(draw, btn) {
			return stepServer // go back and fix the address instead
		}
		if sout, serr := w.runEngine("--set-server", server, "--insecure"); serr != nil || !resultFlag(sout, "server_set") {
			w.errMsg = "Could not save the server address. Check it and try again."
			w.retry = stepServer
			return stepError
		}
		w.working(draw, "Pairing with your server...")
		out, err = w.runEngine("--pair", code)
	}
	if err != nil || !resultFlag(out, "paired") {
		// lodor#31: the engine exit code is authoritative — an unreachable server
		// (rc=3) never blames the code, and B walks back to the server step.
		w.errMsg, w.retry = pairFailure(exitCode(err), out)
		return stepError
	}
	// lodor#38: paired, but the token lacks sync scopes — warn NOW (NextUI
	// launch.sh parity) instead of letting saves fail confusingly later.
	if !resultFlag(out, "scopes_ok") {
		w.showMsg("Paired", "Paired - but the token is missing some sync permissions. Re-generate it in RomM with all scopes.", w.t.Bad, draw, btn)
	}
	return stepDevice
}

// pairFailLine extracts the engine's last PAIRFAIL reason line ("" when absent).
func pairFailLine(out string) string {
	why := ""
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "PAIRFAIL ") {
			why = strings.TrimSpace(strings.TrimPrefix(ln, "PAIRFAIL "))
		}
	}
	return why
}

// ---- interactive loop -----------------------------------------------------------------

func (w *wizard) runInteractive() {
	w.phaseStart()
	fb, err := ui.OpenFramebuffer("/dev/fb0")
	if err != nil {
		w.logPhase("fb open FAILED: %v", err)
		fmt.Fprintln(os.Stderr, "wizard: no framebuffer:", err)
		os.Exit(1)
	}
	defer fb.Close()
	w.phaseFB(fb.Xres(), fb.Yres(), fb.Bpp())
	in, err := w.openInput()
	if err != nil {
		w.logPhase("input open FAILED: %v", err)
		fmt.Fprintln(os.Stderr, "wizard: no input device:", err)
		os.Exit(1)
	}
	defer in.Close()
	w.in = in

	// LODOR_FB_DUMP (test seam): after every blit, dump the real framebuffer contents to this
	// PNG (last frame wins) so the off-hardware harness can diff a rendered frame. Off by
	// default — production never dumps. Best-effort; a failed dump never disturbs the UI.
	dumpPath := os.Getenv("LODOR_FB_DUMP")
	draw := func(c *ui.Canvas) {
		fb.Flush(c)
		if dumpPath != "" {
			_ = fb.SavePNG(dumpPath)
		}
	}
	btn := func() ui.Button { return <-in.Buttons() }

	configured := w.configured()
	w.phaseConfigured(configured)
	if configured {
		w.runMainMenu(draw, btn)
		return
	}
	w.runOnboarding(draw, btn)
}

// openInput returns the input source feeding the REAL menu loop. Production opens the live
// evdev devices (unchanged). TEST SEAM: if LODOR_INPUT_SCRIPT is set, a ScriptedSource
// replays a fixed button sequence into the identical loop, so the off-hardware harness
// drives the real runMainMenu/pickScroll without any hardware. The ScriptedSource blocks
// (does not close) once the sequence is exhausted, so a script that fails to reach an exit
// deadlocks the read — which the harness's outer `timeout` catches as an honest TIMEOUT
// rather than a fake pass. The evdev A/B keymap is NOT exercised by this path (scripted
// buttons are already logical) — that stays a hardware-only check.
func (w *wizard) openInput() (ui.InputSource, error) {
	if script := os.Getenv("LODOR_INPUT_SCRIPT"); script != "" {
		seq, err := parseButtonScript(script)
		if err != nil {
			return nil, err
		}
		w.logPhase("input: scripted source (%d events)", len(seq))
		w.phaseInput(len(seq))
		return ui.NewScriptedSource(seq), nil
	}
	ev, err := ui.NewEvdevSource()
	if err != nil {
		return nil, err
	}
	w.phaseInput(ev.Count())
	return ev, nil
}

// parseButtonScript turns a comma/space/newline-separated token list into logical Buttons
// for the ScriptedSource. Tokens (case-insensitive): u/up, d/down, l/left, r/right,
// a/confirm, b/back, start, select. Unknown tokens are a hard error (no silent drops).
func parseButtonScript(s string) ([]ui.Button, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	var out []ui.Button
	for _, f := range fields {
		switch strings.ToLower(f) {
		case "u", "up":
			out = append(out, ui.BtnUp)
		case "d", "down":
			out = append(out, ui.BtnDown)
		case "l", "left":
			out = append(out, ui.BtnLeft)
		case "r", "right":
			out = append(out, ui.BtnRight)
		case "a", "confirm", "ok":
			out = append(out, ui.BtnConfirm)
		case "b", "back":
			out = append(out, ui.BtnBack)
		case "start":
			out = append(out, ui.BtnStart)
		case "select":
			out = append(out, ui.BtnSelect)
		default:
			return nil, fmt.Errorf("bad button token %q in LODOR_INPUT_SCRIPT", f)
		}
	}
	return out, nil
}

// configured reports whether config.json already holds a server + credential.
func (w *wizard) configured() bool {
	b, err := os.ReadFile(filepath.Join(w.dataDir, "config.json"))
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "\"token\"") || strings.Contains(s, "\"password\"")
}

// ---- main management menu (parity with Lodor-NextUI's Lodor.pak) ----------------------
// The re-runnable surface shown once setup is done. muOS can't hook the stock launcher
// (never fork muOS), so ALL day-to-day management lives HERE inside the Lodor app as
// a framebuffer menu. Every row is a thin shell over ONE confirmed engine mode (host
// rendering only; the engine owns all RomM logic). Conditional rows + dynamic labels are
// LOCAL file reads so drawing the menu never touches the network.

type menuAct int

const (
	actRepair menuAct = iota
	actSyncNow
	actPushPending
	actRefreshUpdate
	actRefreshFull
	actGameManager
	actSearch
	actDownloadQueue
	actDownloadBios
	actRecent
	actSwitchUser
	actAddProfile
	actBoxArt
	actRetroAch
	actTsStatus
	actTsReconnect
	actTsReset
	actReSetup
	actRemoveLodor
	actCheckUpdates
	actExit
)

type menuRow struct {
	label string
	act   menuAct
}

// menuState is the LOCAL (no-network) state that shapes the menu: which conditional rows
// appear and the dynamic labels. All fields are file reads or a cheap shim probe.
type menuState struct {
	pairingExpired bool
	pendingN       int
	queueN         int
	userLabel      string
	coversOn       bool
	tsAvail        bool
	updateAvail    string // settings.conf update_available, "" when none/already installed
	updateVenue    string // host install venue for the update badge (lodor#32/#45)
}

// buildMenuRows is the PURE menu spine: local state -> ordered rows + their engine action.
// Table-driven + pure so a test asserts every row maps to the right mode and the
// conditional rows appear only when their count/flag warrants (the "menu-row assertions").
func buildMenuRows(st menuState) []menuRow {
	var r []menuRow
	add := func(label string, a menuAct) { r = append(r, menuRow{label, a}) }
	if st.pairingExpired {
		add("! Pairing expired - re-pair", actRepair)
	}
	add("Sync now", actSyncNow)
	if st.pendingN > 0 {
		add(fmt.Sprintf("Pending saves (%d) - upload", st.pendingN), actPushPending)
	}
	add("Refresh library (update)", actRefreshUpdate)
	add("Refresh library (full)", actRefreshFull)
	add("Game Manager", actGameManager)
	add("Search library", actSearch)
	if st.queueN > 0 {
		add(fmt.Sprintf("Download queue (%d)", st.queueN), actDownloadQueue)
	}
	add("Download BIOS", actDownloadBios)
	add("Recent activity", actRecent)
	if st.updateAvail != "" {
		// store lane: install happens via the host's own venue (muOS App Downloader /
		// Knulli GitHub zip), never a self-swap — selecting the row re-checks and shows
		// what's new + where to get it; the suffix names the venue (lodor#45).
		label := fmt.Sprintf("Update available (%s)", st.updateAvail)
		if st.updateVenue != "" {
			label += " - " + st.updateVenue
		}
		add(label, actCheckUpdates)
	}
	add(fmt.Sprintf("Switch user (%s)", st.userLabel), actSwitchUser)
	add("Add profile", actAddProfile)
	if st.coversOn {
		add("Box art: All covers (on refresh)", actBoxArt)
	} else {
		add("Box art: Downloaded games only", actBoxArt)
	}
	add("RetroAchievements", actRetroAch)
	if st.tsAvail {
		add("Tailscale status", actTsStatus)
		add("Tailscale: Reconnect", actTsReconnect)
		add("Tailscale: Reset & forget", actTsReset)
	}
	add("Check for updates", actCheckUpdates)
	add("Setup / Re-pair", actReSetup)
	add("Remove Lodor from this card", actRemoveLodor)
	add("Exit", actExit)
	return r
}

// runMainMenu draws the management menu and dispatches. B on the top-level menu exits to
// muOS. State is recomputed each time we return to the menu (a Sync/Delete/Switch changed
// a count/label), so the conditional rows and labels stay honest.
func (w *wizard) runMainMenu(draw func(*ui.Canvas), btn func() ui.Button) {
	// lodor#45: one-shot "Updated to <v>" flash when the installed version changed.
	if msg := w.versionFlash(buildinfo.Version); msg != "" {
		w.showMsg("Updated", msg, w.t.Good, draw, btn)
	}
	firstPaint := true
	for {
		w.phaseMenuBuild()  // last line before menuState() — if the log stops HERE, a menu-build
		st := w.menuState() // shell-out (tsAvailable) wedged (now timeout-guarded, so it can't).
		w.phaseMenuStateOK(st)
		rows := buildMenuRows(st)
		labels := make([]string, len(rows))
		for i, rw := range rows {
			labels[i] = rw.label
		}
		m := &ui.ScrollMenu{Items: labels}
		// Instrument ONLY the first paint: wrap draw/btn so "first draw" fires before the first
		// render and "awaiting input" right before we block on input — pinpointing whether a hang
		// is in the render or the input wait. Subsequent iterations use the bare callbacks.
		mdraw, mbtn := draw, btn
		if firstPaint {
			firstPaint = false
			drawn, awaited := false, false
			mdraw = func(c *ui.Canvas) {
				if !drawn {
					w.phaseMenuFirstDraw()
					drawn = true
					w.settlePaint(c, draw) // re-blit the first frame briefly so it wins the display
					return
				}
				draw(c)
			}
			mbtn = func() ui.Button {
				if !awaited {
					w.phaseMenuAwaitingInput()
					awaited = true
				}
				return btn()
			}
		}
		sel, ok := w.pickScroll(menuTitle(), "Up/Down: move   A: select   B: exit", m, mdraw, mbtn)
		if !ok {
			return // B on the main menu = clean exit to muOS
		}
		if w.dispatch(rows[sel].act, draw, btn) {
			return
		}
	}
}

// settlePaint blits the SAME rendered menu frame settleFrames times over a short window, so the
// first menu paint wins the panel against a late muOS overlay composite / re-pan (see the const
// doc above). draw is the real fb-flushing closure; each call re-blits AND re-pans page 0 to the
// front. Bounded and honest (feedback_no_fake_ui_state): it repaints the ACTUAL frame, shows no
// fake progress, and never spins — a fixed count of flushes with a fixed sleep, then it returns
// and the caller blocks on real input.
func (w *wizard) settlePaint(c *ui.Canvas, draw func(*ui.Canvas)) {
	for i := 0; i < settleFrames; i++ {
		draw(c)
		time.Sleep(settleInterval)
	}
}

// dispatch runs one menu action. Returns true only when the app should exit (Exit row).
func (w *wizard) dispatch(a menuAct, draw func(*ui.Canvas), btn func() ui.Button) (exit bool) {
	switch a {
	case actRepair, actReSetup:
		w.runOnboarding(draw, btn)
	case actSyncNow:
		w.doSyncNow(draw, btn)
	case actPushPending:
		w.doPushPending(draw, btn)
	case actRefreshUpdate:
		w.doRefresh(false, draw, btn)
	case actRefreshFull:
		w.doRefresh(true, draw, btn)
	case actGameManager:
		w.gameManager(draw, btn)
	case actSearch:
		w.searchLibrary(draw, btn)
	case actDownloadQueue:
		w.doDownloadQueue(draw, btn)
	case actDownloadBios:
		w.doDownloadBios(draw, btn)
	case actRecent:
		w.doRecent(draw, btn)
	case actSwitchUser:
		w.doSwitchUser(draw, btn)
	case actAddProfile:
		w.doAddProfile(draw, btn)
	case actBoxArt:
		w.doToggleCovers(draw, btn)
	case actRetroAch:
		w.doRetroAch(draw, btn)
	case actTsStatus:
		w.doTsStatus(draw, btn)
	case actTsReconnect:
		w.doTsReconnect(draw, btn)
	case actTsReset:
		w.doTsReset(draw, btn)
	case actRemoveLodor:
		w.doRemoveLodor(draw, btn)
	case actCheckUpdates:
		w.doCheckUpdates(draw, btn)
	case actExit:
		return true
	}
	return false
}

// menuState reads all the local state that shapes the menu (no network).
func (w *wizard) menuState() menuState {
	return menuState{
		pairingExpired: w.pairingExpired(),
		pendingN:       countLines(filepath.Join(w.dataDir, "pending-saves.txt")),
		queueN:         countLines(filepath.Join(w.dataDir, "download-queue.txt")),
		userLabel:      w.activeProfileLabel(),
		coversOn:       w.fetchCoversOn(),
		tsAvail:        w.tsAvailable(),
		updateAvail:    w.updateAvailable(),
		updateVenue:    w.hc().updateVenue,
	}
}

// updateAvailable reads the update_available stamp (written by doCheckUpdates, cleared when a
// check says up-to-date). The badge self-retires once the named version IS the running build —
// the App Downloader install changes buildinfo.Version, not settings.conf.
func (w *wizard) updateAvailable() string {
	v := w.getSetting("update_available")
	if v == "" || v == buildinfo.Version {
		return ""
	}
	return v
}

// pairingExpired reports the sticky flag written when any engine call returns rc=6. Cleared
// by the first successful (rc=0) network action (clearPairFlag).
func (w *wizard) pairingExpired() bool {
	_, err := os.Stat(filepath.Join(w.dataDir, ".pairing-expired"))
	return err == nil
}

func (w *wizard) markPairFlag(rc int) {
	switch rc {
	case 6:
		_ = os.WriteFile(filepath.Join(w.dataDir, ".pairing-expired"), []byte("1\n"), 0o644)
	case 0:
		_ = os.Remove(filepath.Join(w.dataDir, ".pairing-expired"))
	}
}

// countLines returns the non-blank line count of a file; 0 for missing/unreadable.
func countLines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

// activeProfileLabel resolves the active profile label locally the same way the engine
// does: active-profile.txt names it; else config.json's profile_label, else username, else
// "Default". Pure file reads (drawing the menu never shells the engine).
func (w *wizard) activeProfileLabel() string {
	if b, err := os.ReadFile(filepath.Join(w.dataDir, "active-profile.txt")); err == nil {
		if s := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]); s != "" {
			return s
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(w.dataDir, "config.json"))
	if v := jsonString(string(cfg), "profile_label"); v != "" {
		return v
	}
	if v := jsonString(string(cfg), "username"); v != "" {
		return v
	}
	return "Default"
}

// jsonString is a tiny, dependency-free "grep" for a top-level "key":"value" string in
// config.json — the wizard only ever needs a couple of scalar labels and must stay stdlib.
func jsonString(s, key string) string {
	needle := "\"" + key + "\""
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	rest := s[i+len(needle):]
	c := strings.IndexByte(rest, ':')
	if c < 0 {
		return ""
	}
	rest = rest[c+1:]
	q1 := strings.IndexByte(rest, '"')
	if q1 < 0 {
		return ""
	}
	rest = rest[q1+1:]
	q2 := strings.IndexByte(rest, '"')
	if q2 < 0 {
		return ""
	}
	return rest[:q2]
}

// ---- shared action helpers (G0) -------------------------------------------------------

// exitCode extracts a process exit code from an exec error (0 on nil).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

// runEngineStdin runs lodor-sync with args, piping pw on STDIN (for --login-profile and
// --ra-login, which read the password from stdin, NEVER argv). Same CWD as runEngine so
// config.json resolves. The password is never logged or placed in argv.
func (w *wizard) runEngineStdin(pw string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.bin, args...)
	cmd.Dir = w.dataDir
	cmd.Stdin = strings.NewReader(pw + "\n")
	return runCaptured(ctx, cmd, engineTimeout)
}

// working paints one honest "working" frame (NOT fake progress — the op really is running
// synchronously beneath it; feedback_no_fake_ui_state). Used to bracket blocking engine
// calls that have no progress side-channel.
func (w *wizard) working(draw func(*ui.Canvas), body string) {
	c := ui.NewCanvas(W, H)
	x, y, ww, _ := w.t.Frame(c, "Lodor", "please wait...")
	c.DrawTextCentered(x, y+10, ww, "Working", w.t.Accent, w.t.TitleScale-1)
	w.t.DrawTextWrappedAt(c, x, y+10+wizGlyphH*(w.t.TitleScale-1)+30, ww, body, w.t.Text, w.t.BodyScale)
	draw(c)
}

// showMsg draws a titled message and waits for a dismiss (A/Start/B all dismiss).
func (w *wizard) showMsg(title, body string, col ui.Color, draw func(*ui.Canvas), btn func() ui.Button) {
	for {
		c := ui.NewCanvas(W, H)
		x, y, ww, _ := w.t.Frame(c, "Lodor", "A: OK")
		c.DrawTextCentered(x, y+10, ww, title, w.t.Accent, w.t.TitleScale-1)
		w.t.DrawTextWrappedAt(c, x, y+10+wizGlyphH*(w.t.TitleScale-1)+30, ww, body, col, w.t.BodyScale)
		draw(c)
		switch btn() {
		case ui.BtnConfirm, ui.BtnStart, ui.BtnBack:
			return
		}
	}
}

// showResult renders an engine call's outcome HONESTLY: on a non-zero exit it shows the
// diagnosed cause; on success it echoes the engine's real RESULT/reason line (never an
// invented "OK"). It also updates the sticky pairing-expired flag from rc.
func (w *wizard) showResult(output string, rc int, okBody string, draw func(*ui.Canvas), btn func() ui.Button) {
	w.markPairFlag(rc)
	if rc != 0 {
		w.showMsg("Problem", w.diagnose(rc, output), w.t.Bad, draw, btn)
		return
	}
	body := ui.ParseEngineResult(output)
	if body == "" {
		body = okBody
	}
	w.showMsg("Done", body, w.t.Good, draw, btn)
}

// diagnose turns a failed engine exit into an honest, actionable cause (mirrors the NextUI
// pak's diagnose()). It trusts the engine's own exit code (its happy-eyeballs connect is
// authoritative) and re-checks only cheap local preconditions.
func (w *wizard) diagnose(rc int, output string) string {
	switch rc {
	case 2:
		return "Not connected - run Setup / Re-pair, then try again."
	case 3:
		return "Couldn't reach your server - check Wi-Fi and the server, then try again."
	case 4:
		return "Finished with errors - some items didn't sync. Try again."
	case 6:
		return "Pairing expired - run Setup / Re-pair."
	default:
		if s := ui.ParseEngineResult(output); s != "" {
			return s
		}
		return fmt.Sprintf("Something went wrong (code %d) - try again.", rc)
	}
}

// requireOnline gates a network action: Wi-Fi is muOS's job, so if the link is down we say
// so honestly and abort (no fake attempt). Tailscale bring-up is async at launch (G3); a
// tunnel that isn't up yet surfaces as the engine's own rc=3, diagnosed honestly.
func (w *wizard) requireOnline(draw func(*ui.Canvas), btn func() ui.Button) bool {
	if wifiUp() {
		return true
	}
	w.showMsg("No Wi-Fi", w.hc().noWifi, w.t.Bad, draw, btn)
	return false
}

// pickScroll draws a framed scrolling menu until A/Start (returns index, true) or B
// (returns -1, false). The one picker every list-based screen uses.
func (w *wizard) pickScroll(title, hint string, m *ui.ScrollMenu, draw func(*ui.Canvas), btn func() ui.Button) (int, bool) {
	for {
		c := ui.NewCanvas(W, H)
		x, y, ww, hh := w.t.Frame(c, title, hint)
		m.Draw(c, w.t, x, y, ww, hh)
		draw(c)
		b := btn()
		if b == ui.BtnBack {
			return -1, false
		}
		if m.Handle(b) {
			return m.Selected(), true
		}
	}
}

// confirmScreen is a 2-option yes/no over screenChoice: returns true only on an explicit
// "yes" pick (index 0). B / "no" => false.
func (w *wizard) confirmScreen(subtitle, yes, no string, draw func(*ui.Canvas), btn func() ui.Button) bool {
	i, act := w.screenChoice(subtitle, []string{yes, no}, draw, btn)
	return act == actAdvance && i == 0
}

// ---- G1: core action handlers ---------------------------------------------------------

// doSyncNow is the FAST sync (fix for the 0.9.x "Sync now == full mirror" mistake): push
// pending saves, pull newer saves, refresh Continue. NO catalog mirror (that is Refresh).
func (w *wizard) doSyncNow(draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Flushing pending saves...")
	_, e1 := w.runEngine("--push-pending")
	// Handoff v1: Sync Now must push save STATES too, not just battery saves.
	w.working(draw, "Pushing save states...")
	_, es := w.runEngine("--push-all-states")
	_, _ = w.runEngine("--push-pending-states")
	w.working(draw, "Pulling latest saves...")
	_, e2 := w.runEngine("--pull-saves")
	w.working(draw, "Updating Continue...")
	_, e3 := w.runEngine("--sync-continue")
	rc := max(exitCode(e1), max(exitCode(es), max(exitCode(e2), exitCode(e3))))
	w.markPairFlag(rc)
	if rc == 0 {
		w.maybeCheckUpdates(draw)
		w.showMsg("Done", "Sync complete.", w.t.Good, draw, btn)
	} else {
		w.showMsg("Problem", w.diagnose(rc, ""), w.t.Bad, draw, btn)
	}
}

// ---- self-update notices (store lane) ----------------------------------------------------
// On muOS, updates INSTALL via the App Downloader (or a .muxapp through Archive Manager) —
// Lodor only CHECKS and points there. The engine's --check-update reads versions.json
// (gh-pages, needs real internet — NOT just RomM reachability) and compares against the
// stamped build; the wizard owns every settings.conf stamp.

// doCheckUpdates is the manual check (menu row) — honest outcome in all three cases.
func (w *wizard) doCheckUpdates(draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Checking for updates...")
	out, err := w.runEngine("--check-update")
	if exitCode(err) != 0 {
		w.showMsg("Updates", "Couldn't reach the update server - this check needs internet access, not just your RomM server.", w.t.Bad, draw, btn)
		return
	}
	w.stampUpdateState(out)
	latest := ui.ResultToken(out, "latest")
	current := ui.ResultToken(out, "current")
	if ui.ResultToken(out, "update") != "1" {
		w.showMsg("Updates", fmt.Sprintf("You're up to date (%s).", current), w.t.Good, draw, btn)
		return
	}
	body := updateInstructions(hostOS(), latest, current)
	if notes := notesLine(out); notes != "" {
		body += "\n\nNew: " + notes
	}
	w.showMsg("Update available", body, w.t.Good, draw, btn)
}

// maybeCheckUpdates is the opportunistic tail of a good Sync now: radio already up, manifest
// GET nearly free. At most once a day, and EVERY failure is silent — a background path never
// nags. The honest working frame covers the wait (no invisible stall).
func (w *wizard) maybeCheckUpdates(draw func(*ui.Canvas)) {
	if last, err := strconv.ParseInt(w.getSetting("update_last_check"), 10, 64); err == nil {
		if time.Now().Unix()-last < 86400 {
			return
		}
	}
	w.working(draw, "Checking for updates...")
	out, err := w.runEngine("--check-update")
	if exitCode(err) == 0 {
		w.stampUpdateState(out)
	}
}

// stampUpdateState records a successful check: update_available set from update=1 (cleared on
// up-to-date, so an installed update self-clears its badge) + the last-check gate stamp.
func (w *wizard) stampUpdateState(out string) {
	if ui.ResultToken(out, "update") == "1" {
		_ = w.setSetting("update_available", ui.ResultToken(out, "latest"))
	} else {
		_ = w.setSetting("update_available", "")
	}
	_ = w.setSetting("update_last_check", strconv.FormatInt(time.Now().Unix(), 10))
}

// notesLine extracts the engine's single-line NOTES\t<text> trailer ("" when absent).
func notesLine(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "NOTES\t") {
			return strings.TrimPrefix(ln, "NOTES\t")
		}
	}
	return ""
}

func (w *wizard) doPushPending(draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Uploading pending saves...")
	out, err := w.runEngine("--push-pending")
	rc := exitCode(err)
	w.markPairFlag(rc)
	if rc != 0 {
		w.showMsg("Problem", w.diagnose(rc, out), w.t.Bad, draw, btn)
		return
	}
	pushed := ui.ResultToken(out, "pushed")
	stuck := ui.ResultToken(out, "stuck")
	if stuck != "" && stuck != "0" {
		w.showMsg("Uploaded", fmt.Sprintf("Uploaded %s save(s) - %s still stuck (kept queued, will retry).", nz(pushed), stuck), w.t.Bad, draw, btn)
	} else {
		w.showMsg("Uploaded", fmt.Sprintf("Uploaded %s save(s).", nz(pushed)), w.t.Good, draw, btn)
	}
}

// doRefresh mirrors the catalog (full re-fetches every cover) then refreshes collections.
// The catalog leg has a progress side-channel, so it uses screenMirror; collections is a
// quick working-frame.
func (w *wizard) doRefresh(full bool, draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	args := []string{"--mirror-catalog"}
	if full {
		args = append(args, "--full")
	}
	rc := w.screenMirrorArgs(draw, args...)
	w.working(draw, "Refreshing collections...")
	_, ec := w.runEngine("--mirror-collections")
	if rc == 0 {
		rc = exitCode(ec)
	}
	w.markPairFlag(rc)
	if rc == 0 {
		w.showMsg("Done", "Library refreshed.", w.t.Good, draw, btn)
	} else {
		w.showMsg("Problem", w.diagnose(rc, ""), w.t.Bad, draw, btn)
	}
}

func (w *wizard) doDownloadQueue(draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Downloading queued games...")
	out, err := w.runEngine("--download-queue")
	rc := exitCode(err)
	w.markPairFlag(rc)
	if rc != 0 {
		w.showMsg("Problem", w.diagnose(rc, out), w.t.Bad, draw, btn)
		return
	}
	dn := nz(ui.ResultToken(out, "downloaded"))
	fl := nz(ui.ResultToken(out, "failed"))
	rem := nz(ui.ResultToken(out, "remaining"))
	w.showMsg("Download queue", fmt.Sprintf("Downloaded %s, failed %s, %s still queued.", dn, fl, rem), w.t.Good, draw, btn)
}

func (w *wizard) doDownloadBios(draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Downloading BIOS files...")
	out, err := w.runEngine("--download-bios")
	rc := exitCode(err)
	w.markPairFlag(rc)
	if rc != 0 {
		w.showMsg("Problem", w.diagnose(rc, out), w.t.Bad, draw, btn)
		return
	}
	w.showMsg("Download BIOS", fmt.Sprintf("Downloaded %s BIOS file(s).", nz(ui.ResultToken(out, "bios"))), w.t.Good, draw, btn)
}

// doRecent shows the read-only cross-device save feed (--sync-feed) in a scroll list.
func (w *wizard) doRecent(draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Fetching recent activity...")
	out, err := w.runEngine("--sync-feed")
	if rc := exitCode(err); rc != 0 {
		w.markPairFlag(rc)
		if rc == 3 {
			// lodor#44: unreachable is the engine's rc=3 now — say so, never the empty state.
			w.showMsg("Recent activity", "Couldn't reach RomM - check Wi-Fi and the server, then try again.", w.t.Bad, draw, btn)
			return
		}
		w.showMsg("Problem", w.diagnose(rc, out), w.t.Bad, draw, btn)
		return
	}
	var items []string
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Split(ln, "\t")
		if len(f) < 2 {
			continue
		}
		row := f[0] + "  -  " + f[1]
		if len(f) >= 3 && strings.TrimSpace(f[2]) != "" {
			row += "  -  " + f[2]
		}
		items = append(items, row)
	}
	if len(items) == 0 {
		w.showMsg("Recent activity", "No recent activity yet.", w.t.Text, draw, btn)
		return
	}
	m := &ui.ScrollMenu{Items: items}
	// Read-only: A or B both leave (the list carries no per-row action here).
	w.pickScroll("Recent activity", "Up/Down: scroll   B: back", m, draw, btn)
}

// nz renders an empty engine token as "0" so a count line never reads "Uploaded  save(s)".
func nz(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// runOnboarding is the first-run flow: welcome -> Wi-Fi -> server -> pair -> name ->
// validate -> initial mirror -> done. It is a back-navigable state machine: B (BtnBack)
// steps backward through server->wifi->welcome, and B on the welcome (first) screen exits
// cleanly to muOS - the user is never trapped (blocker #170). Each network step fails
// honestly onto the error screen, which itself exits on A or retries on B.
func (w *wizard) runOnboarding(draw func(*ui.Canvas), btn func() ui.Button) {
	server, code, device := w.server, w.code, w.device
	w.tsAvail = w.tsAvailable() // computed once; Connect step only exists when true
	s := stepWelcome
	for {
		switch s {
		case stepWelcome:
			if w.screenMessage(stepWelcome, draw, btn) == actBack {
				return // exit the app from the first screen
			}
			s = stepWifi

		case stepWifi:
			// Pairing needs the network. A advances only when online (else re-checks);
			// B always walks back to the welcome screen.
			w.wifiUp = wifiUp()
			if w.screenMessage(stepWifi, draw, btn) == actBack {
				s = stepWelcome
				continue
			}
			if w.wifiUp {
				if w.tsAvail {
					s = stepConnect
				} else {
					s = stepServer
				}
			}

		case stepConnect:
			// How does this device reach RomM? Tailscale (tunnel) or a plain-LAN/public URL.
			choice, act := w.screenChoice(
				"Where is your RomM server?",
				[]string{"Connect via Tailscale", "Home network / public URL"}, draw, btn)
			if act == actBack {
				s = stepWifi
				continue
			}
			if choice == 0 {
				w.useTailscale = true
				s = stepTsSignin
			} else {
				w.useTailscale = false
				s = stepServer
			}

		case stepTsSignin:
			if !w.tsSignIn(draw, btn) {
				s = stepConnect // sign-in cancelled/failed -> back to the choice
				continue
			}
			s = stepServer

		case stepServer:
			kb := &ui.Keyboard{Prompt: "Enter your RomM server address:", Text: server}
			w.screenKeyboard(stepServer, kb, draw, btn)
			if kb.Cancelled {
				if w.tsAvail {
					s = stepConnect
				} else {
					s = stepWifi
				}
				continue
			}
			server = strings.TrimSpace(kb.Text)
			w.server = server
			if out, err := w.runEngine("--set-server", server); err != nil || !resultFlag(out, "server_set") {
				w.errMsg = "Could not save the server address. Check it and try again."
				w.retry, s = stepServer, stepError
				continue
			}
			// Tailscale path: promote the just-written host to a SOCKS5 tier-1 endpoint BEFORE
			// pairing so --pair / --register-device route through the tunnel (engine reads
			// socks5_proxy + tier from config.json). Non-fatal - a plain host still pairs.
			if w.useTailscale {
				_, _ = w.runShim("mark-tier1")
			}
			s = stepCode

		case stepCode:
			kb := &ui.Keyboard{Prompt: "Enter your RomM pairing code:", Text: code, Hint: pairCodeHint}
			w.screenKeyboard(stepCode, kb, draw, btn)
			if kb.Cancelled {
				s = stepServer
				continue
			}
			code = strings.TrimSpace(kb.Text)
			w.code = code
			// lodor#31 failure mapping + the lodor#35 certificate-trust path live in
			// pairServer (extracted so the off-hardware tests drive the real flow).
			s = w.pairServer(server, code, draw, btn)

		case stepDevice:
			kb := &ui.Keyboard{Prompt: "Name this device:", Text: device}
			w.screenKeyboard(stepDevice, kb, draw, btn)
			if kb.Cancelled {
				s = stepCode
				continue
			}
			device = strings.TrimSpace(kb.Text)
			w.device = device
			// lodor#39: keep the outcome — the validate screen shows a "Device registered"
			// row (still non-fatal: pulls work unregistered, the shell lib retries later).
			rout, rerr := w.runEngine("--register-device", device)
			w.registered = exitCode(rerr) == 0 && resultFlag(rout, "registered")
			s = stepValidate

		case stepValidate:
			out, _ := w.runEngine("--validate")
			w.reach = resultFlag(out, "reachable")
			w.auth = resultFlag(out, "auth")
			if w.screenMessage(stepValidate, draw, btn) == actBack {
				s = stepServer // let the user fix the address/code and re-try
				continue
			}
			// Network is up from pairing; mirror directly then flush any pending saves.
			w.screenMirror(draw)
			_, _ = w.runEngine("--push-pending")
			w.screenMessage(stepDone, draw, btn)
			return

		case stepError:
			if w.screenMessage(stepError, draw, btn) == actBack && w.retry != stepWelcome {
				s = w.retry
				continue
			}
			return // A (or an unset retry target) exits cleanly to muOS
		}
	}
}

// screenMessage draws step s and waits for a decision: A/Start advances, B backs out.
// Returning the choice lets every caller honour B, so no message screen is a dead end.
func (w *wizard) screenMessage(s step, draw func(*ui.Canvas), btn func() ui.Button) action {
	for {
		draw(w.render(s, nil))
		switch btn() {
		case ui.BtnConfirm, ui.BtnStart:
			return actAdvance
		case ui.BtnBack:
			return actBack
		}
	}
}

// screenKeyboard runs a text-entry screen until OK/Start.
func (w *wizard) screenKeyboard(s step, kb *ui.Keyboard, draw func(*ui.Canvas), btn func() ui.Button) {
	for {
		draw(w.render(s, kb))
		if kb.Handle(btn()) {
			return
		}
	}
}

// screenMirror runs --mirror-catalog with a live progress bar (onboarding's initial build).
func (w *wizard) screenMirror(draw func(*ui.Canvas)) {
	_ = w.screenMirrorArgs(draw, "--mirror-catalog")
}

// screenMirrorArgs runs a long engine op (mirror-catalog[+--full]) in the background,
// polling the engine's progress side-channels (/tmp/dl-progress, /tmp/romm-phase) and
// rendering an honest progress bar. Returns the engine exit code.
func (w *wizard) screenMirrorArgs(draw func(*ui.Canvas), args ...string) int {
	_ = os.Remove("/tmp/dl-progress")
	_ = os.Remove("/tmp/romm-phase") // clear a stale phase label from a prior op (no-fake-UI)
	// The select loop already keeps the UI live (polls every 400ms), so the mirror never
	// freezes the wizard; the context is a wedge-breaker so a hung engine can't hold the
	// progress screen forever (BUG 2b). engineTimeout is generous — not a tight bound.
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.bin, args...)
	cmd.Dir = w.dataDir
	done := make(chan error, 1)
	go func() { done <- runAndWait(cmd) }()
	for {
		select {
		case err := <-done:
			rc := exitCode(err)
			if rc == 0 {
				// #180A: re-seed the launch overrides IN-SESSION, right after a
				// successful mirror. mux_launch.sh seeds BEFORE the wizard runs, so a
				// first-run or menu-driven Refresh that just created the system folders
				// would otherwise leave MUOS/info/override/ empty until the NEXT app
				// open — every fresh stub launch dead in between (RG40XXV field log
				// 2026-07-05: 6939 stubs mirrored, overrides wired only on relaunch).
				w.reseedOverrides()
			}
			c := ui.NewCanvas(W, H)
			w.t.Progress(c, "Building your library...", "Done", 100)
			draw(c)
			return rc
		case <-time.After(400 * time.Millisecond):
			pct := readPct()
			phase := readPhase()
			c := ui.NewCanvas(W, H)
			w.t.Progress(c, "Building your library...", phase, pct)
			draw(c)
		}
	}
}

func runAndWait(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

func readPct() int {
	b, err := os.ReadFile("/tmp/dl-progress")
	if err != nil {
		return -1
	}
	n := 0
	for _, ch := range strings.TrimSpace(string(b)) {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func readPhase() string {
	b, _ := os.ReadFile("/tmp/romm-phase")
	return strings.TrimSpace(string(b))
}

// seederScript locates the host's launch-override seeder (bin/lodor-seed.sh):
// $LODOR_APPDIR/bin first (mux_launch exports it; tests set it), else next to the
// wizard binary — the same precedence lodor_appdir() uses in the shell lib.
func seederScript() string {
	if d := os.Getenv("LODOR_APPDIR"); d != "" {
		return filepath.Join(d, "bin", "lodor-seed.sh")
	}
	self, _ := os.Executable()
	return filepath.Join(filepath.Dir(self), "bin", "lodor-seed.sh")
}

// reseedOverrides re-runs the host's launch-override seeder IN-SESSION (#180A). The seed
// script owns ALL assign/override knowledge (host plumbing); the wizard only shells it —
// the same engine/host boundary shape as the lodor-ts.sh shim. Honest logging only: the
// success line reports the seeder's OWN overrides count, a failure is loud, and a host
// without the seeder (off-hardware unit tests, non-muOS packaging) is a logged no-op.
// The seeder also re-stamps the seed-gate (lodor-seed.sh stamps itself post-seed), so
// the next app launch skips instead of re-seeding what this call just did.
func (w *wizard) reseedOverrides() {
	script := seederScript()
	if _, err := os.Stat(script); err != nil {
		w.logPhase("seed: post-mirror re-seed skipped - no seeder at %s", script)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), seedTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", script)
	cmd.Dir = w.dataDir
	out, err := runCaptured(ctx, cmd, seedTimeout)
	if err != nil {
		w.logPhase("seed: post-mirror re-seed FAILED: %v", timeoutErr(ctx, err, seedTimeout))
		return
	}
	n := ui.ResultToken(out, "overrides") // the seeder's "SEED overrides=N skipped=M" line
	if n == "" {
		n = "?"
	}
	w.logPhase("seed: post-mirror re-seed, overrides=%s", n)
}

// ---- Tailscale onboarding (host delivery via the lodor-ts.sh shim) --------------------
// The wizard owns NO Tailscale logic: it shells bin/lodor-ts.sh, which sources the ported
// tailscale-lib.sh. That keeps the engine/host boundary intact - the tunnel is host plumbing,
// the same shape NextUI uses.

// shimBin locates lodor-ts.sh next to the wizard binary (APPDIR/bin/lodor-ts.sh).
func (w *wizard) shimBin() string {
	if s := os.Getenv("LODOR_TS_SHIM"); s != "" {
		return s
	}
	self, _ := os.Executable()
	return filepath.Join(filepath.Dir(self), "bin", "lodor-ts.sh")
}

// runShim runs one shim subcommand in the data dir, returning combined output. Errors and
// non-zero exits are returned to the caller (honest: no faked success).
func (w *wizard) runShim(args ...string) (string, error) {
	return w.runShimFor(tsShimTimeout, args...)
}

// runShimFor runs the shim under a hard timeout (BUG 2b): a wedged shell-out is KILLED and
// surfaces as an honest timeout error rather than freezing the wizard. CommandContext kills
// only the /bin/sh child on deadline; the shim detaches tailscaled via nohup/setsid so a
// slow-starting daemon is never collaterally killed (the "never kill a slow starter" rule).
func (w *wizard) runShimFor(d time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{w.shimBin()}, args...)...)
	cmd.Dir = w.dataDir
	// Capture output to a REAL temp file, NOT CombinedOutput. CombinedOutput wires stdout/
	// stderr through an os.Pipe plus a copier goroutine that cmd.Wait() blocks on; a shim that
	// leaves ANY child holding that pipe (a slow tailscaled, a wedged `tailscale status`) keeps
	// the write end open, so Wait()/CombinedOutput never returns even after the context has
	// killed /bin/sh — the timeout is silently defeated and the menu wedges at "menu: build
	// state" (the exact 2026-07-04 field symptom; reproduced deterministically in the off-
	// hardware real-loop rig). A real *os.File is dup'd straight to the child with NO copier
	// goroutine, so Wait() returns the instant the direct /bin/sh child is reaped/killed and a
	// grandchild inheriting the fd cannot block us. Context still kills ONLY the direct child,
	// so a nohup/setsid-detached daemon is never collaterally killed (the "never kill a slow
	// starter" invariant holds). If the temp file can't be made we fall back to CombinedOutput
	// (correct output, original wedge risk) rather than losing the shim call entirely.
	tf, ferr := os.CreateTemp("", "lodor-shim-*.out")
	if ferr != nil {
		out, err := cmd.CombinedOutput()
		return string(out), timeoutErr(ctx, err, d)
	}
	defer os.Remove(tf.Name())
	cmd.Stdout, cmd.Stderr = tf, tf
	runErr := cmd.Run()
	_ = tf.Sync()
	b, _ := os.ReadFile(tf.Name())
	_ = tf.Close()
	return string(b), timeoutErr(ctx, runErr, d)
}

// tsAvailable reports whether the Tailscale option should be offered: the shim exists and
// `available` exits 0 (binaries bundled + device capable). Off-hardware / in the sim tests
// the shim is absent, so this is false and the Connect step is skipped entirely.
func (w *wizard) tsAvailable() bool {
	if _, err := os.Stat(w.shimBin()); err != nil {
		return false
	}
	// SHORT ceiling: this runs during menu-build (first paint). On timeout/error we degrade
	// to "unavailable" and keep going — NOTHING blocks the first menu render (BUG 2b).
	if _, err := w.runShimFor(tsProbeTimeout, "available"); err != nil {
		w.logPhase("tsAvailable: shim probe failed/timeout (%v) — degrading to false", err)
		return false
	}
	return true
}

// tsConnected reports the tunnel as Running per the shim's status token.
func (w *wizard) tsConnected() bool {
	out, _ := w.runShimFor(tsProbeTimeout, "status") // quick probe; polled in a loop
	return strings.TrimSpace(firstLine(out)) == "connected"
}

// tsSignIn runs the interactive (no-authkey) Tailscale sign-in: bring up userspace tailscaled,
// scrape the login URL, show it, and poll to Running. A/Start re-checks now, B cancels. Returns
// true only when the node is actually connected. Honest throughout (no fake progress).
func (w *wizard) tsSignIn(draw func(*ui.Canvas), btn func() ui.Button) bool {
	w.tsURL, w.tsPhase = "", "Starting Tailscale sign-in..."
	draw(w.render(stepTsSignin, nil))

	out, _ := w.runShim("up-interactive")
	url := strings.TrimSpace(firstLine(out))
	if url == "" {
		// Empty URL = already signed in (persisted state) OR bring-up failed. Disambiguate.
		if w.tsConnected() {
			return true
		}
		w.tsPhase = "Couldn't start Tailscale sign-in. Check Wi-Fi.\n\nPress B to go back and choose Home / public URL."
		return w.screenMessage(stepTsSignin, draw, btn) == actAdvance && w.tsConnected()
	}
	w.tsURL = url
	w.tsPhase = "Open this link in a browser and approve this device. Waiting for sign-in... (B to cancel)"

	// Poll status on a ticker while remaining responsive to B (cancel). w.in is the live
	// evdev channel on-device; when nil (never reached without a real device) we fall back
	// to a plain timed poll so the loop still terminates.
	deadline := time.Now().Add(180 * time.Second)
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	var ch <-chan ui.Button
	if w.in != nil {
		ch = w.in.Buttons()
	}
	for time.Now().Before(deadline) {
		if w.tsConnected() {
			w.tsPhase = "Connected to Tailscale."
			return true
		}
		draw(w.render(stepTsSignin, nil))
		select {
		case b := <-ch:
			if b == ui.BtnBack {
				_, _ = w.runShim("down")
				return false
			}
		case <-ticker.C:
		}
	}
	// Timed out - tear down so a retry starts clean, and report honestly.
	_, _ = w.runShim("down")
	return false
}

// screenChoice draws a titled single-select menu until A/Start (returns the index + actAdvance)
// or B (returns -1 + actBack). Used by the Connect step; back is always honoured.
func (w *wizard) screenChoice(subtitle string, items []string, draw func(*ui.Canvas), btn func() ui.Button) (int, action) {
	m := &ui.Menu{Items: items}
	for {
		c := ui.NewCanvas(W, H)
		x, y, ww, hh := w.t.Frame(c, "Lodor Setup", "Up/Down: move   A: select   B: back")
		c.DrawText(x, y, subtitle, w.t.Text, w.t.BodyScale)
		m.Draw(c, w.t, x, y+50, ww, hh-50)
		draw(c)
		b := btn()
		if b == ui.BtnBack {
			return -1, actBack
		}
		if m.Handle(b) {
			return m.Selected(), actAdvance
		}
	}
}

// firstLine returns the first line of s (the shim may log extra lines; the token/URL is line 1).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- G2: Tailscale maintenance (host delivery via lodor-ts.sh; no TS logic in the wizard)

func (w *wizard) doTsStatus(draw func(*ui.Canvas), btn func() ui.Button) {
	out, _ := w.runShimFor(tsProbeTimeout, "status")
	if strings.TrimSpace(firstLine(out)) == "connected" {
		ip, _ := w.runShimFor(tsProbeTimeout, "ip")
		body := "Tailscale: CONNECTED"
		if s := strings.TrimSpace(firstLine(ip)); s != "" {
			body += "\n" + s
		}
		w.showMsg("Tailscale", body, w.t.Good, draw, btn)
		return
	}
	w.showMsg("Tailscale", "Not connected. Re-onboard via Setup / Re-pair.", w.t.Bad, draw, btn)
}

func (w *wizard) doTsReconnect(draw func(*ui.Canvas), btn func() ui.Button) {
	w.working(draw, "Reconnecting Tailscale...")
	out, _ := w.runShim("reconnect")
	tok := strings.TrimSpace(firstLine(out))
	switch {
	case strings.HasPrefix(tok, "connected:"):
		w.showMsg("Tailscale", "Reconnected ("+strings.TrimPrefix(tok, "connected:")+").", w.t.Good, draw, btn)
	case tok == "connected":
		w.showMsg("Tailscale", "Reconnected.", w.t.Good, draw, btn)
	case tok == "no-login":
		w.showMsg("Tailscale", "No saved sign-in. Re-onboard via Setup / Re-pair.", w.t.Bad, draw, btn)
	default:
		w.showMsg("Tailscale", "Couldn't reconnect - didn't reach Running. Check Wi-Fi.", w.t.Bad, draw, btn)
	}
}

func (w *wizard) doTsReset(draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.confirmScreen("Reset & forget Tailscale? You'll re-sign-in next time.", "Reset & forget", "Cancel", draw, btn) {
		return
	}
	w.working(draw, "Resetting Tailscale...")
	_, _ = w.runShim("reset")
	w.showMsg("Tailscale", "Reset. Re-onboard via Setup / Re-pair when you need it.", w.t.Text, draw, btn)
}

// ---- G4: multi-user -------------------------------------------------------------------

// doSwitchUser lists profiles (--list-profiles) and either switches to an already-signed-in
// one (write active-profile.txt, offline) or signs into a token-less one (runEngineStdin
// --login-profile, password on stdin). TSV: "<active>\t<label>\t<hastoken>\t<hasdevice>".
func (w *wizard) doSwitchUser(draw func(*ui.Canvas), btn func() ui.Button) {
	out, err := w.runEngine("--list-profiles")
	if rc := exitCode(err); rc != 0 {
		w.showMsg("Switch user", w.diagnose(rc, out), w.t.Bad, draw, btn)
		return
	}
	type prof struct {
		label  string
		active bool
		hasTok bool
	}
	var profs []prof
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Split(ln, "\t")
		if len(f) < 4 {
			continue
		}
		profs = append(profs, prof{label: f[1], active: f[0] == "1", hasTok: f[2] == "1"})
	}
	if len(profs) == 0 {
		w.showMsg("Switch user", "No profiles found - add one via Add profile.", w.t.Text, draw, btn)
		return
	}
	labels := make([]string, len(profs))
	for i, p := range profs {
		labels[i] = p.label
		if p.active {
			labels[i] += "  (active)"
		}
		if !p.hasTok {
			labels[i] += "  (sign in)"
		}
	}
	m := &ui.ScrollMenu{Items: labels}
	sel, ok := w.pickScroll("Switch user", "Up/Down: move   A: select   B: back", m, draw, btn)
	if !ok {
		return
	}
	p := profs[sel]
	if p.hasTok {
		// FAT32-durable (fsutil): active-profile.txt names WHO is signed in. A plain
		// WriteFile zeroed by a power-yank silently falls the card back to the admin
		// identity (empty/missing file = hosts[0]) — atomic temp+fsync+rename+dir-fsync.
		if err := fsutil.WriteFileAtomicString(filepath.Join(w.dataDir, "active-profile.txt"), p.label+"\n", 0o644); err != nil {
			w.showMsg("Switch user", "Couldn't switch profile - check the SD card.", w.t.Bad, draw, btn)
			return
		}
		w.showMsg("Switch user", "Switched to "+p.label+".", w.t.Good, draw, btn)
		return
	}
	// token-less profile: sign in (username + password), password on stdin.
	w.loginProfile(p.label, draw, btn)
}

// loginProfile signs into an existing RomM user under a profile label: keyboard username +
// password, then runEngineStdin --login-profile <label> --login-user <u> (password on stdin).
func (w *wizard) loginProfile(label string, draw func(*ui.Canvas), btn func() ui.Button) {
	kbu := &ui.Keyboard{Prompt: "RomM username for " + label + ":"}
	w.screenKeyboardFree(kbu, draw, btn)
	if kbu.Cancelled {
		return
	}
	user := strings.TrimSpace(kbu.Text)
	if user == "" {
		return
	}
	kbp := &ui.Keyboard{Prompt: "Password (hidden after OK):"}
	w.screenKeyboardFree(kbp, draw, btn)
	if kbp.Cancelled {
		return
	}
	w.working(draw, "Signing in...")
	out, err := w.runEngineStdin(kbp.Text, "--login-profile", label, "--login-user", user)
	rc := exitCode(err)
	if rc == 0 && ui.ResultToken(out, "logged_in") == "1" {
		// FAT32-durable (fsutil): same power-yank guarantee as the Switch-user write.
		_ = fsutil.WriteFileAtomicString(filepath.Join(w.dataDir, "active-profile.txt"), label+"\n", 0o644)
		w.showMsg("Signed in", "Signed in and switched to "+label+".", w.t.Good, draw, btn)
	} else {
		w.showMsg("Sign in", w.diagnose(rc, out)+"\nCheck the username and password.", w.t.Bad, draw, btn)
	}
}

// doAddProfile pairs a NEW profile with a RomM pairing code (--pair-profile <code>).
func (w *wizard) doAddProfile(draw func(*ui.Canvas), btn func() ui.Button) {
	kb := &ui.Keyboard{Prompt: "Enter a RomM pairing code for the new profile:", Hint: pairCodeHint}
	w.screenKeyboardFree(kb, draw, btn)
	if kb.Cancelled {
		return
	}
	code := strings.TrimSpace(kb.Text)
	if code == "" {
		return
	}
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Pairing new profile...")
	out, err := w.runEngine("--pair-profile", code)
	rc := exitCode(err)
	if rc == 0 && ui.ResultToken(out, "paired") == "1" {
		u := ui.ResultToken(out, "username")
		if u == "" {
			u = "the new profile"
		}
		w.showMsg("Profile added", "Paired "+u+". Use Switch user to activate it.", w.t.Good, draw, btn)
	} else {
		w.showMsg("Add profile", w.diagnose(rc, out)+"\nGenerate a fresh code in RomM and try again.", w.t.Bad, draw, btn)
	}
}

// screenKeyboardFree runs a text-entry screen (framed, no wizard step) until OK/BACK.
func (w *wizard) screenKeyboardFree(kb *ui.Keyboard, draw func(*ui.Canvas), btn func() ui.Button) {
	for {
		c := ui.NewCanvas(W, H)
		x, y, ww, hh := w.t.Frame(c, "Lodor", "D-pad: move   A: type   B: delete   BACK: cancel   Start: OK")
		kb.Draw(c, w.t, x, y, ww, hh)
		draw(c)
		if kb.Handle(btn()) {
			return
		}
	}
}

// ---- G5: RetroAchievements ------------------------------------------------------------

func (w *wizard) doRetroAch(draw func(*ui.Canvas), btn func() ui.Button) {
	for {
		m := &ui.ScrollMenu{Items: []string{"RA status", "RA login"}}
		sel, ok := w.pickScroll("RetroAchievements", "Up/Down: move   A: select   B: back", m, draw, btn)
		if !ok {
			return
		}
		switch sel {
		case 0:
			out, _ := w.runEngine("--ra-status")
			if ui.ResultToken(out, "ra_logged_in") == "1" {
				w.showMsg("RetroAchievements", "Logged in as "+nzs(ui.ResultToken(out, "ra_user")), w.t.Good, draw, btn)
			} else {
				w.showMsg("RetroAchievements", "Not logged in. Choose RA login to sign in.", w.t.Text, draw, btn)
			}
		case 1:
			w.raLogin(draw, btn)
		}
	}
}

func (w *wizard) raLogin(draw func(*ui.Canvas), btn func() ui.Button) {
	kbu := &ui.Keyboard{Prompt: "RetroAchievements username:"}
	w.screenKeyboardFree(kbu, draw, btn)
	if kbu.Cancelled {
		return
	}
	user := strings.TrimSpace(kbu.Text)
	if user == "" {
		return
	}
	kbp := &ui.Keyboard{Prompt: "RetroAchievements password:"}
	w.screenKeyboardFree(kbp, draw, btn)
	if kbp.Cancelled {
		return
	}
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Signing in to RetroAchievements...")
	out, err := w.runEngineStdin(kbp.Text, "--ra-login", user)
	if exitCode(err) == 0 && ui.ResultToken(out, "ra_login") == "1" {
		w.showMsg("RetroAchievements", "Signed in as "+user+".", w.t.Good, draw, btn)
	} else {
		w.showMsg("RetroAchievements", "Sign in failed - check the username and password.", w.t.Bad, draw, btn)
	}
}

func nzs(s string) string {
	if s == "" {
		return "your RA account"
	}
	return s
}

// ---- G6: Game Manager + Search + box art + remove -------------------------------------

// romsDir resolves the on-card ROMS root: ROMS_DIR (pinned by mux_launch's lodor_export_env
// to the live rom mount), else the known muOS mounts. The on-card stub mirror under here IS
// the whole catalog (downloaded + 0-byte cloud stubs) — the SAME source NextUI's Game
// Manager uses, and it carries real file paths the engine acts on (--download/--evict/etc).
func (w *wizard) romsDir() string {
	if d := os.Getenv("ROMS_DIR"); d != "" {
		return d
	}
	for _, d := range []string{"/mnt/mmc/ROMS", "/mnt/sdcard/ROMS"} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return "/mnt/mmc/ROMS"
}

// listSubdirs returns the visible immediate subdirectory names of dir, sorted.
func listSubdirs(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// listGameFiles returns the visible ROM files in a system dir, sorted (skips hidden + map.txt).
func listGameFiles(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "map.txt" {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// isDownloaded: a real ROM has bytes; a 0-byte file is a cloud stub (the exact ground-truth
// test lodor-override.sh uses for fetch-on-launch).
func isDownloaded(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() > 0
}

func (w *wizard) gameManager(draw func(*ui.Canvas), btn func() ui.Button) {
	for {
		systems := listSubdirs(w.romsDir())
		items := append([]string{"Search library"}, systems...)
		if len(systems) == 0 {
			w.showMsg("Game Manager", "No game library on this card yet - run Refresh library first.", w.t.Text, draw, btn)
			return
		}
		m := &ui.ScrollMenu{Items: items}
		sel, ok := w.pickScroll("Game Manager", "Up/Down: move   A: open   B: back", m, draw, btn)
		if !ok {
			return
		}
		if sel == 0 {
			w.searchLibrary(draw, btn)
			continue
		}
		w.gmGames(systems[sel-1], draw, btn)
	}
}

func (w *wizard) gmGames(system string, draw func(*ui.Canvas), btn func() ui.Button) {
	sysdir := filepath.Join(w.romsDir(), system)
	for {
		files := listGameFiles(sysdir)
		if len(files) == 0 {
			w.showMsg(system, "No games in "+system+" yet.", w.t.Text, draw, btn)
			return
		}
		labels := make([]string, len(files))
		for i, f := range files {
			mark := "cloud"
			if isDownloaded(filepath.Join(sysdir, f)) {
				mark = "on card"
			}
			labels[i] = "[" + mark + "] " + f
		}
		m := &ui.ScrollMenu{Items: labels}
		sel, ok := w.pickScroll(system, "Up/Down: move   A: manage   B: back", m, draw, btn)
		if !ok {
			return
		}
		w.gmActions(filepath.Join(sysdir, files[sel]), draw, btn)
	}
}

// gmActions is the per-game menu; each action is one confirmed engine mode. Rebuilds the
// list (returns) after an action that renames the file (download reconcile ✘->✓, evict ✓->✘).
func (w *wizard) gmActions(path string, draw func(*ui.Canvas), btn func() ui.Button) {
	for {
		if _, err := os.Stat(path); err != nil {
			return // renamed/evicted under us -> caller rebuilds the list
		}
		var acts []string
		if isDownloaded(path) {
			acts = []string{"Delete from card", "Sync save now", "Server saves", "Details"}
		} else {
			acts = []string{"Download now", "Add to queue", "Sync save now", "Server saves", "Details"}
		}
		if w.statesLive() {
			// Handoff v1: only when the assembler shipped statecores.json —
			// a dark build gets no dead menu row (design D7).
			acts = append(acts[:len(acts)-1], "Continue from another device", "Details")
		}
		m := &ui.ScrollMenu{Items: acts}
		sel, ok := w.pickScroll(filepath.Base(path), "Up/Down: move   A: select   B: back", m, draw, btn)
		if !ok {
			return
		}
		switch acts[sel] {
		case "Download now":
			if w.gmDownload(path, draw, btn) {
				return
			}
		case "Add to queue":
			w.gmQueueAdd(path, draw, btn)
		case "Delete from card":
			if w.gmDelete(path, draw, btn) {
				return
			}
		case "Sync save now":
			w.gmSyncSave(path, draw, btn)
		case "Server saves":
			w.gmServerSaves(path, draw, btn)
		case "Continue from another device":
			w.gmServerStates(path, draw, btn)
		case "Details":
			w.gmDetails(path, draw, btn)
		}
	}
}

// gmDownload: --download then the offline ✘->✓ --reconcile. Returns true (path changed) so
// the caller rebuilds. Honest success only: engine downloaded=1 AND the file has bytes now.
func (w *wizard) gmDownload(path string, draw func(*ui.Canvas), btn func() ui.Button) bool {
	if !w.requireOnline(draw, btn) {
		return false
	}
	w.working(draw, "Downloading "+filepath.Base(path)+"...")
	out, err := w.runEngine("--download", path)
	rc := exitCode(err)
	w.markPairFlag(rc)
	if rc == 0 && strings.Contains(out, "downloaded=1") && isDownloaded(path) {
		_, _ = w.runEngine("--reconcile", path) // offline ✘->✓, carries save+cover
		w.showMsg("Downloaded", "It's on the card now.", w.t.Good, draw, btn)
		return true
	}
	if rc != 0 {
		w.showMsg("Problem", w.diagnose(rc, out), w.t.Bad, draw, btn)
	} else {
		w.showMsg("Problem", "Couldn't download "+filepath.Base(path)+" - try again.", w.t.Bad, draw, btn)
	}
	return false
}

// gmQueueAdd appends the SDCARD-relative ROM path to download-queue.txt (the exact format
// --download-queue reads; absolute is also accepted), deduped so double-taps never double.
func (w *wizard) gmQueueAdd(path string, draw func(*ui.Canvas), btn func() ui.Button) {
	rel := path
	if sd := os.Getenv("SDCARD_PATH"); sd != "" {
		rel = strings.TrimPrefix(path, strings.TrimRight(sd, "/")+"/")
	}
	qf := filepath.Join(w.dataDir, "download-queue.txt")
	for _, ln := range strings.Split(readFileString(qf), "\n") {
		if strings.TrimSpace(ln) == rel {
			w.showMsg("Queue", "Already queued - run Download queue from the menu.", w.t.Text, draw, btn)
			return
		}
	}
	f, err := os.OpenFile(qf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		w.showMsg("Queue", "Couldn't write the queue - check the SD card.", w.t.Bad, draw, btn)
		return
	}
	_, werr := f.WriteString(rel + "\n")
	_ = f.Close()
	if werr != nil {
		w.showMsg("Queue", "Couldn't write the queue - check the SD card.", w.t.Bad, draw, btn)
		return
	}
	w.showMsg("Queue", "Queued - download it any time from the Lodor menu.", w.t.Good, draw, btn)
}

// gmDelete: confirm -> --evict (offline). Save is kept. Returns true (path changed).
func (w *wizard) gmDelete(path string, draw func(*ui.Canvas), btn func() ui.Button) bool {
	if !w.confirmScreen("Delete "+filepath.Base(path)+"? Your save data is kept.", "Delete from card", "Keep it", draw, btn) {
		return false
	}
	w.working(draw, "Deleting "+filepath.Base(path)+"...")
	out, _ := w.runEngine("--evict", path)
	if ui.ResultToken(out, "evicted") == "1" {
		w.showMsg("Deleted", "Deleted from card - still in your library to re-download.", w.t.Good, draw, btn)
		return true
	}
	w.showMsg("Problem", "Couldn't delete "+filepath.Base(path)+" - check the SD card.", w.t.Bad, draw, btn)
	return false
}

// gmSyncSave: targeted --sync-save; report the engine's honest reason/direction token.
func (w *wizard) gmSyncSave(path string, draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Syncing save for "+filepath.Base(path)+"...")
	out, err := w.runEngine("--sync-save", path)
	rc := exitCode(err)
	w.markPairFlag(rc)
	if rc != 0 {
		w.showMsg("Problem", w.diagnose(rc, out), w.t.Bad, draw, btn)
		return
	}
	pulled := ui.ResultToken(out, "pulled")
	pushed := ui.ResultToken(out, "pushed")
	reason := ui.ResultToken(out, "reason")
	var msg string
	switch {
	case reason == "in-sync" || (pulled == "0" && pushed == "0"):
		msg = "Save already in sync."
	case pulled == "1" && pushed == "1":
		msg = "Save synced - newer server copy pulled, yours pushed."
	case pushed == "1":
		msg = "Save uploaded to your server."
	case pulled == "1":
		msg = "Newer save pulled from your server."
	default:
		if s := ui.ParseEngineResult(out); s != "" {
			msg = s
		} else {
			msg = "Save sync finished."
		}
	}
	w.showMsg("Sync save", msg, w.t.Good, draw, btn)
}

// gmServerSaves: --list-saves picker -> confirm -> --restore-save. TSV per row:
// "<id>\t<date>\t<who>\t<kb>KB[\tCURRENT]", trailed by a single-field LOCAL= line (dropped
// by the >=2-field filter). rc is checked FIRST so an unreachable server can never render
// as "no server saves".
func (w *wizard) gmServerSaves(path string, draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Checking server saves...")
	out, err := w.runEngine("--list-saves", path)
	if rc := exitCode(err); rc != 0 {
		w.markPairFlag(rc)
		w.showMsg("Server saves", w.diagnose(rc, out), w.t.Bad, draw, btn)
		return
	}
	type sv struct{ id, label string }
	var saves []sv
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Split(ln, "\t")
		if len(f) < 2 {
			continue
		}
		label := f[1]
		if len(f) >= 3 && strings.TrimSpace(f[2]) != "" {
			label += "  -  " + f[2]
		}
		if len(f) >= 4 && strings.TrimSpace(f[3]) != "" {
			label += "  -  " + f[3]
		}
		if len(f) >= 5 && f[4] == "CURRENT" {
			label += "  (on this device)"
		}
		saves = append(saves, sv{id: f[0], label: label})
	}
	if len(saves) == 0 {
		w.showMsg("Server saves", "No server saves for "+filepath.Base(path)+".", w.t.Text, draw, btn)
		return
	}
	labels := make([]string, len(saves))
	for i, s := range saves {
		labels[i] = s.label
	}
	m := &ui.ScrollMenu{Items: labels}
	sel, ok := w.pickScroll("Server saves", "Up/Down: move   A: restore   B: back", m, draw, btn)
	if !ok {
		return
	}
	if !w.confirmScreen("Restore this save? Your current save is preserved first.", "Restore this save", "Cancel", draw, btn) {
		return
	}
	w.working(draw, "Restoring save...")
	rout, rerr := w.runEngine("--restore-save", path, saves[sel].id)
	if exitCode(rerr) == 0 && ui.ResultToken(rout, "restored") == "1" {
		if s := ui.ResultToken(rout, "staged"); s != "" && s != "0" {
			w.showMsg("Restored", "Save restored - your previous save is queued to upload.", w.t.Good, draw, btn)
		} else {
			w.showMsg("Restored", "Save restored - launch the game from your library.", w.t.Good, draw, btn)
		}
	} else {
		w.showMsg("Problem", "Restore failed - try again.", w.t.Bad, draw, btn)
	}
}

// statesLive: the Handoff states feature is live on this build only when the
// assembler shipped statecores.json next to config.json (design D7 — no
// manifest, no feature, no dead menu row).
func (w *wizard) statesLive() bool {
	_, err := os.Stat(filepath.Join(w.dataDir, "statecores.json"))
	return err == nil
}

// gmServerStates: Handoff v1 "Continue from another device" — --list-states
// picker -> confirm -> --pull-state. Incompatible states stay VISIBLE (the
// timeline is real) but explain themselves instead of placing; placement
// itself never destroys (engine invariant 7.1: occupant uploaded first + .bak).
func (w *wizard) gmServerStates(path string, draw func(*ui.Canvas), btn func() ui.Button) {
	if !w.requireOnline(draw, btn) {
		return
	}
	w.working(draw, "Checking save states on your server...")
	out, err := w.runEngine("--list-states", path)
	rc := exitCode(err)
	w.markPairFlag(rc)
	if rc != 0 {
		w.showMsg("Continue", w.diagnose(rc, out), w.t.Bad, draw, btn)
		return
	}
	type offer struct {
		id, label, why string
		compat         bool
	}
	var offers []offer
	for _, ln := range strings.Split(out, "\n") {
		if !strings.HasPrefix(ln, "LISTSTATE ") {
			continue
		}
		kv := parseStateLine(ln)
		if kv["id"] == "" {
			continue
		}
		lbl := "Slot " + kv["slot"]
		if kv["slot"] == "auto" {
			lbl = "Auto-resume"
		}
		if age, aerr := strconv.ParseInt(kv["age"], 10, 64); aerr == nil {
			lbl += "  -  " + humanAge(age)
		}
		lbl += "  -  " + originLabel(kv["origin"])
		o := offer{id: kv["id"], why: kv["why"], compat: kv["compat"] == "1"}
		if !o.compat {
			lbl += "  (can't use here)"
		}
		o.label = lbl
		offers = append(offers, o)
	}
	if len(offers) == 0 {
		switch ui.ResultToken(out, "reason") {
		case "no-manifest":
			w.showMsg("Continue", "State sync isn't enabled on this build.", w.t.Text, draw, btn)
		case "no-system":
			w.showMsg("Continue", "State sync doesn't cover this system yet.", w.t.Text, draw, btn)
		default:
			w.showMsg("Continue", "No save states on your server for "+filepath.Base(path)+".", w.t.Text, draw, btn)
		}
		return
	}
	labels := make([]string, len(offers))
	for i, o := range offers {
		labels[i] = o.label
	}
	m := &ui.ScrollMenu{Items: labels}
	sel, ok := w.pickScroll("Continue from another device", "Up/Down: move   A: continue   B: back", m, draw, btn)
	if !ok {
		return
	}
	o := offers[sel]
	if !o.compat {
		why := o.why
		if why == "" || why == "-" {
			why = "an unknown difference"
		}
		w.showMsg("Can't use this one", "This state can't run here: "+why+".\nUse it on the device it came from.", w.t.Text, draw, btn)
		return
	}
	if !w.confirmScreen("Continue from this state? Anything already in that slot is preserved first.", "Continue here", "Cancel", draw, btn) {
		return
	}
	w.working(draw, "Placing state...")
	pout, perr := w.runEngine("--pull-state", path, "--state-id", o.id)
	if exitCode(perr) == 0 && ui.ResultToken(pout, "placedstate") == "1" {
		w.showMsg("Ready", "State placed - load it from the in-game menu.", w.t.Good, draw, btn)
		return
	}
	switch ui.ResultToken(pout, "reason") {
	case "occupant-unsafe":
		w.showMsg("Not placed", "Couldn't back up the state already in that slot, so nothing was changed. Try again with the server reachable.", w.t.Bad, draw, btn)
	case "size-mismatch":
		w.showMsg("Not placed", "That state doesn't match this system's expected size - refused rather than risk a corrupt load.", w.t.Bad, draw, btn)
	case "incompatible":
		w.showMsg("Not placed", "That state isn't compatible with this device.", w.t.Bad, draw, btn)
	case "offline":
		w.showMsg("Not placed", "Server unreachable - nothing was changed. Try again.", w.t.Bad, draw, btn)
	default:
		w.showMsg("Problem", "State placement failed - nothing was changed.", w.t.Bad, draw, btn)
	}
}

// parseStateLine parses one LISTSTATE machine line into key->value, tolerating
// Go-%q-quoted values (why= and name= may contain spaces).
func parseStateLine(line string) map[string]string {
	out := map[string]string{}
	s := strings.TrimPrefix(line, "LISTSTATE ")
	for s != "" {
		s = strings.TrimLeft(s, " ")
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			break
		}
		key := s[:eq]
		rest := s[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			q, qerr := strconv.QuotedPrefix(rest)
			if qerr != nil {
				break
			}
			val, _ = strconv.Unquote(q)
			s = rest[len(q):]
		} else if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			val, s = rest[:sp], rest[sp:]
		} else {
			val, s = rest, ""
		}
		out[key] = val
	}
	return out
}

// humanAge renders an age in seconds the way a person reads a timeline.
func humanAge(secs int64) string {
	switch {
	case secs < 90:
		return "just now"
	case secs < 3600:
		return strconv.FormatInt(secs/60, 10) + "m ago"
	case secs < 86400:
		return strconv.FormatInt(secs/3600, 10) + "h ago"
	default:
		return strconv.FormatInt(secs/86400, 10) + "d ago"
	}
}

// originLabel turns a producer tuple (lodor/<frontend>/<core>@<ver>/<arch>)
// into a human origin. Foreign records (no tuple) come from other clients.
func originLabel(origin string) string {
	if strings.HasPrefix(origin, "foreign:") {
		return "another app"
	}
	p := strings.Split(origin, "/")
	if len(p) != 4 || p[0] != "lodor" {
		return "unknown device"
	}
	switch p[1] {
	case "lodoros":
		return "a LodorOS device"
	case "nextui":
		return "a NextUI device"
	case "muos":
		return "a muOS device"
	case "knulli":
		return "a Knulli device"
	case "onion":
		return "an OnionOS device"
	}
	return "a " + p[1] + " device"
}

// gmDetails: offline text details (name / system / state+size / free space). No cover
// thumbnail in v1 (ui has no image decoder — deferred, not faked).
func (w *wizard) gmDetails(path string, draw func(*ui.Canvas), btn func() ui.Button) {
	name := filepath.Base(path)
	system := filepath.Base(filepath.Dir(path))
	state := "In your cloud library (not downloaded)"
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		state = "On this card (" + humanSize(fi.Size()) + ")"
	}
	body := "System: " + system + "\nStatus: " + state
	w.showMsg(name, body, w.t.Text, draw, btn)
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", (b+1023)/1024)
	}
}

// searchLibrary: keyboard term -> case-insensitive substring over EVERY on-card game
// (system/game two-level walk of the stub mirror) -> same per-game actions. No engine
// search mode exists; this filters the local library (offline).
func (w *wizard) searchLibrary(draw func(*ui.Canvas), btn func() ui.Button) {
	kb := &ui.Keyboard{Prompt: "Search your library:"}
	w.screenKeyboardFree(kb, draw, btn)
	if kb.Cancelled {
		return
	}
	q := strings.ToLower(strings.TrimSpace(kb.Text))
	if q == "" {
		w.showMsg("Search", "Type part of a game name to search.", w.t.Text, draw, btn)
		return
	}
	for {
		var paths, labels []string
		for _, sys := range listSubdirs(w.romsDir()) {
			sysdir := filepath.Join(w.romsDir(), sys)
			for _, f := range listGameFiles(sysdir) {
				if !strings.Contains(strings.ToLower(f), q) {
					continue
				}
				paths = append(paths, filepath.Join(sysdir, f))
				labels = append(labels, f+"  -  "+sys)
				if len(paths) >= 300 {
					break
				}
			}
			if len(paths) >= 300 {
				break
			}
		}
		if len(paths) == 0 {
			w.showMsg("Search", "No games match \""+strings.TrimSpace(kb.Text)+"\".", w.t.Text, draw, btn)
			return
		}
		m := &ui.ScrollMenu{Items: labels}
		sel, ok := w.pickScroll("Search: "+strings.TrimSpace(kb.Text), "Up/Down: move   A: manage   B: back", m, draw, btn)
		if !ok {
			return
		}
		w.gmActions(paths[sel], draw, btn)
	}
}

// doToggleCovers flips fetch_covers in settings.conf (offline; label reflects state next
// menu render). Whole-library covers download on the next Refresh (full).
func (w *wizard) doToggleCovers(draw func(*ui.Canvas), btn func() ui.Button) {
	next := "on"
	if w.fetchCoversOn() {
		next = "off"
	}
	if err := w.setSetting("fetch_covers", next); err != nil {
		w.showMsg("Box art", "Couldn't save the setting - check the SD card.", w.t.Bad, draw, btn)
		return
	}
	if next == "on" {
		w.showMsg("Box art", "Box art for your whole library will download on the next Refresh library (full).", w.t.Good, draw, btn)
	} else {
		w.showMsg("Box art", "Only downloaded games fetch box art now. Art already on the card is kept.", w.t.Good, draw, btn)
	}
}

// getSetting reads one key=value from settings.conf ("" when absent). Same file the
// NextUI pak's set_setting writes; the wizard is the settings writer on muOS.
func (w *wizard) getSetting(key string) string {
	for _, ln := range strings.Split(readFileString(filepath.Join(w.dataDir, "settings.conf")), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, key+"=") {
			return strings.TrimPrefix(ln, key+"=")
		}
	}
	return ""
}

// fetchCoversOn reads fetch_covers from settings.conf, falling back to config.json's
// "fetch_covers": true. Default off.
func (w *wizard) fetchCoversOn() bool {
	for _, ln := range strings.Split(readFileString(filepath.Join(w.dataDir, "settings.conf")), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "fetch_covers=") {
			return strings.TrimPrefix(ln, "fetch_covers=") == "on"
		}
	}
	return strings.Contains(readFileString(filepath.Join(w.dataDir, "config.json")), "\"fetch_covers\": true") ||
		strings.Contains(readFileString(filepath.Join(w.dataDir, "config.json")), "\"fetch_covers\":true")
}

// setSetting writes key=value into settings.conf (other keys preserved). FAT32-durable
// via fsutil: temp + fsync + rename + parent-dir fsync — a bare temp+rename leaves the
// rename pointing at unflushed blocks on a power-yank, zeroing every setting.
func (w *wizard) setSetting(key, value string) error {
	path := filepath.Join(w.dataDir, "settings.conf")
	var lines []string
	found := false
	for _, ln := range strings.Split(readFileString(path), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), key+"=") {
			lines = append(lines, key+"="+value)
			found = true
		} else if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	return fsutil.WriteFileAtomicString(path, strings.Join(lines, "\n")+"\n", 0o644)
}

// doRemoveLodor: confirm (keep vs also remove downloads) -> --uninstall-mirror. Offline.
// Saves are NEVER touched by the engine; user files stay byte-identical.
func (w *wizard) doRemoveLodor(draw func(*ui.Canvas), btn func() ui.Button) {
	choice, act := w.screenChoice("Remove Lodor from this card?",
		[]string{"Keep my downloaded games", "Also remove downloaded games"}, draw, btn)
	if act == actBack {
		return
	}
	if !w.confirmScreen("Are you sure? This removes Lodor's stubs, covers and folders.", "Remove", "Cancel", draw, btn) {
		return
	}
	args := []string{"--uninstall-mirror"}
	if choice == 1 {
		args = append(args, "--remove-downloads")
	}
	w.working(draw, "Removing Lodor files...")
	out, _ := w.runEngine(args...)
	if ui.ResultToken(out, "uninstalled") == "1" {
		w.showMsg("Removed", fmt.Sprintf("Removed %s Lodor file(s). Delete the Lodor app to finish.", nz(ui.ResultToken(out, "removed"))), w.t.Good, draw, btn)
	} else {
		w.showMsg("Remove Lodor", "Nothing removed - Lodor's file records were missing. Run Refresh library once, then retry.", w.t.Bad, draw, btn)
	}
}

// readFileString reads a file or returns "" (missing/unreadable) — a small local helper.
func readFileString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// ---- capture mode (off-hardware verification) -----------------------------------------

func (w *wizard) capture(dir string) {
	_ = os.MkdirAll(dir, 0o755)
	type shot struct {
		name string
		s    step
		kb   *ui.Keyboard
	}
	w.wifiUp = false
	w.reach, w.auth, w.registered = true, true, true
	shots := []shot{
		{"01-welcome.png", stepWelcome, nil},
		{"02-wifi-down.png", stepWifi, nil},
		{"04-server.png", stepServer, &ui.Keyboard{Prompt: "Enter your RomM server address:", Text: "https://romm.lodor.io"}},
		{"05-code.png", stepCode, &ui.Keyboard{Prompt: "Enter your RomM pairing code:", Text: "X7K2", Hint: pairCodeHint}},
		{"06-device.png", stepDevice, &ui.Keyboard{Prompt: "Name this device:", Text: "RG34XX"}},
		{"07-validate.png", stepValidate, nil},
		{"09-done.png", stepDone, nil},
	}
	for _, sh := range shots {
		c := w.render(sh.s, sh.kb)
		if err := c.SavePNG(filepath.Join(dir, sh.name)); err != nil {
			fmt.Fprintln(os.Stderr, "capture:", err)
			os.Exit(1)
		}
	}
	// Wi-Fi-up + progress variants.
	w.wifiUp = true
	_ = w.render(stepWifi, nil).SavePNG(filepath.Join(dir, "03-wifi-up.png"))
	pc := ui.NewCanvas(W, H)
	w.t.Progress(pc, "Building your library...", "Mirroring Sega Game Gear", 42)
	_ = pc.SavePNG(filepath.Join(dir, "08-mirror.png"))
	// Main management menu (the re-runnable parity surface once configured). Rendered from
	// the SAME buildMenuRows spine the interactive loop uses, with representative state so
	// the conditional rows (pending/queue counts, Tailscale) all appear.
	mc := ui.NewCanvas(W, H)
	mx, my, mw, mh := w.t.Frame(mc, menuTitle(), "Up/Down: move   A: select   B: exit")
	rows := buildMenuRows(menuState{pendingN: 2, queueN: 3, userLabel: "Default", tsAvail: true})
	labels := make([]string, len(rows))
	for i, rw := range rows {
		labels[i] = rw.label
	}
	menu := &ui.ScrollMenu{Items: labels}
	menu.Draw(mc, w.t, mx, my, mw, mh)
	_ = mc.SavePNG(filepath.Join(dir, "10-menu.png"))
	// Game Manager per-game action screen (scrolls; text details, no cover thumbnail v1).
	gc := ui.NewCanvas(W, H)
	gx, gy, gw, gh := w.t.Frame(gc, "Sonic The Hedgehog (USA).md", "Up/Down: move   A: select   B: back")
	gm := &ui.ScrollMenu{Items: []string{"Delete from card", "Sync save now", "Server saves", "Details"}}
	gm.Draw(gc, w.t, gx, gy, gw, gh)
	_ = gc.SavePNG(filepath.Join(dir, "13-gamemanager.png"))
	// Connect choice (Tailscale vs plain URL) + the Tailscale sign-in screen (login URL as text).
	cc := ui.NewCanvas(W, H)
	cx, cy, cw, chh := w.t.Frame(cc, "Lodor Setup", "Up/Down: move   A: select   B: back")
	cc.DrawText(cx, cy, "Where is your RomM server?", w.t.Text, w.t.BodyScale)
	cm := &ui.Menu{Items: []string{"Connect via Tailscale", "Home network / public URL"}}
	cm.Draw(cc, w.t, cx, cy+50, cw, chh-50)
	_ = cc.SavePNG(filepath.Join(dir, "11-connect.png"))
	w.tsURL = "https://login.tailscale.com/a/1a2b3c4d5e6f"
	w.tsPhase = "Open this link in a browser and approve this device. Waiting for sign-in... (B to cancel)"
	_ = w.render(stepTsSignin, nil).SavePNG(filepath.Join(dir, "12-tailscale.png"))
	// Certificate-trust offer (lodor#35) — the self-signed-server escape hatch.
	_ = w.renderCertTrust(&ui.Menu{Items: []string{certTrustOption, "Go back"}}).SavePNG(filepath.Join(dir, "14-certtrust.png"))
	fmt.Println("captured wizard screens to", dir)
}
