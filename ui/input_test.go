package ui

import (
	"encoding/binary"
	"testing"
)

func evt(typ, code uint16, val int32) []byte {
	b := make([]byte, 24) // 16-byte timeval ignored
	le := binary.LittleEndian
	le.PutUint16(b[16:], typ)
	le.PutUint16(b[18:], code)
	le.PutUint32(b[20:], uint32(val))
	return b
}

func TestDecodeEvent(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want Button
	}{
		{"south press = confirm", evt(evKEY, 304, 1), BtnConfirm},
		{"east press = back", evt(evKEY, 305, 1), BtnBack},
		{"south release ignored", evt(evKEY, 304, 0), BtnNone},
		{"south autorepeat = confirm", evt(evKEY, 304, 2), BtnConfirm},
		{"key up", evt(evKEY, 103, 1), BtnUp},
		{"key enter = confirm", evt(evKEY, 28, 1), BtnConfirm},
		{"hat x left", evt(evABS, absHAT0X, -1), BtnLeft},
		{"hat x right", evt(evABS, absHAT0X, 1), BtnRight},
		{"hat x center", evt(evABS, absHAT0X, 0), BtnNone},
		{"hat y down", evt(evABS, absHAT0Y, 1), BtnDown},
		{"unknown key", evt(evKEY, 9999, 1), BtnNone},
		{"short buffer", make([]byte, 10), BtnNone},
	}
	for _, c := range cases {
		if got := decodeEvent(c.buf); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestKeymapSplit proves the per-platform keymap split across all THREE lane maps
// (launch-card-v2 fleet rollout). The two Nintendo-layout maps (tg5040/NextUI and H700
// muos/knulli) SWAP A/B; the DEFAULT (Xbox/SDL, Miyoo/LodorOS) map keeps A=304. The H700
// map differs from NextUI in its Start/Select/shoulder codes (standard BTN_ vs the TrimUI
// block). The D-pad (EV_ABS) and the KEY_* keyboard safety net stay identical across all maps.
func TestKeymapSplit(t *testing.T) {
	cases := []struct {
		name string
		km   Keymap
		buf  []byte
		want Button
	}{
		// tg5040/NextUI: A/B swapped vs SDL. [certain]
		{"nextui 305(A)=confirm", NextUIKeyMap(), evt(evKEY, 305, 1), BtnConfirm},
		{"nextui 304(B)=back", NextUIKeyMap(), evt(evKEY, 304, 1), BtnBack},
		{"nextui 311=start", NextUIKeyMap(), evt(evKEY, 311, 1), BtnStart},
		{"nextui 310=select", NextUIKeyMap(), evt(evKEY, 310, 1), BtnSelect},
		{"nextui 308=L1", NextUIKeyMap(), evt(evKEY, 308, 1), BtnL1},
		{"nextui 309=R1", NextUIKeyMap(), evt(evKEY, 309, 1), BtnR1},
		// tg5040 keyboard safety net survives.
		{"nextui enter=confirm", NextUIKeyMap(), evt(evKEY, 28, 1), BtnConfirm},
		{"nextui esc=back", NextUIKeyMap(), evt(evKEY, 1, 1), BtnBack},
		// tg5040 D-pad unchanged (map-independent EV_ABS path).
		{"nextui hat up", NextUIKeyMap(), evt(evABS, absHAT0Y, -1), BtnUp},

		// H700 (muos/knulli): hardware-corrected 2026-07-21 (RG34XX-H) — A/B are NOT swapped
		// (304=A/305=B, vendor BSP kernel) and start/select/shoulders are the TrimUI-STYLE
		// block. And BTN_DPAD_* (544) is Up. These three assertions are the task's required proof.
		{"h700 304(A)=confirm", H700KeyMap(), evt(evKEY, 304, 1), BtnConfirm},
		{"h700 305(B)=back", H700KeyMap(), evt(evKEY, 305, 1), BtnBack},
		{"h700 544(dpad)=up", H700KeyMap(), evt(evKEY, 544, 1), BtnUp},
		{"h700 311=start", H700KeyMap(), evt(evKEY, 311, 1), BtnStart},
		{"h700 310=select", H700KeyMap(), evt(evKEY, 310, 1), BtnSelect},
		{"h700 308=L1", H700KeyMap(), evt(evKEY, 308, 1), BtnL1},
		{"h700 309=R1", H700KeyMap(), evt(evKEY, 309, 1), BtnR1},
		// H700 must NOT wear the standard BTN_ start/select block: 315/314 are real R2/L2 on
		// this hardware and stay dead (proves the hw-corrected map dropped the old codes).
		{"h700 315 unmapped (real R2)", H700KeyMap(), evt(evKEY, 315, 1), BtnNone},
		{"h700 314 unmapped (real L2)", H700KeyMap(), evt(evKEY, 314, 1), BtnNone},
		// H700 keyboard safety net survives.
		{"h700 enter=confirm", H700KeyMap(), evt(evKEY, 28, 1), BtnConfirm},
		{"h700 esc=back", H700KeyMap(), evt(evKEY, 1, 1), BtnBack},

		// DEFAULT (Miyoo/LodorOS miyoomini + my355) is UNCHANGED: 304 is still Confirm here —
		// proves the split. Plus the miyoomini KEYBOARD codes the task requires verified:
		// 57 (KEY_SPACE)=Confirm, 29 (KEY_LEFTCTRL)=Back, arrows=dpad.
		{"default 304(A)=confirm", DefaultKeyMap(), evt(evKEY, 304, 1), BtnConfirm},
		{"default 305(B)=back", DefaultKeyMap(), evt(evKEY, 305, 1), BtnBack},
		{"default 315=start", DefaultKeyMap(), evt(evKEY, 315, 1), BtnStart},
		{"default 310=L1", DefaultKeyMap(), evt(evKEY, 310, 1), BtnL1},
		{"default 57(space)=confirm", DefaultKeyMap(), evt(evKEY, 57, 1), BtnConfirm},
		{"default 29(leftctrl)=back", DefaultKeyMap(), evt(evKEY, 29, 1), BtnBack},
		{"default 103(up-arrow)=up", DefaultKeyMap(), evt(evKEY, 103, 1), BtnUp},
		{"default 108(down-arrow)=down", DefaultKeyMap(), evt(evKEY, 108, 1), BtnDown},
	}
	for _, c := range cases {
		if got := decodeEventWith(c.buf, c.km); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestScriptedSource(t *testing.T) {
	s := NewScriptedSource([]Button{BtnDown, BtnConfirm})
	if got := <-s.Buttons(); got != BtnDown {
		t.Fatalf("first = %v", got)
	}
	if got := <-s.Buttons(); got != BtnConfirm {
		t.Fatalf("second = %v", got)
	}
}

func TestEvdevLogLine(t *testing.T) {
	// BTN_SOUTH (304) press => logged, decoded name Confirm.
	got := evdevLogLine("/dev/input/event2", evt(evKEY, 304, 1), BtnConfirm)
	want := "#evdev path=/dev/input/event2 type=1 code=304 val=1 name=Confirm\n"
	if got != want {
		t.Errorf("key line = %q, want %q", got, want)
	}
	// D-pad hat motion is logged.
	if l := evdevLogLine("/dev/input/event0", evt(evABS, absHAT0Y, -1), BtnUp); l == "" {
		t.Error("hat motion should be logged")
	}
	// EV_SYN / EV_MSC / analog noise is NOT logged (keeps the spike map readable).
	if l := evdevLogLine("/dev/input/event0", evt(0x00, 0, 0), BtnNone); l != "" {
		t.Errorf("EV_SYN should not log, got %q", l)
	}
	if l := evdevLogLine("/dev/input/event0", evt(evABS, 0x00 /*ABS_X analog*/, 128), BtnNone); l != "" {
		t.Errorf("analog stick should not log, got %q", l)
	}
	// short buffer => no line, no panic.
	if l := evdevLogLine("/dev/input/event0", make([]byte, 10), BtnNone); l != "" {
		t.Errorf("short buf should not log, got %q", l)
	}
}
