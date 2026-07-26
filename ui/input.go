package ui

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Button is a logical UI action, decoded from raw evdev codes so the wizard never deals
// with hardware key numbers directly.
type Button int

const (
	BtnNone Button = iota
	BtnUp
	BtnDown
	BtnLeft
	BtnRight
	BtnConfirm // A / Enter
	BtnBack    // B / Esc
	BtnStart   // Start (commit/next)
	BtnSelect  // Select
	BtnL1      // left shoulder (launch card: action-row move)
	BtnR1      // right shoulder (launch card: action-row move)
)

func (b Button) String() string {
	switch b {
	case BtnUp:
		return "Up"
	case BtnDown:
		return "Down"
	case BtnLeft:
		return "Left"
	case BtnRight:
		return "Right"
	case BtnConfirm:
		return "Confirm"
	case BtnBack:
		return "Back"
	case BtnStart:
		return "Start"
	case BtnSelect:
		return "Select"
	case BtnL1:
		return "L1"
	case BtnR1:
		return "R1"
	}
	return "None"
}

// InputSource yields logical button presses. EvdevSource reads real hardware;
// ScriptedSource replays a fixed sequence for off-hardware tests.
type InputSource interface {
	Buttons() <-chan Button
	Close() error
}

// evdev event types/codes (Linux input.h subset).
const (
	evKEY = 0x01
	evABS = 0x03

	absHAT0X = 0x10
	absHAT0Y = 0x11
)

// Keymap maps EV_KEY codes to logical buttons. It is selected PER-PLATFORM at EvdevSource
// creation (see NewEvdevSource*), because the physical A/B (and shoulder/start) evdev codes
// are NOT the same across our device lanes:
//
//   - Miyoo (miyoomini/my282/my355 stock, LodorOS) is Xbox/SDL layout: the south face
//     button (BTN_SOUTH/304) is physical A/Confirm, east (BTN_EAST/305) is B/Back. That is
//     defaultKeyMap — UNCHANGED from the historical single global map, so every existing
//     Miyoo device is byte-identical in behavior.
//
//   - H700 (Anbernic RG35xx/RG40xx/RG34xx family, muos + knulli) is NINTENDO layout and
//     SWAPS A/B: physical A/Confirm emits 305 (BTN_EAST), physical B/Back emits 304
//     (BTN_SOUTH) — SAME swap as the TrimUI [certain, sun50i-h700 DTS]. Its Start/Select/
//     shoulders are the STANDARD BTN_ codes (315/314/310/311), NOT the TrimUI's block.
//     That is h700KeyMap.
//
//   - tg5040/tg5050 (TrimUI Smart Pro / Brick, the NextUI/SDL lane) is NINTENDO layout and
//     SWAPS them: physical A/Confirm emits 305 (BTN_EAST), physical B/Back emits 304
//     (BTN_SOUTH). Its shoulders/start/select codes differ too. That is nextuiKeyMap.
//
// Both maps keep the KEY_* keyboard codes (Enter/Space->Confirm, Esc/Backspace->Back) as a
// gptokeyb safety net, and the D-pad is EV_ABS ABS_HAT0X/Y for all lanes (handled in
// decodeEvent, not here).
type Keymap map[uint16]Button

// defaultKeyMap — Xbox/SDL layout. Miyoo + H700. UNCHANGED from the pre-split global map.
var defaultKeyMap = Keymap{
	103: BtnUp, 108: BtnDown, 105: BtnLeft, 106: BtnRight, // KEY_UP/DOWN/LEFT/RIGHT
	544: BtnUp, 545: BtnDown, 546: BtnLeft, 547: BtnRight, // BTN_DPAD_*
	28: BtnConfirm, 57: BtnConfirm, // KEY_ENTER, KEY_SPACE
	1: BtnBack, 14: BtnBack, // KEY_ESC, KEY_BACKSPACE
	304: BtnConfirm, // BTN_SOUTH (A)
	305: BtnBack,    // BTN_EAST  (B)
	315: BtnStart,   // BTN_START
	314: BtnSelect,  // BTN_SELECT
	310: BtnL1,      // BTN_TL  (left shoulder)
	311: BtnR1,      // BTN_TR  (right shoulder)
	// Miyoo Mini / Mini Plus (OnionOS) stock keyboard-style input driver. HARDWARE-DEFERRED:
	// confirm the A/B assignment on the Mini Plus on-device. (Parity with the vendored
	// onion menu copy of this package.)
	29: BtnBack,   // KEY_LEFTCTRL  -> Mini B
	97: BtnSelect, // KEY_RIGHTCTRL -> Mini Select
}

