package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/romm"
)

// fakeMirrorClient is the full romClient (+GetPlatforms) the mirror needs, returning a
// fixed single-platform, single-rom library so move-awareness is testable offline.
type fakeMirrorClient struct {
	platforms []romm.Platform
	roms      []romm.Rom
}

func (f fakeMirrorClient) GetRoms(q romm.GetRomsQuery) (romm.PaginatedRoms, error) {
	return romm.PaginatedRoms{Items: f.roms}, nil
}
func (f fakeMirrorClient) GetCollections() ([]romm.Collection, error) { return nil, nil }
func (f fakeMirrorClient) DownloadCover(string) ([]byte, error)       { return nil, nil }
func (f fakeMirrorClient) GetPlatforms() ([]romm.Platform, error)     { return f.platforms, nil }

// TestMirrorMoveAware locks the NextUI folder-as-badge invariants:
//   - first mirror stubs into the "<Display> RomM (<TAG>)" cloud folder;
//   - a fetch-on-launch download that left a REAL file in the cloud folder is RELOCATED
//     to the on-device "<Display> (<TAG>)" twin on the next mirror (no re-stub, no dup);
//   - a re-mirror of an already-on-device game does NOT resurrect a cloud stub;
//   - ResolveRomID reverses BOTH the cloud path and the on-device path to the same id
//     (so list-saves / sync-save / download keep working from the moved file).
func TestMirrorMoveAware(t *testing.T) {
	sd := t.TempDir()
	t.Setenv("SDCARD_PATH", sd)
	t.Setenv("BASE_PATH", sd)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_HOST_OS", "nextui")
	// FC.pak present so the NES platform is launchable (mirror won't prune it).
	if err := os.MkdirAll(filepath.Join(sd, ".system", "tg5040", "paks", "Emus", "FC.pak"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		MirrorMode: config.MirrorModeSeparate,
		DirectoryMappings: map[string]config.DirMapping{
			"nes": {Slug: "nes", RelativePath: "Nintendo Entertainment System RomM (FC)"},
		},
	}
	client := fakeMirrorClient{
		platforms: []romm.Platform{{ID: 1, FsSlug: "nes", Name: "Nintendo Entertainment System", RomCount: 1}},
		roms:      []romm.Rom{{ID: 100, PlatformFsSlug: "nes", Files: []romm.RomFile{{FileName: "Super Mario Bros (USA).nes"}}}},
	}

	roms := filepath.Join(sd, "Roms")
	cloudPath := filepath.Join(roms, "Nintendo Entertainment System RomM (FC)", "Super Mario Bros (USA) (RomM).nes")
	onDevPath := filepath.Join(roms, "Nintendo Entertainment System (FC)", "Super Mario Bros (USA) (RomM).nes")

	// --- mirror #1: fresh -> a 0-byte cloud stub is created.
	created, _, _, _, _, err := MirrorCatalog(client, cfg, nil)
	if err != nil {
		t.Fatalf("mirror #1: %v", err)
	}
	if created != 1 {
		t.Fatalf("mirror #1 created=%d, want 1", created)
	}
	if fi, e := os.Stat(cloudPath); e != nil || fi.Size() != 0 {
		t.Fatalf("mirror #1: want 0-byte cloud stub at %q (err=%v)", cloudPath, e)
	}

	// Both the cloud path and the (future) on-device path resolve to the rom id.
	if id, ok := ResolveRomID(cfg, cloudPath); !ok || id != 100 {
		t.Fatalf("resolve cloud path: got (%d,%v), want (100,true)", id, ok)
	}
	if id, ok := ResolveRomID(cfg, onDevPath); !ok || id != 100 {
		t.Fatalf("resolve on-device path: got (%d,%v), want (100,true) — twin folder not resolved (saves would orphan)", id, ok)
	}

	// --- simulate a fetch-on-launch download IN PLACE (relocate suppressed): the cloud
	// stub becomes a REAL file still sitting in the cloud folder.
	if err := os.WriteFile(cloudPath, []byte("ROMDATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --- mirror #2: must RELOCATE the real file to the on-device folder, NOT re-stub.
	created2, existing2, _, _, _, err := MirrorCatalog(client, cfg, nil)
	if err != nil {
		t.Fatalf("mirror #2: %v", err)
	}
	if created2 != 0 {
		t.Errorf("mirror #2 created=%d, want 0 (downloaded game must not be re-stubbed)", created2)
	}
	if existing2 != 1 {
		t.Errorf("mirror #2 existing=%d, want 1", existing2)
	}
	if _, e := os.Stat(cloudPath); !os.IsNotExist(e) {
		t.Errorf("mirror #2: cloud-folder file should be GONE after relocate, stat err=%v", e)
	}
	if fi, e := os.Stat(onDevPath); e != nil || fi.Size() == 0 {
		t.Errorf("mirror #2: real file should now be in the on-device folder %q (err=%v)", onDevPath, e)
	}

	// --- mirror #3: already on device -> still no cloud stub resurrected.
	created3, _, _, _, _, err := MirrorCatalog(client, cfg, nil)
	if err != nil {
		t.Fatalf("mirror #3: %v", err)
	}
	if created3 != 0 {
		t.Errorf("mirror #3 created=%d, want 0", created3)
	}
	if _, e := os.Stat(cloudPath); !os.IsNotExist(e) {
		t.Errorf("mirror #3: cloud stub resurrected for a downloaded game: %q", cloudPath)
	}

	// Resolution still works from the on-device path after the move.
	if id, ok := ResolveRomID(cfg, onDevPath); !ok || id != 100 {
		t.Fatalf("post-move resolve on-device path: got (%d,%v), want (100,true)", id, ok)
	}
}
