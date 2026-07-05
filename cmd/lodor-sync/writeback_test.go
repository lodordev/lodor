package main

// Unit tests for the write-back CLI arg parsing (task #167) — the key=val → RomUserData
// mapping and the bounded/bool parsers that decide badarg vs a real write. Wire behavior
// itself is proven by the client-layer mock tests (romm/props_test.go).

import (
	"encoding/json"
	"testing"

	"lodor/romm"
)

func TestParsePropArgsExcludeUnset(t *testing.T) {
	d, err := parsePropArgs([]string{"rating=8", "status=finished", "backlogged=true"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// only the three keys must serialize (exclude_unset).
	raw, _ := json.Marshal(d)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if len(m) != 3 || m["rating"] == nil || m["status"] != "finished" || m["backlogged"] != true {
		t.Fatalf("unexpected body %s", raw)
	}
}

func TestParsePropArgsStatusClear(t *testing.T) {
	d, err := parsePropArgs([]string{"status=clear"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, _ := json.Marshal(d)
	if string(raw) != `{"status":null}` {
		t.Fatalf("status=clear should marshal to null status, got %s", raw)
	}
}

func TestParsePropArgsRejects(t *testing.T) {
	for _, bad := range [][]string{
		{"rating=11"},          // out of range
		{"completion=101"},     // out of range
		{"difficulty=-1"},      // out of range
		{"status=bogus"},       // bad enum
		{"backlogged=maybe"},   // bad bool
		{"unknownkey=1"},       // unknown key
		{"noequalsign"},        // malformed pair
	} {
		if _, err := parsePropArgs(bad); err == nil {
			t.Errorf("parsePropArgs(%v) should error", bad)
		}
	}
}

func TestParsePropArgsClears(t *testing.T) {
	// rating=0 is a real clear (non-nullable) — must be sent, not omitted.
	d, err := parsePropArgs([]string{"rating=0"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.IsEmpty() {
		t.Fatalf("rating=0 must not be empty (0 clears)")
	}
	raw, _ := json.Marshal(d)
	if string(raw) != `{"rating":0}` {
		t.Fatalf("rating=0 should marshal to rating:0, got %s", raw)
	}
	_ = romm.RomUserData(d) // type sanity
}

func TestParseBoundedAndBool(t *testing.T) {
	if _, err := parseBounded("5", 0, 10); err != nil {
		t.Fatalf("5 in [0,10] should parse")
	}
	if _, err := parseBounded("x", 0, 10); err == nil {
		t.Fatalf("non-numeric should error")
	}
	for _, v := range []string{"1", "true", "on", "YES"} {
		if b, err := parseBool(v); err != nil || !b {
			t.Fatalf("%q should be true", v)
		}
	}
	for _, v := range []string{"0", "false", "off", "NO"} {
		if b, err := parseBool(v); err != nil || b {
			t.Fatalf("%q should be false", v)
		}
	}
}
