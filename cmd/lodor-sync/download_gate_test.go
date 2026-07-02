package main

// V5 (C1 coexist-design audit): the download fill removes/overwrites whatever
// sits at dest, so dest must be mirror-owned or triple-gate reclaimable — a
// USER's 0-byte file with a canonical name (the merge dedup edge) and a user's
// REAL file are both refused.

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
)

func gateTestEnv(t *testing.T) (*config.Config, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_HOST_OS", "nextui")
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))
	dir := filepath.Join(base, "Roms", "Game Boy Advance (GBA)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DirectoryMappings: map[string]config.DirMapping{"gba": {RelativePath: "Game Boy Advance (GBA)"}},
	}
	return cfg, dir
}

func TestDownloadDestAllowed(t *testing.T) {
	cfg, dir := gateTestEnv(t)
	man := platform.LoadManifest()

	// Absent dest: additive, allowed.
	if !downloadDestAllowed(cfg, man, filepath.Join(dir, "✘ Fresh.gba")) {
		t.Error("absent dest refused")
	}

	// Manifest-owned stub: allowed.
	owned := filepath.Join(dir, "✘ Ours.gba")
	if err := os.WriteFile(owned, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	man.Record(owned, platform.ManifestStub, 7)
	if !downloadDestAllowed(cfg, man, owned) {
		t.Error("manifest-owned stub refused")
	}

	// User's 0-byte file at a canonical name (no marker, unowned): REFUSED — the
	// exact V5 hole (modes.go used to os.Remove(dest) before the rename).
	userZero := filepath.Join(dir, "Broken Copy.gba")
	if err := os.WriteFile(userZero, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if downloadDestAllowed(cfg, man, userZero) {
		t.Error("user 0-byte file allowed as download dest (V5)")
	}

	// User's REAL file (adopted in merge mode — resolvable, unowned): REFUSED.
	userReal := filepath.Join(dir, "Their Game.gba")
	if err := os.WriteFile(userReal, []byte("REAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if downloadDestAllowed(cfg, man, userReal) {
		t.Error("user real file allowed as download dest — would be replaced by the server copy")
	}

	// Directory at dest: refused.
	sub := filepath.Join(dir, "Some Folder")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if downloadDestAllowed(cfg, man, sub) {
		t.Error("directory allowed as download dest")
	}
}
