package romm

import "testing"

// TestBuildURL locks path composition against a base that may carry a subpath. The
// API path (e.g. /api/heartbeat) must be APPENDED to the full base path, never
// truncate or replace the subpath — a subpath RomM (https://example.com/romm) must
// resolve /api/heartbeat to https://example.com/romm/api/heartbeat. Also covers the
// port+subpath base (the shape config.URL() now emits) and space escaping in the
// appended path.
func TestBuildURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		path string
		want string
	}{
		{
			name: "no subpath",
			base: "https://example.com",
			path: "/api/heartbeat",
			want: "https://example.com/api/heartbeat",
		},
		{
			name: "subpath appended (not truncated)",
			base: "https://example.com/romm",
			path: "/api/heartbeat",
			want: "https://example.com/romm/api/heartbeat",
		},
		{
			name: "subpath with port appended",
			base: "https://example.com:8443/romm",
			path: "/api/heartbeat",
			want: "https://example.com:8443/romm/api/heartbeat",
		},
		{
			name: "no-port subpath, content path with space",
			base: "https://example.com/romm",
			path: "/api/roms/5/content/Super Mario.sfc",
			want: "https://example.com/romm/api/roms/5/content/Super%20Mario.sfc",
		},
		{
			name: "path with query preserved",
			base: "https://example.com/romm",
			path: "/api/saves?rom_id=5",
			want: "https://example.com/romm/api/saves?rom_id=5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildURL(tt.base, tt.path)
			if err != nil {
				t.Fatalf("buildURL(%q, %q) error: %v", tt.base, tt.path, err)
			}
			if got != tt.want {
				t.Errorf("buildURL(%q, %q) = %q, want %q", tt.base, tt.path, got, tt.want)
			}
		})
	}
}
