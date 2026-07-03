package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfigInCWD writes a config.json into a temp dir and chdirs there so Load()
// (which reads config.json CWD-relative) picks it up. Restores CWD on cleanup.
func writeConfigInCWD(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// TestApiTimeoutClampsToCeiling proves an over-limit api_timeout is clamped to the
// 300s ceiling, NOT reset to the 30s default (bug #166 — the doc says clamp-to-300).
func TestApiTimeoutClampsToCeiling(t *testing.T) {
	writeConfigInCWD(t, `{"hosts":[],"api_timeout":600,"download_timeout":3600}`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ApiTimeout != 300 {
		t.Fatalf("ApiTimeout = %d, want 300 (clamped, not reset to 30)", c.ApiTimeout)
	}
}

// TestApiTimeoutInRangeUntouched proves an in-range value is left alone.
func TestApiTimeoutInRangeUntouched(t *testing.T) {
	writeConfigInCWD(t, `{"hosts":[],"api_timeout":120,"download_timeout":3600}`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ApiTimeout != 120 {
		t.Fatalf("ApiTimeout = %d, want 120 (untouched)", c.ApiTimeout)
	}
}

// TestApiTimeoutZeroDefaults proves a zero/absent value still takes the 30s default.
func TestApiTimeoutZeroDefaults(t *testing.T) {
	writeConfigInCWD(t, `{"hosts":[],"api_timeout":0,"download_timeout":3600}`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ApiTimeout != 30 {
		t.Fatalf("ApiTimeout = %d, want 30 (default)", c.ApiTimeout)
	}
}

// TestDownloadTimeoutClampsToCeiling proves a runaway download_timeout is bounded.
func TestDownloadTimeoutClampsToCeiling(t *testing.T) {
	writeConfigInCWD(t, `{"hosts":[],"api_timeout":30,"download_timeout":999999999}`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DownloadTimeout != maxDownloadTimeout {
		t.Fatalf("DownloadTimeout = %d, want %d (clamped)", c.DownloadTimeout, maxDownloadTimeout)
	}
}
