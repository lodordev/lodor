package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The overlay must never invent numbers it does not have: no size before the engine has
// published bytes, and no rate computed over a sub-second window. Elapsed is the one
// value that always appears once a second has passed, because it is what proves to the
// user that the screen is live rather than frozen.
func TestTransferStatusHonesty(t *testing.T) {
	cases := []struct {
		name    string
		pct     int
		done    int64
		total   int64
		elapsed time.Duration
		want    string
	}{
		{"nothing known yet", -1, 0, 0, 0, "starting..."},
		{"percent only, no bytes", 12, 0, 0, 0, "12%"},
		{"no rate under one second", 12, 1 << 20, 16 << 20, 500 * time.Millisecond,
			"12%  -  1.0 MB / 16.0 MB"},
		{"full line once a second has passed", 42, 7 << 20, 16 << 20, 34 * time.Second,
			"42%  -  7.0 MB / 16.0 MB  -  211 KB/s  -  34s"},
		{"unknown total still shows what moved", 0, 3 << 20, 0, 10 * time.Second,
			"0%  -  3.0 MB  -  307 KB/s  -  10s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := transferStatus(c.pct, c.done, c.total, c.elapsed); got != c.want {
				t.Errorf("transferStatus = %q, want %q", got, c.want)
			}
		})
	}
}

// A zero-second window must not divide by zero, and must not report an infinite rate.
func TestTransferStatusNoDivideByZero(t *testing.T) {
	got := transferStatus(50, 8<<20, 16<<20, 0)
	if got == "" {
		t.Fatal("empty status")
	}
	if strings.Contains(got, "/s") {
		t.Errorf("rate reported over a zero-length window: %q", got)
	}
}

// readBytes must degrade to 0,0 on anything malformed rather than surfacing garbage —
// the caller renders percent-only in that case, which is honest.
func TestReadBytesParsesAndDegrades(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LODOR_PROGRESS_DIR", dir)
	p := filepath.Join(dir, "dl-bytes")

	if d, tot := readBytes(); d != 0 || tot != 0 {
		t.Errorf("absent file = %d/%d, want 0/0", d, tot)
	}
	for _, bad := range []string{"", "garbage", "12", "12/", "/34", "-1/34", "a/b"} {
		if err := os.WriteFile(p, []byte(bad+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if d, tot := readBytes(); d != 0 || tot != 0 {
			t.Errorf("malformed %q = %d/%d, want 0/0", bad, d, tot)
		}
	}
	if err := os.WriteFile(p, []byte("7340032/16777216\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, tot := readBytes()
	if d != 7340032 || tot != 16777216 {
		t.Errorf("readBytes = %d/%d, want 7340032/16777216", d, tot)
	}
}

func TestHumanBytesScales(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{2048, "2 KB"},
		{16 << 20, "16.0 MB"},
		{3 << 30, "3.0 GB"},
	} {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
