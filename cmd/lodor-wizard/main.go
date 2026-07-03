// Command lodor-wizard is the on-device onboarding UI for Lodor-MuOS. It renders to
// /dev/fb0 and reads /dev/input/event* (pure-Go, CGO-free, stdlib only - no SDL), and
// drives the headless engine (lodor-sync) for the actual RomM work. Wi-Fi entry stays
// muOS's job (Principle 1): the wizard verifies connectivity and points the user at
// muOS Settings if down, owning only the RomM-specific steps.
//
// Modes:
//   (default)         interactive - fb0 + evdev, runs the engine, writes config.json.
//   --capture <dir>   render every screen with representative state to PNG (off-hardware
//                     verification). No fb, no input, no engine calls.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"lodor/ui"
)

const W, H = 720, 480

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
)

type wizard struct {
	t        ui.Theme
	server   string
	code     string
	device   string
	wifiUp   bool
	reach    bool
	auth     bool
	errMsg   string
	mirrorN  string
	bin      string // path to lodor-sync
	dataDir  string
}

func main() {
	args := os.Args[1:]
	w := &wizard{t: ui.DefaultTheme(), device: "RG34XX", server: "https://"}
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
	w.runInteractive()
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
	cmd := exec.Command(w.bin, args...)
	cmd.Dir = w.dataDir
	out, err := cmd.CombinedOutput()
	return string(out), err
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
		t.Message(c, "Welcome", "This sets up Lodor Sync. Your whole library appears in the games menu, each game downloads on first launch, and saves sync automatically around every session.\n\nPress A to begin.", t.Text)
	case stepWifi:
		if w.wifiUp {
			t.Message(c, "Wi-Fi connected", "Your handheld is online. Press A to continue.", t.Good)
		} else {
			t.Message(c, "Connect Wi-Fi first", "Open muOS Settings, Network, and join your Wi-Fi. Then press A to re-check.\n\nPress B to skip - you can sync later from the Lodor Sync app.", t.Bad)
		}
	case stepServer:
		x, y, ww, hh := t.Frame(c, "Lodor Sync Setup", "D-pad: move   A: type   B: delete   Start: OK")
		kb.Draw(c, t, x, y, ww, hh)
	case stepCode:
		x, y, ww, hh := t.Frame(c, "Lodor Sync Setup", "D-pad: move   A: type   B: delete   Start: OK")
		kb.Draw(c, t, x, y, ww, hh)
	case stepDevice:
		x, y, ww, hh := t.Frame(c, "Lodor Sync Setup", "D-pad: move   A: type   B: delete   Start: OK")
		kb.Draw(c, t, x, y, ww, hh)
	case stepValidate:
		body := "Server reachable: " + yn(w.reach) + "\nLogin accepted: " + yn(w.auth)
		col := t.Good
		title := "Connected!"
		if !w.reach || !w.auth {
			col = t.Bad
			title = "Couldn't connect"
		}
		t.Message(c, title, body+"\n\nPress A to continue.", col)
	case stepDone:
		t.Message(c, "All set!", "Your library is now in the games menu as stubs. Pick any game and it downloads on first launch; saves sync around every session.\n\nPress A to exit.", t.Good)
	case stepError:
		t.Message(c, "Setup error", w.errMsg+"\n\nPress A to exit.", t.Bad)
	}
	return c
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// ---- interactive loop -----------------------------------------------------------------

func (w *wizard) runInteractive() {
	fb, err := ui.OpenFramebuffer("/dev/fb0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "wizard: no framebuffer:", err)
		os.Exit(1)
	}
	defer fb.Close()
	in, err := ui.NewEvdevSource()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wizard: no input device:", err)
		os.Exit(1)
	}
	defer in.Close()

	draw := func(c *ui.Canvas) { fb.Flush(c) }
	btn := func() ui.Button { return <-in.Buttons() }

	if w.configured() {
		w.runMainMenu(draw, btn)
		return
	}
	w.runOnboarding(draw, btn)
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

// runMainMenu is the re-runnable entry shown once setup is done.
func (w *wizard) runMainMenu(draw func(*ui.Canvas), btn func() ui.Button) {
	m := &ui.Menu{Items: []string{"Sync now", "Run setup again", "Exit"}}
	for {
		c := ui.NewCanvas(W, H)
		x, y, ww, hh := w.t.Frame(c, "Lodor Sync", "Up/Down: move   A: select")
		c.DrawText(x, y, "Your library is set up. What now?", w.t.Text, w.t.BodyScale)
		m.Draw(c, w.t, x, y+50, ww, hh-50)
		draw(c)
		if !m.Handle(btn()) {
			continue
		}
		switch m.Selected() {
		case 0:
			w.syncNow(draw, btn)
			w.screenMessage(stepDone, draw, btn)
			return
		case 1:
			w.runOnboarding(draw, btn)
			return
		default:
			return
		}
	}
}

