package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/platform"
)

// TestReconcileAfterDownloadFlipsMarkerAndMigratesSave exercises the exact CLI path the
// NextUI post-launch hook drives: lodor-sync --reconcile is handed the MARKED on-disk path
// the game launched from (the filled-in-place cloud stub "✘ Game"), and must (a) flip the
// marker to on-device "✓ Game", (b) carry the save written under the cloud name with the
// rename (no orphan), and (c) keep the catalog index's by_id path pointing at the real
// on-disk name. This is Issue 1 end-to-end, OFFLINE (no client).
func TestReconcileAfterDownloadFlipsMarkerAndMigratesSave(t *testing.T) {
	if platform.HostShowsStateNatively() {
		t.Skip("marker-less host (hard-true build tag): there is no ✘→✓ flip; reconcile is a no-op by design")
	}
	cfg, rom, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	base := filepath.Base(unmarked)
	cloud := filepath.Join(dir, platform.MarkerCloud+base) // the marked path the launcher used
	dev := filepath.Join(dir, platform.MarkerOnDevice+base)

	// Fetch-on-launch filled the cloud stub in place with real bytes; a save was written
	// under the cloud on-disk name while playing.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cloud, []byte("REAL ROM BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveDir := platform.SaveDirectory("gba")
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cloudSave := filepath.Join(saveDir, platform.MarkerCloud+base+".sav")
	if err := os.WriteFile(cloudSave, []byte("SAVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Seed the index by_id with the cloud path (what a mirror would have written).
	idx, _ := loadIndex(IndexPath(cfg))
	pi := idx.Platforms["gba"]
	if pi.ByID == nil {
		pi.ByID = map[int]string{}
	}
	pi.ByID[rom.ID] = string(os.PathSeparator) + filepath.Join("Roms", "GBA", platform.MarkerCloud+base)
	idx.Platforms["gba"] = pi
	if err := writeIndexAtomic(IndexPath(cfg), idx); err != nil {
		t.Fatal(err)
	}

	// The post-launch hook calls reconcile with the MARKED launch path.
	flipped := ReconcileAfterDownload(cfg, cloud)
	if !flipped {
		t.Fatalf("ReconcileAfterDownload should report a flip for a filled cloud stub")
	}
	if _, err := os.Stat(dev); err != nil {
		t.Fatalf("ROM not promoted to on-device name: %v", err)
	}
	if _, err := os.Stat(cloud); !os.IsNotExist(err) {
		t.Fatalf("cloud name should be gone after the flip")
	}
	devSave := filepath.Join(saveDir, platform.MarkerOnDevice+base+".sav")
	if _, err := os.Stat(devSave); err != nil {
		t.Fatalf("save not migrated with the rename (orphaned): %v", err)
	}
	if _, err := os.Stat(cloudSave); !os.IsNotExist(err) {
		t.Fatalf("orphaned save left under cloud name")
	}
	// Index by_id now points at the on-device path.
	idx2, _ := loadIndex(IndexPath(cfg))
	got := idx2.Platforms["gba"].ByID[rom.ID]
	want := string(os.PathSeparator) + filepath.Join("Roms", "GBA", platform.MarkerOnDevice+base)
	if got != want {
		t.Fatalf("index by_id = %q, want %q", got, want)
	}
	// Still resolves to the server rom_id.
	if id, ok := ResolveRomID(cfg, dev); !ok || id != 1234 {
		t.Fatalf("promoted ROM resolves to (%d,%v), want (1234,true)", id, ok)
	}
}

// TestReconcileAfterDownloadStubStaysCloud: a game still a 0-byte stub (never downloaded)
// is NOT flipped — reconcile reports no change.
func TestReconcileAfterDownloadStubStaysCloud(t *testing.T) {
	cfg, _, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	cloud := filepath.Join(dir, platform.MarkerCloud+filepath.Base(unmarked))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cloud, nil, 0o644); err != nil { // 0-byte
		t.Fatal(err)
	}
	if ReconcileAfterDownload(cfg, cloud) {
		t.Fatalf("a 0-byte stub must not flip to on-device")
	}
	if _, err := os.Stat(cloud); err != nil {
		t.Fatalf("stub should remain at cloud name: %v", err)
	}
}
