package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile is a tiny helper for the overlay tests.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSettingsConfOverlay verifies the UI-owned settings.conf toggles override config.json
// without the launcher ever writing the token-bearing config.json: fetch_covers flips the
// bulk-cover gate and mirror_mode sets the coexist mode.
func TestSettingsConfOverlay(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// config.json: covers OFF, mode own.
	writeFile(t, dir, "config.json", `{"hosts":[{"root_uri":"https://romm.example.com","token":"x"}],"mirror_mode":"own"}`)
	// settings.conf: UI turned box art ON and switched to separate folders.
	writeFile(t, dir, "settings.conf", "fetch_covers=on\nmirror_mode=separate\n# a comment\n")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.CoversEnabled() {
		t.Errorf("fetch_covers=on in settings.conf should enable bulk covers")
	}
	if c.ResolvedMirrorMode() != MirrorModeSeparate {
		t.Errorf("mirror_mode=separate in settings.conf should win; got %q", c.ResolvedMirrorMode())
	}
}

// TestSettingsConfOff: an explicit off overrides a config.json that enabled covers.
func TestSettingsConfOff(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "config.json", `{"hosts":[{"root_uri":"https://romm.example.com","token":"x"}],"fetch_covers":true}`)
	writeFile(t, dir, "settings.conf", "fetch_covers=off\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.CoversEnabled() {
		t.Errorf("fetch_covers=off in settings.conf should disable bulk covers")
	}
}

// TestSettingsConfAbsent: no settings.conf is a clean no-op (config.json stands).
func TestSettingsConfAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "config.json", `{"hosts":[{"root_uri":"https://romm.example.com","token":"x"}],"fetch_covers":true}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.CoversEnabled() {
		t.Errorf("with no settings.conf, config.json fetch_covers=true should stand")
	}
}
