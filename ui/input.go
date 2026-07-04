package ui

import (
	"encoding/binary"
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

// keyMap maps EV_KEY codes to logical buttons. We map BOTH keyboard keys (in case
// gptokeyb is feeding the app) AND raw gamepad BTN_* codes. NOTE (hardware-deferred): the
// physical A/B assignment on the RG34XX must be confirmed on-device; the south face
// button (BTN_SOUTH/304) is taken as Confirm and east (BTN_EAST/305) as Back by default,
// matching SDL's layout. Override via an input map file if the unit differs.
var keyMap = map[uint16]Button{
	103: BtnUp, 108: BtnDown, 105: BtnLeft, 106: BtnRight, // KEY_UP/DOWN/LEFT/RIGHT
	544: BtnUp, 545: BtnDown, 546: BtnLeft, 547: BtnRight, // BTN_DPAD_*
	28: BtnConfirm, 57: BtnConfirm, // KEY_ENTER, KEY_SPACE
	1: BtnBack, 14: BtnBack, // KEY_ESC, KEY_BACKSPACE
	304: BtnConfirm, // BTN_SOUTH (A)
	305: BtnBack,    // BTN_EAST  (B)
	315: BtnStart,   // BTN_START
	314: BtnSelect,  // BTN_SELECT
	// Miyoo Mini / Mini Plus (OnionOS) stock keyboard-style input driver. HARDWARE-DEFERRED:
	// confirm the A/B assignment on the Mini Plus on-device. (Parity with the vendored
	// onion menu copy of this package.)
	29: BtnBack,   // KEY_LEFTCTRL  -> Mini B
	97: BtnSelect, // KEY_RIGHTCTRL -> Mini Select
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
}

// NewEvdevSource opens all event devices. Devices that can't be opened are skipped (an
// app may not have permission to every node); as long as the gamepad node opens, input
// works. Returns an error only if NO device could be opened.
func NewEvdevSource() (*EvdevSource, error) {
	paths, _ := filepath.Glob("/dev/input/event*")
	s := &EvdevSource{ch: make(chan Button, 16), closed: make(chan struct{})}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		s.files = append(s.files, f)
		go s.read(f)
	}
	if len(s.files) == 0 {
		return nil, os.ErrNotExist
	}
	return s, nil
}

// decodeEvent maps one 24-byte input_event record (64-bit timeval) to a logical Button,
// or BtnNone if it's not a button-down we care about. Pure + testable.
func decodeEvent(buf []byte) Button {
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
		return keyMap[code]
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

// read decodes input_event records from one device and forwards logical buttons.
func (s *EvdevSource) read(f *os.File) {
	buf := make([]byte, 24)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		if n < 24 {
			continue
		}
		b := decodeEvent(buf)
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