// runOnboarding is the first-run flow: welcome -> Wi-Fi -> server -> pair -> name ->
// validate -> initial mirror -> done. Each network step fails honestly.
func (w *wizard) runOnboarding(draw func(*ui.Canvas), btn func() ui.Button) {
	w.screenMessage(stepWelcome, draw, btn)

	// Wi-Fi (loop until up, or skipped with B). Pairing needs the network.
	for {
		w.wifiUp = wifiUp()
		draw(w.render(stepWifi, nil))
		b := btn()
		if w.wifiUp && b == ui.BtnConfirm {
			break
		}
		if !w.wifiUp && b == ui.BtnBack {
			break // skip - user syncs later from the app
		}
	}

	kb := &ui.Keyboard{Prompt: "Enter your RomM server address:", Text: w.server}
	w.screenKeyboard(stepServer, kb, draw, btn)
	w.server = strings.TrimSpace(kb.Text)
	if out, err := w.runEngine("--set-server", w.server); err != nil || !resultFlag(out, "server_set") {
		w.errMsg = "Could not save the server address. Check it and try again."
		w.screenMessage(stepError, draw, btn)
		return
	}

	kb = &ui.Keyboard{Prompt: "Enter your RomM pairing code:", Text: ""}
	w.screenKeyboard(stepCode, kb, draw, btn)
	w.code = strings.TrimSpace(kb.Text)
	if out, err := w.runEngine("--pair", w.code); err != nil || !resultFlag(out, "paired") {
		w.errMsg = "Pairing failed. Generate a fresh code in RomM and try again."
		w.screenMessage(stepError, draw, btn)
		return
	}

	kb = &ui.Keyboard{Prompt: "Name this device:", Text: w.device}
	w.screenKeyboard(stepDevice, kb, draw, btn)
	w.device = strings.TrimSpace(kb.Text)
	_, _ = w.runEngine("--register-device", w.device) // non-fatal; pulls work unregistered

	out, _ := w.runEngine("--validate")
	w.reach = resultFlag(out, "reachable")
	w.auth = resultFlag(out, "auth")
	w.screenMessage(stepValidate, draw, btn)

	// Network is up from pairing; mirror directly then flush any pending saves.
	w.screenMirror(draw)
	_, _ = w.runEngine("--push-pending")

	w.screenMessage(stepDone, draw, btn)
}

// syncNow verifies Wi-Fi (honest: can't sync offline), mirrors with progress, and pushes
// the pending-save queue. Shared by the main menu's "Sync now".
func (w *wizard) syncNow(draw func(*ui.Canvas), btn func() ui.Button) {
	for {
		w.wifiUp = wifiUp()
		if w.wifiUp {
			break
		}
		draw(w.render(stepWifi, nil))
		if btn() == ui.BtnBack {
			return // offline; user chose to skip
		}
	}
	w.screenMirror(draw)
	_, _ = w.runEngine("--push-pending")
}

// screenMessage draws step s and waits for A (Confirm) to advance.
func (w *wizard) screenMessage(s step, draw func(*ui.Canvas), btn func() ui.Button) {
	for {
		draw(w.render(s, nil))
		if btn() == ui.BtnConfirm {
			return
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

// screenMirror runs --mirror-catalog in the background, polling the engine's progress
// side-channels (/tmp/dl-progress, /tmp/romm-phase) and rendering an honest progress bar.
func (w *wizard) screenMirror(draw func(*ui.Canvas)) {
	_ = os.Remove("/tmp/dl-progress")
	cmd := exec.Command(w.bin, "--mirror-catalog")
	cmd.Dir = w.dataDir
	done := make(chan error, 1)
	go func() { done <- runAndWait(cmd) }()
	for {
		select {
		case <-done:
			c := ui.NewCanvas(W, H)
			w.t.Progress(c, "Building your library...", "Done", 100)
			draw(c)
			return
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

// ---- capture mode (off-hardware verification) -----------------------------------------

func (w *wizard) capture(dir string) {
	_ = os.MkdirAll(dir, 0o755)
	type shot struct {
		name string
		s    step
		kb   *ui.Keyboard
	}
	w.wifiUp = false
	w.reach, w.auth = true, true
	shots := []shot{
		{"01-welcome.png", stepWelcome, nil},
		{"02-wifi-down.png", stepWifi, nil},
		{"04-server.png", stepServer, &ui.Keyboard{Prompt: "Enter your RomM server address:", Text: "https://romm.lodor.io"}},
		{"05-code.png", stepCode, &ui.Keyboard{Prompt: "Enter your RomM pairing code:", Text: "X7K2"}},
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
	// Main menu (the re-runnable entry once configured).
	mc := ui.NewCanvas(W, H)
	mx, my, mw, mh := w.t.Frame(mc, "Lodor Sync", "Up/Down: move   A: select")
	mc.DrawText(mx, my, "Your library is set up. What now?", w.t.Text, w.t.BodyScale)
	menu := &ui.Menu{Items: []string{"Sync now", "Run setup again", "Exit"}}
	menu.Draw(mc, w.t, mx, my+50, mw, mh-50)
	_ = mc.SavePNG(filepath.Join(dir, "10-menu.png"))
	fmt.Println("captured wizard screens to", dir)
}
