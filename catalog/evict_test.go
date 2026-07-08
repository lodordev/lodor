package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/platform"
)

// TestEvictFlipsToCloudStubAndKeepsSave exercises the Game Manager "Delete from
// card" contract end-to-end: a downloaded on-device game ("✓ …") is truncated to
// a 0-byte stub, renamed back to its cloud name ("✘ …"), and its SAVE and COVER
// ride the rename — never deleted, never orphaned. The index by_id follows.
func TestEvictFlipsToCloudStubAndKeepsSave(t *testing.T) {
	if platform.HostShowsStateNatively() {
		t.Skip("marker-less host (hard-true build tag): evict writes an unmarked stub; ✘ expectations do not apply")
	}
	cfg, rom, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	base := filepath.Base(unmarked)
	dev := filepath.Join(dir, platform.MarkerOnDevice+base)
	cloud := filepath.Join(dir, platform.MarkerCloud+base)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dev, []byte("REAL ROM BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveDir := platform.SaveDirectory("gba")
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	devSave := filepath.Join(saveDir, platform.MarkerOnDevice+base+".sav")
	if err := os.WriteFile(devSave, []byte("SAVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	stem := base[:len(base)-len(filepath.Ext(base))]
	media := filepath.Join(dir, ".media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	devCover := filepath.Join(media, platform.MarkerOnDevice+stem+".png")
	if err := os.WriteFile(devCover, []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Index by_id points at the on-device path (what a post-download mirror wrote).
	idx, _ := loadIndex(IndexPath(cfg))
	pi := idx.Platforms["gba"]
	if pi.ByID == nil {
		pi.ByID = map[int]string{}
	}
	pi.ByID[rom.ID] = string(os.PathSeparator) + filepath.Join("Roms", "GBA", platform.MarkerOnDevice+base)
	idx.Platforms["gba"] = pi
	if err := writeIndexAtomic(IndexPath(cfg), idx); err != nil {
		t.Fatal(err)
	}

	// V3: evict requires manifest kind=download — record it as the mirror/download
	// path would have.
	man := platform.LoadManifest()
	man.Record(dev, platform.ManifestDownload, rom.ID)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	evicted, reason := EvictToStub(cfg, dev)
	if !evicted {
		t.Fatalf("EvictToStub refused a real on-device game (reason=%q)", reason)
	}
	fi, err := os.Stat(cloud)
	if err != nil {
		t.Fatalf("cloud stub not present after evict: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("cloud stub not 0 bytes (size=%d)", fi.Size())
	}
	if _, err := os.Stat(dev); !os.IsNotExist(err) {
		t.Fatalf("on-device name should be gone after evict")
	}
	// The save survived, migrated to the cloud name, bytes intact.
	cloudSave := filepath.Join(saveDir, platform.MarkerCloud+base+".sav")
	data, serr := os.ReadFile(cloudSave)
	if serr != nil {
		t.Fatalf("save not migrated with the evict (orphaned/deleted): %v", serr)
	}
	if string(data) != "SAVE" {
		t.Fatalf("save bytes changed across evict: %q", data)
	}
	if _, err := os.Stat(devSave); !os.IsNotExist(err) {
		t.Fatalf("orphaned save left under on-device name")
	}
	if _, err := os.Stat(filepath.Join(media, platform.MarkerCloud+stem+".png")); err != nil {
		t.Fatalf("cover not migrated with the evict: %v", err)
	}
	idx2, _ := loadIndex(IndexPath(cfg))
	want := string(os.PathSeparator) + filepath.Join("Roms", "GBA", platform.MarkerCloud+base)
	if got := idx2.Platforms["gba"].ByID[rom.ID]; got != want {
		t.Fatalf("index by_id = %q, want %q", got, want)
	}
	if id, ok := ResolveRomID(cfg, cloud); !ok || id != rom.ID {
		t.Fatalf("evicted stub resolves to (%d,%v), want (%d,true)", id, ok, rom.ID)
	}
}

// TestEvictRefusesStubAndMissing: a 0-byte stub or an absent path is not evictable.
func TestEvictRefusesStubAndMissing(t *testing.T) {
	cfg, _, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	base := filepath.Base(unmarked)
	cloud := filepath.Join(dir, platform.MarkerCloud+base)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cloud, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, reason := EvictToStub(cfg, cloud); ok || reason != "stub" {
		t.Fatalf("stub evict = (%v,%q), want (false,stub)", ok, reason)
	}
	if ok, reason := EvictToStub(cfg, filepath.Join(dir, "nope.gba")); ok || reason != "missing" {
		t.Fatalf("missing evict = (%v,%q), want (false,missing)", ok, reason)
	}
}

// TestEvictRefusesUnmanagedPath: a real file OUTSIDE any managed platform folder
// is refused untouched — the library never deletes what it doesn't own.
func TestEvictRefusesUnmanagedPath(t *testing.T) {
	cfg, _, _ := markerTestCfg(t)
	dir := filepath.Join(os.Getenv("SDCARD_PATH"), "Roms", "Unknown")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "thing.bin")
	if err := os.WriteFile(p, []byte("BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, reason := EvictToStub(cfg, p)
	if ok || reason != "resolve" {
		t.Fatalf("unmanaged evict = (%v,%q), want (false,resolve)", ok, reason)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "BYTES" {
		t.Fatalf("unmanaged file was modified: %q", data)
	}
}

// TestEvictMultiDiscRemovesDiscFiles: evicting a real .m3u deletes the disc files
// it references (they ARE the bytes), removes the emptied per-game folder, and
// leaves the .m3u itself as the 0-byte cloud stub.
func TestEvictMultiDiscRemovesDiscFiles(t *testing.T) {
	if platform.HostShowsStateNatively() {
		t.Skip("marker-less host (hard-true build tag): evict writes an unmarked stub; ✘ expectations do not apply")
	}
	cfg, _, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	dev := filepath.Join(dir, platform.MarkerOnDevice+"Chrono (USA).m3u")
	discDir := filepath.Join(dir, "Chrono (USA)")
	if err := os.MkdirAll(discDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"disc1.chd", "disc2.chd"} {
		if err := os.WriteFile(filepath.Join(discDir, d), []byte("DISC"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m3u := "Chrono (USA)/disc1.chd\nChrono (USA)/disc2.chd\n../outside.bin\n"
	if err := os.WriteFile(dev, []byte(m3u), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(dir), "outside.bin")
	if err := os.WriteFile(outside, []byte("SAFE"), 0o644); err != nil {
		t.Fatal(err)
	}
	man := platform.LoadManifest()
	man.Record(dev, platform.ManifestDownload, 1234)
	man.Record(discDir, platform.ManifestFolder, 1234)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	evicted, reason := EvictToStub(cfg, dev)
	if !evicted {
		t.Fatalf("multi-disc evict refused (reason=%q)", reason)
	}
	if _, err := os.Stat(discDir); !os.IsNotExist(err) {
		t.Fatalf("disc folder not removed")
	}
	cloud := filepath.Join(dir, platform.MarkerCloud+"Chrono (USA).m3u")
	fi, err := os.Stat(cloud)
	if err != nil || fi.Size() != 0 {
		t.Fatalf("m3u not left as 0-byte cloud stub (err=%v)", err)
	}
	// The ".."-escaping line was ignored — nothing outside the folder is touched.
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("path-traversal guard failed: outside file deleted")
	}
}

// TestEvictRefusesAdoptedUserFile is the V3 kill-shot from the C1 audit: in merge
// mode an ADOPTED user file RESOLVES to a rom_id (that's the feature), so before
// the manifest gate the Game Manager's "Delete from card" would have TRUNCATED THE
// USER'S OWN ROM to 0 bytes — the highest-severity violation found. Resolution
// alone must never authorize destruction: an unmanifested real file is refused,
// byte-identical, reason "not-lodor-managed".
func TestEvictRefusesAdoptedUserFile(t *testing.T) {
	cfg, _, unmarked := markerTestCfg(t)
	// The user's own file at the canonical (adopted) name — resolvable, real bytes,
	// NOT in the manifest.
	if err := os.MkdirAll(filepath.Dir(unmarked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unmarked, []byte("THE USER'S ROM"), 0o644); err != nil {
		t.Fatal(err)
	}
	if id, ok := ResolveRomID(cfg, unmarked); !ok || id != 1234 {
		t.Fatalf("precondition: adopted file must resolve (got %d,%v)", id, ok)
	}

	evicted, reason := EvictToStub(cfg, unmarked)
	if evicted || reason != "not-lodor-managed" {
		t.Fatalf("evict on adopted user file = (%v,%q), want (false,not-lodor-managed)", evicted, reason)
	}
	data, err := os.ReadFile(unmarked)
	if err != nil || string(data) != "THE USER'S ROM" {
		t.Fatalf("user ROM modified by refused evict (err=%v data=%q)", err, data)
	}
}

// TestEvictRefusesOnCorruptManifest: ownership unknowable ⇒ destructive no-op —
// even for a file that LOOKS like a Lodor download (✓-marked, resolvable).
func TestEvictRefusesOnCorruptManifest(t *testing.T) {
	cfg, _, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	dev := filepath.Join(dir, platform.MarkerOnDevice+filepath.Base(unmarked))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dev, []byte("REAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform.ManifestPath(), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if evicted, reason := EvictToStub(cfg, dev); evicted || reason != "not-lodor-managed" {
		t.Fatalf("evict with corrupt manifest = (%v,%q), want refuse", evicted, reason)
	}
	if data, _ := os.ReadFile(dev); string(data) != "REAL" {
		t.Fatalf("file modified under corrupt manifest: %q", data)
	}
}

// TestEvictRefusesGameManagerEntry locks the task #128 contract: the NextUI Game
// Manager ROOT ENTRY — Roms/"0) Game Manager (LODORGM)"/Open Game Manager.gm — is a
// launcher affordance, not a game. "LODORGM" is in no directory mapping and no
// emulator-folder tag table, so every engine pass that resolves a rom path must
// REFUSE it (reason "resolve") and leave the bytes alone: evict can never truncate
// it into a "cloud stub", and ResolveRomID can never hand it a rom_id to sync.
func TestEvictRefusesGameManagerEntry(t *testing.T) {
	cfg, _, unmarked := markerTestCfg(t)
	gmDir := filepath.Join(filepath.Dir(filepath.Dir(unmarked)), "0) Game Manager (LODORGM)")
	if err := os.MkdirAll(gmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(gmDir, "Open Game Manager.gm")
	body := []byte("Lodor Game Manager root entry — not a game.\n")
	if err := os.WriteFile(entry, body, 0o644); err != nil {
		t.Fatal(err)
	}

	evicted, reason := EvictToStub(cfg, entry)
	if evicted {
		t.Fatalf("EvictToStub truncated the Game Manager entry — it must refuse unmanaged folders")
	}
	if reason != "resolve" {
		t.Fatalf("refusal reason = %q, want %q", reason, "resolve")
	}
	got, err := os.ReadFile(entry)
	if err != nil || string(got) != string(body) {
		t.Fatalf("Game Manager entry bytes changed (err=%v)", err)
	}

	if id, ok := ResolveRomID(cfg, entry); ok {
		t.Fatalf("ResolveRomID resolved the Game Manager entry to rom_id %d — must not", id)
	}

	// The auto-launch m3u is equally untouchable (evict's multi-disc branch must not
	// fire for a folder the library does not own).
	m3u := filepath.Join(gmDir, "0) Game Manager (LODORGM).m3u")
	if err := os.WriteFile(m3u, []byte("Open Game Manager.gm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if evicted, reason = EvictToStub(cfg, m3u); evicted || reason != "resolve" {
		t.Fatalf("EvictToStub on the GM m3u: evicted=%v reason=%q, want refuse/resolve", evicted, reason)
	}
	if got, err = os.ReadFile(entry); err != nil || string(got) != string(body) {
		t.Fatalf("GM m3u evict attempt touched the entry file (err=%v)", err)
	}
}