// h700KeyMap — Anbernic H700 handhelds (RG35xx/RG40xx/RG34xx family) on the muOS + Knulli
// lanes. HARDWARE-CORRECTED 2026-07-21 on the first RG34XX-H on-device test: the previous map
// came from the MAINLINE sun50i-h700 DTS + MinUI platform.h and assumed the TrimUI-style A/B
// swap — but muOS ships the vendor 4.9 BSP kernel, whose gpio-keys emit the SDL-standard A/B.
// Triple-sourced: RG34XX-H wizard-input.log raw evdev backstop (labeled A emitted 304, decoded
// Back, wizard exited — the exact bug this map now fixes), plus muOS's own
// /opt/muos/device/config/input/code/button table in BOTH the RG34XX-H and RG40XX-V 2601.1
// images: a:304 b:305 l1:308 r1:309 select:310 start:311 [certain].
//
// So vs the old assumption: A/B are NOT swapped (304=Confirm/305=Back, same as defaultKeyMap),
// and Start/Select/shoulders use the TrimUI-STYLE 308-311 block, NOT the standard BTN_ codes
// (315/314 are real R2/L2 on this hardware and must stay unmapped). D-pad is BTN_DPAD_*
// (544-547) plus the EV_ABS HAT path (map-independent, in decodeEvent). KEY_* keyboard codes
// kept as a gptokeyb safety net, identical to the other maps. Knulli rides the same vendor BSP
// family; its raw-log backstop re-confirms on its first hardware test.
var h700KeyMap = Keymap{
	103: BtnUp, 108: BtnDown, 105: BtnLeft, 106: BtnRight, // KEY_UP/DOWN/LEFT/RIGHT (kbd safety)
	544: BtnUp, 545: BtnDown, 546: BtnLeft, 547: BtnRight, // BTN_DPAD_* [certain]
	28: BtnConfirm, 57: BtnConfirm, // KEY_ENTER, KEY_SPACE (safety net)
	1: BtnBack, 14: BtnBack, // KEY_ESC, KEY_BACKSPACE  (safety net)
	304: BtnConfirm, // physical A / Confirm [certain: RG34XX-H hw log + muOS device config]
	305: BtnBack,    // physical B / Back    [certain: muOS device config]
	311: BtnStart,   // START  [certain: muOS device config]
	310: BtnSelect,  // SELECT [certain: muOS device config]
	308: BtnL1,      // L1     [certain: muOS device config]
	309: BtnR1,      // R1     [certain: muOS device config]
}

