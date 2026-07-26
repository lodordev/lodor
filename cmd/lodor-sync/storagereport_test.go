package main

// --storage-report (Argosy steal): the OFFLINE per-volume download-cache tally. These
// exercise computeStorageReport (the pure half — runStorageReport just prints + exits):
// downloaded (>0-byte) vs stub (0-byte) counts and byte sums, per mapped platform and
// total, under both ownership scopes.

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
)

// writeSized creates dir/name with n bytes of filler (n==0 => a 0-byte stub).
func writeSized(t *testing.T, dir, name string, n int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	body := make([]byte, n)
	for i := range body {
		body[i] = 'x'
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// storageFixture lays down a 2-platform card: GBA (2 downloaded + 2 stubs + one
// unowned user ROM) and SNES (1 downloaded + 1 stub, plus a .media dir that must be
// ignored). Returns the config and a marker-recorded manifest.
func storageFixture(t *testing.T) (*config.Config, *platform.Manifest) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("SDCARD_PATH", base)

	gba := filepath.Join(base, "Roms", "Game Boy Advance (GBA)")
	snes := filepath.Join(base, "Roms", "Super Nintendo (SNES)")

	man := platform.LoadManifest() // fresh/empty (no LODOR_PAK_DIR file)
	man.Record(writeSized(t, gba, platform.MarkerOnDevice+"GameA.gba", 100), platform.ManifestDownload, 1)
	man.Record(writeSized(t, gba, platform.MarkerOnDevice+"GameB.gba", 250), platform.ManifestDownload, 2)
	man.Record(writeSized(t, gba, platform.MarkerCloud+"GameC.gba", 0), platform.ManifestStub, 3)
	man.Record(writeSized(t, gba, platform.MarkerCloud+"GameD.gba", 0), platform.ManifestStub, 4)
	// A user's own ROM — no marker, not in the manifest: outside the Lodor cache.
	writeSized(t, gba, "User Homebrew.gba", 500)

	man.Record(writeSized(t, snes, platform.MarkerOnDevice+"Mario.sfc", 1000), platform.ManifestDownload, 5)
	man.Record(writeSized(t, snes, platform.MarkerCloud+"Zelda.sfc", 0), platform.ManifestStub, 6)
	// .media cover dir (dotfile) must be skipped, never counted as a ROM.
	writeSized(t, filepath.Join(snes, ".media"), "Mario.png", 42)

	cfg := &config.Config{
		DirectoryMappings: map[string]config.DirMapping{
			"gba":  {Slug: "gba", RelativePath: "Game Boy Advance (GBA)"},
			"snes": {Slug: "snes", RelativePath: "Super Nintendo (SNES)"},
		},
	}
	return cfg, man
}

func rowBySlug(rows []platformStorage, slug string) (platformStorage, bool) {
	for _, r := range rows {
		if r.Slug == slug {
			return r, true
		}
	}
	return platformStorage{}, false
}

// TestStorageReportOwnedScope: with a non-empty manifest the tally scopes to
// Lodor-owned ROMs — the user's own file is excluded from GBA's numbers.
func TestStorageReportOwnedScope(t *testing.T) {
	cfg, man := storageFixture(t)
	rows, totDl, totStubs, totBytes, scopeOwned := computeStorageReport(cfg, man)

	if !scopeOwned {
		t.Fatal("scopeOwned = false with a populated manifest, want true")
	}
	gba, ok := rowBySlug(rows, "gba")
	if !ok {
		t.Fatal("gba row missing")
	}
	if gba.Downloaded != 2 || gba.DownloadedBytes != 350 || gba.Stubs != 2 {
		t.Errorf("gba = {dl:%d bytes:%d stubs:%d}, want {2 350 2} (user ROM must be excluded)",
			gba.Downloaded, gba.DownloadedBytes, gba.Stubs)
	}
	snes, ok := rowBySlug(rows, "snes")
	if !ok {
		t.Fatal("snes row missing")
	}
	if snes.Downloaded != 1 || snes.DownloadedBytes != 1000 || snes.Stubs != 1 {
		t.Errorf("snes = {dl:%d bytes:%d stubs:%d}, want {1 1000 1} (.media must be skipped)",
			snes.Downloaded, snes.DownloadedBytes, snes.Stubs)
	}
	if totDl != 3 || totBytes != 1350 || totStubs != 3 {
		t.Errorf("totals = {dl:%d bytes:%d stubs:%d}, want {3 1350 3}", totDl, totBytes, totStubs)
	}
}

// TestStorageReportAllScope: with an empty manifest the report falls back to all ROMs
// under the mapped dirs — the user's file now counts toward GBA.
func TestStorageReportAllScope(t *testing.T) {
	cfg, _ := storageFixture(t)
	empty := &platform.Manifest{Version: 1, Entries: map[string]platform.ManifestEntry{}}
	rows, totDl, totStubs, totBytes, scopeOwned := computeStorageReport(cfg, empty)

	if scopeOwned {
		t.Fatal("scopeOwned = true with an empty manifest, want false")
	}
	gba, ok := rowBySlug(rows, "gba")
	if !ok {
		t.Fatal("gba row missing")
	}
	// Now the 500-byte user ROM is included: 3 downloaded, 850 bytes, 2 stubs.
	if gba.Downloaded != 3 || gba.DownloadedBytes != 850 || gba.Stubs != 2 {
		t.Errorf("gba (all scope) = {dl:%d bytes:%d stubs:%d}, want {3 850 2}",
			gba.Downloaded, gba.DownloadedBytes, gba.Stubs)
	}
	if totDl != 4 || totBytes != 1850 || totStubs != 3 {
		t.Errorf("totals (all scope) = {dl:%d bytes:%d stubs:%d}, want {4 1850 3}", totDl, totBytes, totStubs)
	}
}
