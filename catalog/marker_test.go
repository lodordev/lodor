package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// markerTestCfg builds a config + on-disk catalog index that maps the GBA folder back to
// slug "gba" with one ROM ("Sonic (USA)" -> rom_id 1234), rooted at a temp SDCARD. It
// returns the cfg and the canonical (unmarked) on-disk ROM path.
func markerTestCfg(t *testing.T) (*config.Config, romm.Rom, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")

	cfg := &config.Config{
		MirrorMode:        config.MirrorModeOwn, // unmarked canonical basename
		DirectoryMappings: map[string]config.DirMapping{"gba": {RelativePath: "GBA"}},
	}
	rom := romm.Rom{
		ID:             1234,
		PlatformFsSlug: "gba",
		FsName:         "Sonic (USA).gba",
		Files:          []romm.RomFile{{FileName: "Sonic (USA).gba"}},
	}

	// Write the index keyed by the UNMARKED canonical basename, exactly as MirrorCatalog does.
	idx := index{Version: 1, Platforms: map[string]platformIndex{
		"gba": {
			ByBasename: map[string]int{platform.LocalBasename(cfg, rom): rom.ID},
			ByFsname:   map[string]int{rom.FsName: rom.ID},
			ByID:       map[int]string{},
		},
	}}
	if err := os.MkdirAll(filepath.Dir(IndexPath(cfg)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeIndexAtomic(IndexPath(cfg), idx); err != nil {
		t.Fatal(err)
	}
	return cfg, rom, platform.LocalRomPath(cfg, rom)
}

// TestMarkedRomResolvesToSameRomID is the load-bearing correctness check: a ROM bearing
// EITHER leading state marker (cloud "[^] " or on-device "[v] ") must reverse to the same
// rom_id as the unmarked name, so saves/catalog still match the server.
func TestMarkedRomResolvesToSameRomID(t *testing.T) {
	cfg, _, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	base := filepath.Base(unmarked)

	cases := map[string]string{
		"unmarked":  filepath.Join(dir, base),
		"cloud":     filepath.Join(dir, platform.MarkerCloud+base),
		"on-device": filepath.Join(dir, platform.MarkerOnDevice+base),
	}
	for name, p := range cases {
		id, ok := ResolveRomID(cfg, p)
		if !ok || id != 1234 {
			t.Errorf("%s: ResolveRomID(%q) = (%d,%v), want (1234,true)", name, filepath.Base(p), id, ok)
		}
	}
}

// TestReconcileCreatesCloudStub: with nothing on disk, reconcile drops a 0-byte cloud
// stub and reports a create.
func TestReconcileCreatesCloudStub(t *testing.T) {
	cfg, rom, unmarked := markerTestCfg(t)
	final, created := platform.ReconcileMarkedPresence(cfg, rom, unmarked)
	if !created {
		t.Fatalf("expected didCreate=true for a fresh stub")
	}
	wantBase := platform.MarkerCloud + filepath.Base(unmarked)
	if filepath.Base(final) != wantBase {
		t.Fatalf("final base = %q, want %q", filepath.Base(final), wantBase)
	}
	if fi, err := os.Stat(final); err != nil || fi.Size() != 0 {
		t.Fatalf("cloud stub not a 0-byte file: %v", err)
	}
	// Idempotent: a second pass over the same 0-byte stub does NOT recreate it.
	if _, created2 := platform.ReconcileMarkedPresence(cfg, rom, unmarked); created2 {
		t.Fatalf("second reconcile re-created an existing stub")
	}
}

// TestReconcilePromotesAndMigratesSave: a cloud stub that has been FILLED in place (real
// bytes) is promoted to the on-device marker, AND the save written under the cloud name
// (the fetch-on-launch window) is migrated so it never orphans — and still resolves to
// the server rom.
func TestReconcilePromotesAndMigratesSave(t *testing.T) {
	cfg, rom, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	base := filepath.Base(unmarked)
	cloud := filepath.Join(dir, platform.MarkerCloud+base)
	dev := filepath.Join(dir, platform.MarkerOnDevice+base)

	// 1. Fetch-on-launch filled the cloud stub in place with real bytes.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cloud, []byte("REAL ROM BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 2. The user played once -> a save was created under the CLOUD on-disk name.
	saveDir := platform.SaveDirectory("gba") // <Saves>/GBA
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cloudSave := filepath.Join(saveDir, platform.MarkerCloud+base+".sav")
	if err := os.WriteFile(cloudSave, []byte("SAVE"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. The next mirror reconciles: real file -> promote to "[v] ", carrying the save.
	final, created := platform.ReconcileMarkedPresence(cfg, rom, unmarked)
	if created {
		t.Fatalf("promotion of an existing file must not count as a create")
	}
	if final != dev {
		t.Fatalf("final = %q, want on-device %q", final, dev)
	}
	if _, err := os.Stat(cloud); !os.IsNotExist(err) {
		t.Fatalf("cloud name should be gone after promotion, stat err=%v", err)
	}
	if _, err := os.Stat(dev); err != nil {
		t.Fatalf("on-device ROM missing after promotion: %v", err)
	}
	// The save followed the rename — no orphan under the cloud name.
	devSave := filepath.Join(saveDir, platform.MarkerOnDevice+base+".sav")
	if _, err := os.Stat(devSave); err != nil {
		t.Fatalf("save not migrated to on-device name: %v", err)
	}
	if _, err := os.Stat(cloudSave); !os.IsNotExist(err) {
		t.Fatalf("orphaned save left under cloud name")
	}
	// The promoted ROM still resolves to the server rom_id (save round-trip intact).
	if id, ok := ResolveRomID(cfg, dev); !ok || id != 1234 {
		t.Fatalf("promoted ROM resolves to (%d,%v), want (1234,true)", id, ok)
	}
}

// TestReconcileMigratesLegacyUnmarked: the first marked mirror over an OLD unmarked
// deployment converts both a 0-byte stub (-> cloud) and an already-downloaded real file
// (-> on-device), so the 4935 unmarked stubs already on the card upgrade cleanly.
func TestReconcileMigratesLegacyUnmarked(t *testing.T) {
	cfg, rom, unmarked := markerTestCfg(t)
	if err := os.MkdirAll(filepath.Dir(unmarked), 0o755); err != nil {
		t.Fatal(err)
	}

	// Legacy 0-byte stub -> cloud.
	if err := os.WriteFile(unmarked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	final, _ := platform.ReconcileMarkedPresence(cfg, rom, unmarked)
	if filepath.Base(final) != platform.MarkerCloud+filepath.Base(unmarked) {
		t.Fatalf("legacy stub -> %q, want cloud-marked", filepath.Base(final))
	}

	// Legacy real file -> on-device.
	unmarked2 := filepath.Join(filepath.Dir(unmarked), "Zelda (USA).gba")
	if err := os.WriteFile(unmarked2, []byte("ROM"), 0o644); err != nil {
		t.Fatal(err)
	}
	rom2 := rom
	rom2.ID = 5678
	rom2.FsName = "Zelda (USA).gba"
	rom2.Files = []romm.RomFile{{FileName: "Zelda (USA).gba"}}
	final2, _ := platform.ReconcileMarkedPresence(cfg, rom2, unmarked2)
	if filepath.Base(final2) != platform.MarkerOnDevice+"Zelda (USA).gba" {
		t.Fatalf("legacy real file -> %q, want on-device-marked", filepath.Base(final2))
	}
}
