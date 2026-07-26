package main

import (
	"testing"

	"lodor/update"
)

// Freeze contract (pivot 2026-07-15): a lane whose per-asset key vanished from
// the channel is frozen -- check-update must treat it as no-update, while
// legacy callers (no asset key) and live lanes keep the version compare.
func TestAssetPresent(t *testing.T) {
	ch := &update.Channel{
		Version: "0.9.9",
		Assets:  map[string]update.Asset{"muos": {}, "knulli": {}},
	}
	cases := []struct {
		key  string
		want bool
	}{
		{"", true},                   // legacy caller: old behavior
		{"muos", true},               // live lane
		{"lodoros-miyoomini", false}, // frozen lane: asset gone
	}
	for _, c := range cases {
		if got := assetPresent(ch, c.key); got != c.want {
			t.Errorf("assetPresent(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}
