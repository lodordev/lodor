package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/platform"
)

// TestUninstallMirrorLeavesUserTreeByteIdentical (C2 §5): after uninstall, every
// mirror artifact is gone (stubs, our cover, our collection, our folder), the
// downloads are KEPT by default, saves are untouched, and every user file is
// byte-identical. map.txt survives even when a lying manifest claims it.
func TestUninstallMirrorLeavesUserTreeByteIdentical(t *testing.T) {
	cfg, rom, unmarked := markerTestCfg(t)
	base := os.Getenv("SDCARD_PATH")
	userDir := filepath.Dir(unmarked) // Roms/GBA — the user's (adopted) folder
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// User content.
	userRom := filepath.Join(userDir, "Their Game.gba")
	if err := os.WriteFile(userRom, []byte("THEIRS"), 0o644); err != nil {
		t.Fatal(err)
	}
	userArt := filepath.Join(userDir, ".media", "Their Game.png")
	if err := os.MkdirAll(filepath.Dir(userArt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userArt, []byte("USERPNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	colDir := filepath.Join(base, "Collections")
	if err := os.MkdirAll(colDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userCol := filepath.Join(colDir, "My Favorites.txt")
	if err := os.WriteFile(userCol, []byte("/Roms/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mapTxt := filepath.Join(colDir, "map.txt")
	if err := os.WriteFile(mapTxt, []byte("A\tB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	save := filepath.Join(platform.SaveDirectory("gba"), platform.MarkerOnDevice+filepath.Base(unmarked)+".sav")
	if err := os.MkdirAll(filepath.Dir(save), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(save, []byte("SAVE"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mirror artifacts, manifest-recorded as the mirror would.
	man := platform.LoadManifest()
	stub := filepath.Join(userDir, platform.MarkerCloud+filepath.Base(unmarked))
	if err := os.WriteFile(stub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	man.Record(stub, platform.ManifestStub, rom.ID)
	dl := filepath.Join(userDir, platform.MarkerOnDevice+"Got Game.gba")
	if err := os.WriteFile(dl, []byte("DOWNLOADED"), 0o644); err != nil {
		t.Fatal(err)
	}
	man.Record(dl, platform.ManifestDownload, 99)
	ourCover := filepath.Join(userDir, ".media", platform.MarkerCloud+"Zeta.png")
	if err := os.WriteFile(ourCover, []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	man.Record(ourCover, platform.ManifestCover, 7)
	ourFolder := filepath.Join(base, "Roms", "Nintendo 64 (N64)")
	if err := os.MkdirAll(ourFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	man.Record(ourFolder, platform.ManifestFolder, 0)
	n64Stub := filepath.Join(ourFolder, platform.MarkerCloud+"Mario 64.z64")
	if err := os.WriteFile(n64Stub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	man.Record(n64Stub, platform.ManifestStub, 5)
	ourCol := filepath.Join(colDir, "Server List.txt")
	if err := os.WriteFile(ourCol, []byte("/Roms/y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	man.Record(ourCol, platform.ManifestCollection, 0)
	man.Record(mapTxt, platform.ManifestCollection, 0) // the LYING claim — hard exclude must hold
	// A stub that silently became real (state drift): must be SKIPPED, not deleted.
	drift := filepath.Join(userDir, platform.MarkerCloud+"Drifted.gba")
	if err := os.WriteFile(drift, []byte("REAL NOW"), 0o644); err != nil {
		t.Fatal(err)
	}
	man.Record(drift, platform.ManifestStub, 6)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	res := UninstallMirror(cfg, false)
	if !res.Ok {
		t.Fatal("uninstall reported not-ok with a valid manifest")
	}

	// Mirror artifacts gone.
	for _, p := range []string{stub, ourCover, n64Stub, ourFolder, ourCol} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("mirror artifact survived uninstall: %s", p)
		}
	}
	// Downloads kept by default; drifted stub skipped.
	if data, err := os.ReadFile(dl); err != nil || string(data) != "DOWNLOADED" {
		t.Errorf("download not kept (err=%v)", err)
	}
	if res.KeptDownloads != 1 {
		t.Errorf("kept_downloads=%d want 1", res.KeptDownloads)
	}
	if data, err := os.ReadFile(drift); err != nil || string(data) != "REAL NOW" {
		t.Errorf("drifted stub deleted despite real bytes (err=%v)", err)
	}
	if res.Skipped == 0 {
		t.Errorf("drifted stub not counted skipped")
	}
	// User tree byte-identical; saves untouched; map.txt intact.
	for p, want := range map[string]string{
		userRom: "THEIRS", userArt: "USERPNG", userCol: "/Roms/x\n", mapTxt: "A\tB\n", save: "SAVE",
	} {
		data, err := os.ReadFile(p)
		if err != nil || string(data) != want {
			t.Errorf("user/save file %s changed (err=%v data=%q)", filepath.Base(p), err, data)
		}
	}
	// Engine state retired.
	if _, err := os.Stat(platform.ManifestPath()); !os.IsNotExist(err) {
		t.Errorf("manifest survived uninstall")
	}
	if _, err := os.Stat(IndexPath(cfg)); !os.IsNotExist(err) {
		t.Errorf("catalog index survived uninstall")
	}
}

// TestUninstallRemoveDownloads: the explicit second confirmation deletes the
// downloads too — including a multi-disc game's disc files — but still never a
// user file or a save.
func TestUninstallRemoveDownloads(t *testing.T) {
	cfg, _, unmarked := markerTestCfg(t)
	userDir := filepath.Dir(unmarked)
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	man := platform.LoadManifest()

	dl := filepath.Join(userDir, platform.MarkerOnDevice+"Got Game.gba")
	if err := os.WriteFile(dl, []byte("DOWNLOADED"), 0o644); err != nil {
		t.Fatal(err)
	}
	man.Record(dl, platform.ManifestDownload, 99)
	// Multi-disc download in the PARTIAL disc-1-first shape (lodor#7): disc 1 has
	// real bytes, disc 2 is the engine's honest 0-byte stub — uninstall must sweep
	// both (a partial set is a first-class downloaded state, not corruption).
	discDir := filepath.Join(userDir, "Chrono (USA)")
	if err := os.MkdirAll(discDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(discDir, "disc1.chd"), []byte("D1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(discDir, "disc2.chd"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m3u := filepath.Join(userDir, platform.MarkerOnDevice+"Chrono (USA).m3u")
	if err := os.WriteFile(m3u, []byte("Chrono (USA)/disc1.chd\nChrono (USA)/disc2.chd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	man.Record(m3u, platform.ManifestDownload, 44)
	man.Record(discDir, platform.ManifestFolder, 44)
	userRom := filepath.Join(userDir, "Their Game.gba")
	if err := os.WriteFile(userRom, []byte("THEIRS"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	res := UninstallMirror(cfg, true)
	if !res.Ok || res.KeptDownloads != 0 {
		t.Fatalf("res=%+v want ok, no kept downloads", res)
	}
	for _, p := range []string{dl, m3u, discDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("download artifact survived --remove-downloads: %s", p)
		}
	}
	if data, err := os.ReadFile(userRom); err != nil || string(data) != "THEIRS" {
		t.Errorf("user file touched (err=%v)", err)
	}
}

// TestUninstallRefusesWithoutManifest: ownership unknowable ⇒ remove nothing.
func TestUninstallRefusesWithoutManifest(t *testing.T) {
	cfg, _, unmarked := markerTestCfg(t)
	dir := filepath.Dir(unmarked)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, platform.MarkerCloud+filepath.Base(unmarked))
	if err := os.WriteFile(stub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := UninstallMirror(cfg, true)
	if res.Ok || res.Removed != 0 {
		t.Fatalf("manifest-less uninstall = %+v, want refuse/no-op", res)
	}
	if _, err := os.Stat(stub); err != nil {
		t.Fatalf("manifest-less uninstall deleted a file: %v", err)
	}
}