// nextuiKeyMap — TrimUI Smart Pro / Brick (tg5040/tg5050), NextUI/SDL lane. Nintendo layout:
// A/B are SWAPPED vs SDL, and the shoulder/start/select codes differ. A/B are [certain]
// (MinUI/NextUI source, multi-source confirmed); 306-311 are [believe] (derived from SDL
// ascending-index order) — the spike's raw evdev log (NewEvdevSourceLogged) still captures
// the real codes on-device so 306-311 can be re-confirmed by evtest later.
var nextuiKeyMap = Keymap{
	103: BtnUp, 108: BtnDown, 105: BtnLeft, 106: BtnRight, // KEY_UP/DOWN/LEFT/RIGHT (kbd safety)
	544: BtnUp, 545: BtnDown, 546: BtnLeft, 547: BtnRight, // BTN_DPAD_*
	28: BtnConfirm, 57: BtnConfirm, // KEY_ENTER, KEY_SPACE (safety net)
	1: BtnBack, 14: BtnBack, // KEY_ESC, KEY_BACKSPACE  (safety net)
	305: BtnConfirm, // BTN_EAST  (physical A / Confirm) [certain]
	304: BtnBack,    // BTN_SOUTH (physical B / Back)    [certain]
	311: BtnStart,   // START  [believe]
	310: BtnSelect,  // SELECT [believe]
	308: BtnL1,      // L1     [believe]
	309: BtnR1,      // R1     [believe]
	// 306 (Y) / 307 (X) [believe] left unmapped — no logical Button consumes them yet.
}

// ScriptedSource replays queued buttons - the off-hardware test input.
type ScriptedSource struct {
	ch chan Button
}

// NewScriptedSource buffers seq and closes after delivering them (then Buttons() blocks).
func NewScriptedSource(seq []Button) *ScriptedSource {
	ch := make(chan Button, len(seq)+1)
	for _, b := range seq {
		ch <- b
	}
	return &ScriptedSource{ch: ch}
}
func (s *ScriptedSource) Buttons() <-chan Button { return s.ch }
func (s *ScriptedSource) Close() error           { close(s.ch); return nil }

// EvdevSource reads every /dev/input/event* device and emits decoded buttons.
type EvdevSource struct {
	ch     chan Button
	files  []*os.File
	closed chan struct{}
	once   sync.Once
	rawLog io.Writer // if set, every input_event is logged ("#evdev ...") — the spike log
	km     Keymap    // per-platform EV_KEY -> Button table (default vs tg5040/NextUI)
}

// NewEvdevSource opens all event devices with the DEFAULT (Xbox/SDL) Keymap — Miyoo/H700.
// Devices that can't be opened are skipped (an app may not have permission to every node);
// as long as the gamepad node opens, input works. Returns an error only if NO device could
// be opened. Use NewEvdevSourceFor to select the tg5040/NextUI (Nintendo-layout) map.
func NewEvdevSource() (*EvdevSource, error) { return NewEvdevSourceLogged(nil) }

// NewEvdevSourceFor is NewEvdevSource with an explicit Keymap — the per-platform selector.
// Pass NextUIKeyMap() on the tg5040/tg5050 (NextUI/SDL) lane, DefaultKeyMap() everywhere else.
func NewEvdevSourceFor(km Keymap) (*EvdevSource, error) { return newEvdev(nil, km) }

// NewEvdevSourceLogged is NewEvdevSource with an optional raw event log (DEFAULT Keymap).
// When rawLog is non-nil, EVERY input_event record is written as a "#evdev path=.. type=..
// code=.. val=.. name=<btn>" line — the SDL spike uses this so the flash records the device's
// REAL evdev codes (SDL joystick input is dead on the TrimUI; evdev is the proven path, so
// the spike must log evdev, not SDL, codes).
func NewEvdevSourceLogged(rawLog io.Writer) (*EvdevSource, error) {
	return newEvdev(rawLog, defaultKeyMap)
}

// NewEvdevSourceLoggedFor is NewEvdevSourceLogged with an explicit per-platform Keymap. The
// SDL spike uses this so the tg5040 raw-log run decodes names against the tg5040 map while
// STILL logging the true numeric codes for later evtest confirmation of the [believe] codes.
func NewEvdevSourceLoggedFor(rawLog io.Writer, km Keymap) (*EvdevSource, error) {
	return newEvdev(rawLog, km)
}

// DefaultKeyMap / H700KeyMap / NextUIKeyMap expose the three platform tables to the wizard's
// lane selector (the ui package owns the codes; the wizard owns which lane it is on).
func DefaultKeyMap() Keymap { return defaultKeyMap }
func H700KeyMap() Keymap    { return h700KeyMap }
func NextUIKeyMap() Keymap  { return nextuiKeyMap }

