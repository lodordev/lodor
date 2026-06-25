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
