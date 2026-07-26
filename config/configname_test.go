package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfigFileNameHonorsEnv guards the spruceOS config-name collision fix: the engine's
// RomM config filename must follow LODOR_CONFIG_NAME so it can coexist with spruce's own
// App/Lodor/config.json app manifest. Default stays "config.json" for every other lane.
func TestConfigFileNameHonorsEnv(t *testing.T) {
	// Default (unset): config.json.
	t.Setenv("LODOR_CONFIG_NAME", "")
	if got := configFileName(); got != "config.json" {
		t.Fatalf("default configFileName = %q, want config.json", got)
	}
	// Overridden: the engine reads/writes the named file.
	t.Setenv("LODOR_CONFIG_NAME", "romm-config.json")
	if got := configFileName(); got != "romm-config.json" {
		t.Fatalf("overridden configFileName = %q, want romm-config.json", got)
	}
	// End-to-end: Load reads the OVERRIDDEN name, and a bare config.json is ignored.
	dir := t.TempDir()
	t.Chdir(dir)
	// A decoy config.json (spruce's app manifest) must NOT be loaded as the RomM config.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"label":"Lodor"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	good := `{"hosts":[{"root_uri":"https://romm.example.com","token":"T","device_name":"A30"}]}`
	if err := os.WriteFile(filepath.Join(dir, "romm-config.json"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with LODOR_CONFIG_NAME=romm-config.json failed: %v", err)
	}
	if len(cfg.Hosts) != 1 || cfg.Hosts[0].RootURI != "https://romm.example.com" {
		t.Fatalf("Load read the wrong file; hosts=%+v", cfg.Hosts)
	}
}
