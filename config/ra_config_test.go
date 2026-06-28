package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteRACredentialsRoundTrip writes RA creds into a config that already has a
// host + unrelated keys, then reloads to confirm the creds land top-level AND every
// pre-existing key survives untouched (the writer's preserve-unknown guarantee).
func TestWriteRACredentialsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	seed := map[string]any{
		"hosts": []any{map[string]any{
			"root_uri": "https://example.test",
			"token":    "ROMM_BEARER",
			"device_id": "dev-1",
		}},
		"directory_mappings": map[string]any{"gba": map[string]any{"relative_path": "GBA"}},
		"api_timeout": 30,
		"launcher_private_key": "keep-me", // an unknown key must survive
	}
	writeJSON(t, filepath.Join(dir, "config.json"), seed)

	if err := WriteRACredentials("Bob", "RA_TOKEN_XYZ"); err != nil {
		t.Fatalf("WriteRACredentials: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RAUsername != "Bob" || cfg.RAToken != "RA_TOKEN_XYZ" {
		t.Errorf("RA creds = %q/%q", cfg.RAUsername, cfg.RAToken)
	}
	if !cfg.RALoggedIn() {
		t.Error("RALoggedIn should be true")
	}
	// Pre-existing data preserved.
	if len(cfg.Hosts) != 1 || cfg.Hosts[0].Token != "ROMM_BEARER" || cfg.Hosts[0].DeviceID != "dev-1" {
		t.Errorf("host clobbered: %+v", cfg.Hosts)
	}
	raw := readJSON(t, filepath.Join(dir, "config.json"))
	if raw["launcher_private_key"] != "keep-me" {
		t.Errorf("unknown key not preserved: %v", raw["launcher_private_key"])
	}
	// RA creds are TOP-LEVEL, not nested in the host.
	if _, nested := raw["hosts"].([]any)[0].(map[string]any)["ra_token"]; nested {
		t.Error("ra_token leaked into hosts[0] — must be top-level")
	}
}

// TestWriteRACredentialsClear: an empty token removes both keys (logout).
func TestWriteRACredentialsClear(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeJSON(t, filepath.Join(dir, "config.json"), map[string]any{
		"hosts":       []any{map[string]any{"root_uri": "https://example.test"}},
		"ra_username": "Bob",
		"ra_token":    "OLD",
	})
	if err := WriteRACredentials("", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RALoggedIn() {
		t.Error("RALoggedIn should be false after clear")
	}
	raw := readJSON(t, filepath.Join(dir, "config.json"))
	if _, ok := raw["ra_token"]; ok {
		t.Error("ra_token key should be gone after clear")
	}
}

// TestRALoggedInNilSafe guards the nil receiver path used by the menu.
func TestRALoggedInNilSafe(t *testing.T) {
	var c *Config
	if c.RALoggedIn() {
		t.Error("nil config must not be logged in")
	}
	if (&Config{RAUsername: "Bob"}).RALoggedIn() {
		t.Error("username without token is not logged in")
	}
}

// --- tiny local helpers (kept here to avoid touching shared test files) ---

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}
