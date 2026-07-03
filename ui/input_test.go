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

func TestScriptedSource(t *testing.T) {
	s := NewScriptedSource([]Button{BtnDown, BtnConfirm})
	if got := <-s.Buttons(); got != BtnDown {
		t.Fatalf("first = %v", got)
	}
	if got := <-s.Buttons(); got != BtnConfirm {
		t.Fatalf("second = %v", got)
	}
}
