package config

import "testing"

// TestHostURL locks the port/subpath composition: the port must land on the
// authority (host), never on the tail of a subpath. Covers the four cases the
// onboarding subpath work cares about plus the legacy no-subpath paths, which must
// round-trip byte-identically to the pre-fix behavior.
func TestHostURL(t *testing.T) {
	tests := []struct {
		name    string
		rootURI string
		port    int
		want    string
	}{
		// --- the subpath cases (the priority fix) ---
		{
			name:    "subpath no port",
			rootURI: "https://example.com/romm",
			port:    0,
			want:    "https://example.com/romm",
		},
		{
			name:    "subpath with port (the malformed-before case)",
			rootURI: "https://example.com/romm",
			port:    8443,
			want:    "https://example.com:8443/romm",
		},
		{
			name:    "deep subpath with port",
			rootURI: "http://my-romm.example.com/apps/romm",
			port:    8080,
			want:    "http://my-romm.example.com:8080/apps/romm",
		},
		{
			name:    "subpath trailing slash trimmed, no port",
			rootURI: "https://example.com/romm/",
			port:    0,
			want:    "https://example.com/romm",
		},
		{
			name:    "subpath trailing slash trimmed, with port",
			rootURI: "https://example.com/romm/",
			port:    8443,
			want:    "https://example.com:8443/romm",
		},
		// --- legacy no-subpath paths: must be byte-identical to old behavior ---
		{
			name:    "no subpath no port",
			rootURI: "https://example.com",
			port:    0,
			want:    "https://example.com",
		},
		{
			name:    "no subpath with port",
			rootURI: "https://example.com",
			port:    8443,
			want:    "https://example.com:8443",
		},
		{
			name:    "http no subpath with port",
			rootURI: "http://192.168.1.50",
			port:    9000,
			want:    "http://192.168.1.50:9000",
		},
		{
			name:    "no subpath trailing slash with port",
			rootURI: "https://example.com/",
			port:    8443,
			want:    "https://example.com:8443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Host{RootURI: tt.rootURI, Port: tt.port}
			if got := h.URL(); got != tt.want {
				t.Errorf("Host{RootURI:%q, Port:%d}.URL() = %q, want %q",
					tt.rootURI, tt.port, got, tt.want)
			}
		})
	}
}

// TestResolvedMirrorMode locks the coexist-mode resolution: an explicit, recognized
// mirror_mode wins (case/space-insensitive); an absent/unknown value falls back to the
// host default, which is "own" on a LodorOS host (or no host hint — byte-identical to
// today) and "separate" on NextUI / any other host.
func TestResolvedMirrorMode(t *testing.T) {
	cases := []struct {
		name   string
		mirror string
		hostOS string // LODOR_HOST_OS env ("" = unset)
		want   string
	}{
		{"explicit own", "own", "nextui", MirrorModeOwn},
		{"explicit separate", "separate", "lodoros", MirrorModeSeparate},
		{"explicit merge", "merge", "", MirrorModeMerge},
		{"explicit case/space insensitive", "  Separate ", "", MirrorModeSeparate},
		{"unset, no host hint -> own", "", "", MirrorModeOwn},
		{"unset, lodoros -> own", "", "lodoros", MirrorModeOwn},
		{"unset, minui -> own", "", "minui", MirrorModeOwn},
		{"unset, nextui -> separate", "", "nextui", MirrorModeSeparate},
		{"unset, unknown host -> separate", "", "weirdos", MirrorModeSeparate},
		{"unknown value falls back to host default", "garbage", "nextui", MirrorModeSeparate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LODOR_HOST_OS", c.hostOS)
			cfg := &Config{MirrorMode: c.mirror}
			if got := cfg.ResolvedMirrorMode(); got != c.want {
				t.Errorf("ResolvedMirrorMode(mirror=%q host=%q) = %q, want %q",
					c.mirror, c.hostOS, got, c.want)
			}
		})
	}
	// Nil receiver must not panic and must use the host default.
	t.Setenv("LODOR_HOST_OS", "nextui")
	var nilCfg *Config
	if got := nilCfg.ResolvedMirrorMode(); got != MirrorModeSeparate {
		t.Errorf("nil Config ResolvedMirrorMode() = %q, want %q", got, MirrorModeSeparate)
	}
}