func newEvdev(rawLog io.Writer, km Keymap) (*EvdevSource, error) {
	if km == nil {
		km = defaultKeyMap
	}
	paths, _ := filepath.Glob("/dev/input/event*")
	s := &EvdevSource{ch: make(chan Button, 16), closed: make(chan struct{}), rawLog: rawLog, km: km}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		s.files = append(s.files, f)
		go s.read(f, p)
	}
	if len(s.files) == 0 {
		return nil, os.ErrNotExist
	}
	return s, nil
}

// decodeEvent maps one 24-byte input_event record (64-bit timeval) to a logical Button using
// the DEFAULT (Xbox/SDL) Keymap. Backward-compatible convenience for callers/tests that don't
// carry a platform map; the live reader uses decodeEventWith(buf, s.km).
func decodeEvent(buf []byte) Button { return decodeEventWith(buf, defaultKeyMap) }

// decodeEventWith maps one 24-byte input_event record (64-bit timeval) to a logical Button
// against the given per-platform Keymap, or BtnNone if it's not a button-down we care about.
// The D-pad (EV_ABS ABS_HAT0X/Y) is Keymap-independent — all lanes share it. Pure + testable.
func decodeEventWith(buf []byte, km Keymap) Button {
	if len(buf) < 24 {
		return BtnNone
	}
	le := binary.LittleEndian
	typ := le.Uint16(buf[16:])
	code := le.Uint16(buf[18:])
	val := int32(le.Uint32(buf[20:]))
	switch typ {
	case evKEY:
		if val != 1 && val != 2 { // press or autorepeat only (ignore release)
			return BtnNone
		}
		return km[code]
	case evABS:
		switch code {
		case absHAT0X:
			if val < 0 {
				return BtnLeft
			} else if val > 0 {
				return BtnRight
			}
		case absHAT0Y:
			if val < 0 {
				return BtnUp
			} else if val > 0 {
				return BtnDown
			}
		}
	}
	return BtnNone
}

// evdevLogLine formats one input_event record as a spike-log line ("#evdev ..."), or ""
// for events not worth logging (EV_SYN/EV_MSC/analog noise). Only key presses and D-pad hat
// motion are logged so the flash captures a readable button-code map of the REAL device
// (SDL never saw these codes; evdev is the proven path). Pure + testable.
func evdevLogLine(path string, buf []byte, decoded Button) string {
	if len(buf) < 24 {
		return ""
	}
	le := binary.LittleEndian
	typ := le.Uint16(buf[16:])
	code := le.Uint16(buf[18:])
	val := int32(le.Uint32(buf[20:]))
	if typ != evKEY && !(typ == evABS && (code == absHAT0X || code == absHAT0Y)) {
		return ""
	}
	return fmt.Sprintf("#evdev path=%s type=%d code=%d val=%d name=%s\n",
		path, typ, code, val, decoded.String())
}

// read decodes input_event records from one device and forwards logical buttons.
func (s *EvdevSource) read(f *os.File, path string) {
	buf := make([]byte, 24)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		if n < 24 {
			continue
		}
		b := decodeEventWith(buf, s.km)
		if s.rawLog != nil {
			if line := evdevLogLine(path, buf, b); line != "" {
				io.WriteString(s.rawLog, line)
			}
		}
		if b == BtnNone {
			continue
		}
		select {
		case s.ch <- b:
		case <-s.closed:
			return
		}
	}
}

func (s *EvdevSource) Buttons() <-chan Button { return s.ch }

// Count is the number of input devices successfully opened (for the startup phase log).
func (s *EvdevSource) Count() int { return len(s.files) }

func (s *EvdevSource) Close() error {
	s.once.Do(func() {
		close(s.closed)
		for _, f := range s.files {
			_ = f.Close()
		}
	})
	return nil
}
