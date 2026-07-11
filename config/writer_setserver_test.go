package config

import (
	"encoding/json"
	"os"
	"testing"
)

// TestSetServerPropagatesToSameServerProfileHosts: --set-server rewrote only
// hosts[0], so multi-user profile hosts (seeded with the SAME server) kept the
// stale endpoint/insecure flag and failed forever after a server move. A server
// update must move every host still carrying the PREVIOUS root_uri — endpoint +
// transport fields in lockstep with hosts[0] — while a host on a DIFFERENT
// root_uri and all per-profile identity fields stay untouched.
func TestSetServerPropagatesToSameServerProfileHosts(t *testing.T) {
	t.Chdir(t.TempDir())

	seed := map[string]any{
		"hosts": []any{
			map[string]any{
				"root_uri":             "https://old.example",
				"port":                 8080,
				"insecure_skip_verify": true,
				"socks5_proxy":         "localhost:1055",
				"tier":                 1,
				"token":                "admin-token",
			},
			map[string]any{ // same-server profile host: must move with hosts[0]
				"root_uri":             "https://old.example",
				"port":                 8080,
				"insecure_skip_verify": true,
				"profile_label":        "player2",
				"token":                "p2-token",
				"device_id":            "dev-p2",
			},
			map[string]any{ // different server (e.g. a genuine tier-2 endpoint): untouched
				"root_uri": "https://public.example",
				"tier":     2,
				"cf_access": map[string]any{
					"client_id": "id.access", "client_secret": "sec",
				},
			},
		},
	}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(configFileName, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var u HostUpdate
	u.SetServer("https://new.example", 0, false)
	if err := WriteHostUpdate(u); err != nil {
		t.Fatalf("WriteHostUpdate: %v", err)
	}

	hosts := hostsOf(t, readConfigTree(t))
	if len(hosts) != 3 {
		t.Fatalf("want 3 hosts, got %d", len(hosts))
	}

	// hosts[0]: the authoritative update.
	if got, _ := hosts[0]["root_uri"].(string); got != "https://new.example" {
		t.Errorf("hosts[0].root_uri = %q", got)
	}

	// Profile host: moved with hosts[0] — endpoint, port removal, insecure removal,
	// and hosts[0]'s transport fields mirrored.
	p := hosts[1]
	if got, _ := p["root_uri"].(string); got != "https://new.example" {
		t.Errorf("profile root_uri = %q — stale endpoint survives --set-server", got)
	}
	if _, present := p["port"]; present {
		t.Error("profile port not removed (port=0 must delete the key)")
	}
	if _, present := p["insecure_skip_verify"]; present {
		t.Error("profile insecure_skip_verify not removed")
	}
	if got, _ := p["socks5_proxy"].(string); got != "localhost:1055" {
		t.Errorf("profile socks5_proxy = %q — transport must mirror hosts[0]", got)
	}
	if got, _ := p["tier"].(float64); got != 1 {
		t.Errorf("profile tier = %v", p["tier"])
	}
	// Per-profile identity fields survive verbatim.
	if got, _ := p["token"].(string); got != "p2-token" {
		t.Errorf("profile token = %q", got)
	}
	if got, _ := p["device_id"].(string); got != "dev-p2" {
		t.Errorf("profile device_id = %q", got)
	}
	if got, _ := p["profile_label"].(string); got != "player2" {
		t.Errorf("profile_label = %q", got)
	}

	// Different-server host: byte-for-value untouched.
	q := hosts[2]
	if got, _ := q["root_uri"].(string); got != "https://public.example" {
		t.Errorf("other-server root_uri = %q — must not be dragged along", got)
	}
	if got, _ := q["tier"].(float64); got != 2 {
		t.Errorf("other-server tier = %v", q["tier"])
	}
	if cf, _ := q["cf_access"].(map[string]any); cf == nil || cf["client_id"] != "id.access" {
		t.Errorf("other-server cf_access mutated: %v", q["cf_access"])
	}
}
