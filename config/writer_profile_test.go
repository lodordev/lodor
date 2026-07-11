package config

import (
	"encoding/json"
	"os"
	"testing"
)

// readConfigTree loads the CWD-relative config.json as the generic tree the writer
// edits, failing the test on any read/parse error.
func readConfigTree(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(configFileName)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return root
}

func hostsOf(t *testing.T, root map[string]any) []map[string]any {
	t.Helper()
	raw, _ := root["hosts"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for i, h := range raw {
		hm, ok := h.(map[string]any)
		if !ok {
			t.Fatalf("hosts[%d] is not an object", i)
		}
		out = append(out, hm)
	}
	return out
}

// TestWriteProfileHostSeedsTransportFields: a NEW profile host must carry hosts[0]'s
// FULL endpoint — not just root_uri/port/insecure. ResolveHost returns a profile host
// verbatim (endpoint fields are never inherited), so a profile seeded without
// socks5_proxy/cf_access can never reach a Tailscale or Cloudflare-Access RomM.
func TestWriteProfileHostSeedsTransportFields(t *testing.T) {
	t.Chdir(t.TempDir())

	seed := map[string]any{
		"hosts": []any{map[string]any{
			"root_uri":             "https://romm.tail1234.ts.net",
			"port":                 8443,
			"insecure_skip_verify": true,
			"socks5_proxy":         "localhost:1055",
			"cf_access":            map[string]any{"client_id": "id.access", "client_secret": "sec"},
			"tier":                 1,
			"device_name":          "Mini Flip",
			"token":                "admin-token",
			"username":             "admin",
			"device_id":            "dev-admin",
		}},
	}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(configFileName, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteProfileHost("player2", "player2", "p2-token", "dev-p2", []string{"me.read"}); err != nil {
		t.Fatalf("WriteProfileHost: %v", err)
	}

	hosts := hostsOf(t, readConfigTree(t))
	if len(hosts) != 2 {
		t.Fatalf("want 2 hosts, got %d", len(hosts))
	}
	p := hosts[1]

	// Endpoint + transport fields must all have been seeded from hosts[0].
	if got, _ := p["root_uri"].(string); got != "https://romm.tail1234.ts.net" {
		t.Errorf("root_uri = %q", got)
	}
	if got, _ := p["port"].(float64); got != 8443 {
		t.Errorf("port = %v", p["port"])
	}
	if got, _ := p["insecure_skip_verify"].(bool); !got {
		t.Error("insecure_skip_verify not seeded")
	}
	if got, _ := p["socks5_proxy"].(string); got != "localhost:1055" {
		t.Errorf("socks5_proxy = %q — profile can't reach a Tailscale server without it", got)
	}
	cf, _ := p["cf_access"].(map[string]any)
	if cf == nil || cf["client_id"] != "id.access" || cf["client_secret"] != "sec" {
		t.Errorf("cf_access not seeded: %v", p["cf_access"])
	}
	if got, _ := p["tier"].(float64); got != 1 {
		t.Errorf("tier = %v", p["tier"])
	}
	if got, _ := p["device_name"].(string); got != "Mini Flip" {
		t.Errorf("device_name = %q", got)
	}

	// Identity fields are the PROFILE's own — never hosts[0]'s.
	if got, _ := p["token"].(string); got != "p2-token" {
		t.Errorf("token = %q", got)
	}
	if got, _ := p["device_id"].(string); got != "dev-p2" {
		t.Errorf("device_id = %q", got)
	}
	if got, _ := p["profile_label"].(string); got != "player2" {
		t.Errorf("profile_label = %q", got)
	}

	// hosts[0] is untouched.
	if got, _ := hosts[0]["token"].(string); got != "admin-token" {
		t.Errorf("hosts[0].token mutated: %q", got)
	}
}

// TestWriteProfileHostBareEndpointSeedsNoTransport: a plain single-endpoint hosts[0]
// (no proxy/CF/tier) seeds a profile with NO transport keys — the pre-fix shape is
// preserved byte-for-key on the common direct-connection card.
func TestWriteProfileHostBareEndpointSeedsNoTransport(t *testing.T) {
	t.Chdir(t.TempDir())

	seed := map[string]any{
		"hosts": []any{map[string]any{
			"root_uri": "http://192.168.1.10",
			"token":    "tok",
		}},
	}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(configFileName, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteProfileHost("kid", "kid", "kid-token", "", nil); err != nil {
		t.Fatalf("WriteProfileHost: %v", err)
	}
	hosts := hostsOf(t, readConfigTree(t))
	if len(hosts) != 2 {
		t.Fatalf("want 2 hosts, got %d", len(hosts))
	}
	for _, k := range []string{"socks5_proxy", "cf_access", "tier", "device_name", "port", "insecure_skip_verify"} {
		if _, present := hosts[1][k]; present {
			t.Errorf("bare endpoint must not seed %q", k)
		}
	}
	if got, _ := hosts[1]["root_uri"].(string); got != "http://192.168.1.10" {
		t.Errorf("root_uri = %q", got)
	}
}
