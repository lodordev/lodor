package romm

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in            string
		maj, min, pat int
	}{
		{"4.9.0", 4, 9, 0},
		{"v5.0.0", 5, 0, 0},
		{"V3.10.2", 3, 10, 2},
		{"3.10.2-beta", 3, 10, 2},
		{"4.9.0+build7", 4, 9, 0},
		{"3.6", 3, 6, 0},
		{"5", 5, 0, 0},
		{"  4.9.0  ", 4, 9, 0},
		{"", 0, 0, 0},
		{"garbage", 0, 0, 0},
	}
	for _, tt := range tests {
		v := ParseVersion(tt.in)
		if v.Major != tt.maj || v.Minor != tt.min || v.Patch != tt.pat {
			t.Errorf("ParseVersion(%q) = %d.%d.%d, want %d.%d.%d", tt.in, v.Major, v.Minor, v.Patch, tt.maj, tt.min, tt.pat)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		v             string
		maj, min, pat int
		want          bool
	}{
		{"4.9.0", 4, 9, 0, true},
		{"4.9.1", 4, 9, 0, true},
		{"4.8.9", 4, 9, 0, false},
		{"5.0.0", 4, 9, 0, true},
		{"4.10.0", 4, 9, 0, true},
		{"3.99.99", 4, 0, 0, false},
		{"0.0.0", 4, 9, 0, false},
		{"4.9.0", 5, 0, 0, false},
	}
	for _, tt := range tests {
		if got := ParseVersion(tt.v).AtLeast(tt.maj, tt.min, tt.pat); got != tt.want {
			t.Errorf("ParseVersion(%q).AtLeast(%d,%d,%d) = %v, want %v", tt.v, tt.maj, tt.min, tt.pat, got, tt.want)
		}
	}
}

func TestCapabilitiesFrom(t *testing.T) {
	tests := []struct {
		ver                                        string
		negotiate, trustHash, deviceAuth, playSess bool
	}{
		{"3.6.0", false, false, false, true},
		{"4.8.0", false, false, false, true},
		{"4.9.0", true, true, false, true},
		{"4.9.5", true, true, false, true},
		{"5.0.0", true, true, true, true},
		{"", false, false, false, false},      // unknown -> all false (legacy)
		{"2.0.0", false, false, false, false}, // pre play-session
	}
	for _, tt := range tests {
		c := CapabilitiesFrom(ParseVersion(tt.ver))
		if c.SupportsSyncNegotiate != tt.negotiate || c.TrustsServerHash != tt.trustHash ||
			c.SupportsDeviceAuth != tt.deviceAuth || c.SupportsPlaySessionIngest != tt.playSess {
			t.Errorf("CapabilitiesFrom(%q) = %+v", tt.ver, c)
		}
	}
}

// TestParseHeartbeatVersion locks the schema-tolerant version search across the known
// (and a future top-level) heartbeat shapes.
func TestParseHeartbeatVersion(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"lowercase nested", `{"system":{"version":"4.9.0"}}`, "4.9.0"},
		{"uppercase nested", `{"SYSTEM":{"VERSION":"5.0.0"}}`, "5.0.0"},
		{"top level", `{"version":"3.6.1"}`, "3.6.1"},
		{"deeper nesting", `{"a":{"b":{"Version":"4.9.2"}}}`, "4.9.2"},
		{"no version", `{"system":{"foo":"bar"}}`, "0.0.0"},
		{"garbage body", `not json`, "0.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHeartbeatVersion([]byte(tt.body)).String()
			if got != tt.want {
				t.Errorf("parseHeartbeatVersion(%s) = %s, want %s", tt.body, got, tt.want)
			}
		})
	}
}
