package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestProfilesRoundTrip verifies the multi-user profiles[]/active_profile schema
// round-trips through Load and that ActiveHost() overlays the active profile's identity
// onto hosts[0] (shared server) without taking the server address from the profile.
func TestProfilesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	raw := `{
      "hosts": [{"root_uri": "https://romm.example.com", "port": 8443, "token": "SHARED-OWNER-TOKEN", "device_id": "dev-owner", "username": "owner"}],
      "profiles": [
        {"label": "Alice", "token": "ALICE-TOKEN", "device_id": "dev-alice", "username": "alice"},
        {"label": "Bob",   "token": "BOB-TOKEN",   "device_id": "dev-bob",   "username": "bob"}
      ],
      "active_profile": "Bob",
      "api_timeout": 30,
      "download_timeout": 3600
    }`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	os.Unsetenv("LODOR_PROFILE")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Profiles) != 2 {
		t.Fatalf("profiles: got %d want 2", len(c.Profiles))
	}
	if c.ActiveProfileName() != "Bob" {
		t.Fatalf("active: got %q want Bob", c.ActiveProfileName())
	}

	h := c.ActiveHost()
	// Identity comes from the active profile (Bob)...
	if h.Token != "BOB-TOKEN" {
		t.Errorf("token: got %q want BOB-TOKEN", h.Token)
	}
	if h.DeviceID != "dev-bob" {
		t.Errorf("device_id: got %q want dev-bob", h.DeviceID)
	}
	if h.Username != "bob" {
		t.Errorf("username: got %q want bob", h.Username)
	}
	// ...but the SERVER ADDRESS stays hosts[0]'s (never from the profile).
	if h.RootURI != "https://romm.example.com" || h.Port != 8443 {
		t.Errorf("server overlay leaked: got %q:%d", h.RootURI, h.Port)
	}

	// LODOR_PROFILE overrides the config's active_profile.
	os.Setenv("LODOR_PROFILE", "Alice")
	defer os.Unsetenv("LODOR_PROFILE")
	if c.ActiveProfileName() != "Alice" {
		t.Fatalf("env override: got %q want Alice", c.ActiveProfileName())
	}
	if c.ActiveHost().Token != "ALICE-TOKEN" {
		t.Errorf("env-overlaid token: got %q want ALICE-TOKEN", c.ActiveHost().Token)
	}
}

// TestActiveHostNoProfile asserts a single-user config (no profiles) yields hosts[0]
// byte-identical via ActiveHost — the no-regression guarantee for existing cards.
func TestActiveHostNoProfile(t *testing.T) {
	os.Unsetenv("LODOR_PROFILE")
	c := &Config{Hosts: []Host{{RootURI: "https://x", Token: "T", DeviceID: "D", Username: "u"}}}
	h := c.ActiveHost()
	if h.Token != "T" || h.DeviceID != "D" || h.Username != "u" || h.RootURI != "https://x" {
		t.Fatalf("ActiveHost mutated hosts[0] with no profile: %+v", h)
	}
}

// TestWriteProfileUpdateRoundTrip verifies WriteProfileUpdate creates a profile, writes
// its identity, preserves hosts[] + unknown keys, and that SetActiveProfile selects it.
func TestWriteProfileUpdateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	os.Unsetenv("LODOR_PROFILE")

	seed := `{"hosts":[{"root_uri":"https://srv","token":"OWNER"}],"directory_mappings":{"gba":{"relative_path":"GBA"}},"weird_key":42}`
	if err := os.WriteFile("config.json", []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	upd := HostUpdate{Username: "kid", DeviceID: "kid-dev", DeviceName: "Kid A30"}
	upd.SetToken("KID-TOKEN", "kid token", "", []string{"assets.read"})
	if err := WriteProfileUpdate("Kid", upd); err != nil {
		t.Fatalf("WriteProfileUpdate: %v", err)
	}
	if err := SetActiveProfile("Kid"); err != nil {
		t.Fatalf("SetActiveProfile: %v", err)
	}

	// Unknown key + hosts must survive.
	var tree map[string]any
	b, _ := os.ReadFile("config.json")
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatal(err)
	}
	if tree["weird_key"] == nil {
		t.Error("unknown key weird_key was dropped")
	}
	if _, ok := tree["hosts"].([]any); !ok {
		t.Error("hosts[] was dropped")
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	h := c.ActiveHost()
	if h.Token != "KID-TOKEN" || h.DeviceID != "kid-dev" || h.Username != "kid" {
		t.Errorf("profile identity not active after write: %+v", h)
	}
	if h.RootURI != "https://srv" {
		t.Errorf("shared server lost: %q", h.RootURI)
	}

	// RemoveProfile clears it and the active selector.
	if err := RemoveProfile("Kid"); err != nil {
		t.Fatalf("RemoveProfile: %v", err)
	}
	c2, _ := Load()
	if len(c2.Profiles) != 0 || c2.ActiveProfileName() != "" {
		t.Errorf("remove left residue: profiles=%d active=%q", len(c2.Profiles), c2.ActiveProfileName())
	}
}
