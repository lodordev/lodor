//go:build !onion && !muos && !knulli && !android && !lodorandroid

package main

// Mode-flip migration tests (C2 §6): the Smart Pro field-mess cleanup — the
// "(RomM)"-twin beside the user's (✓-renamed) real file — plus the prompt-only
// guard and the push-before-remove gate.

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

var twinSaveBytes = []byte("TWIN-SAVE-LINEAGE")

// migrateServer serves the minimal RomM API the merge migration touches: the
// platform list (mapping regeneration), the Emerald rom, and a save list that
// already holds the twin save's content hash (so the funnel dedups — the
// AlreadyOnServer outcome — without needing the upload endpoint).
func migrateServer(t *testing.T) *httptest.Server {
	t.Helper()
	h := md5.Sum(twinSaveBytes)
	hash := hex.EncodeToString(h[:])
	rom := romm.Rom{
		ID: 100, PlatformFsSlug: "gba",
		FsName: "Pokemon - Emerald Version (USA, Europe).gba", FsNameNoExt: "Pokemon - Emerald Version (USA, Europe)",
		Files: []romm.RomFile{{FileName: "Pokemon - Emerald Version (USA, Europe).gba"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/platforms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]romm.Platform{{ID: 1, FsSlug: "gba", Name: "Game Boy Advance"}})
	})
	mux.HandleFunc("/api/roms/100", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rom)
	})
	mux.HandleFunc("/api/saves", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]romm.Save{{
			ID: 1, RomID: 100, FileName: "Pokemon - Emerald Version (USA, Europe).gba.sav",
			FileSizeBytes: int64(len(twinSaveBytes)), ContentHash: &hash,
			UpdatedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		}})
	})
	return httptest.NewServer(mux)
}

// migrateEnv builds the Smart Pro card shape: own-layout mapping, the user's
// ✓-renamed real Emerald + the "(RomM)" stub twin (each with a save), and a
// catalog index carrying the SEPARATE-era keys.
func migrateEnv(t *testing.T, srvURL string) (*romm.Client, *config.Config, string, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_HOST_OS", "nextui")
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))
	t.Chdir(mkTestDir(t, base, "pak"))
	mkTestDir(t, base, ".system/tg5040/paks/Emus/GBA.pak")

	dir := mkTestDir(t, base, "Roms/Game Boy Advance (GBA)")
	survivor := filepath.Join(dir, platform.MarkerOnDevice+"Pokemon - Emerald Version (USA, Europe).gba")
	if err := os.WriteFile(survivor, []byte("USER EMERALD ROM"), 0o644); err != nil {
		t.Fatal(err)
	}
	twin := filepath.Join(dir, platform.MarkerCloud+"Pokemon - Emerald Version (USA, Europe) (RomM).gba")
	if err := os.WriteFile(twin, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	saveDir := mkTestDir(t, base, "Saves/GBA")
	if err := os.WriteFile(filepath.Join(saveDir, filepath.Base(twin)+".sav"), twinSaveBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	idx := `{"version":1,"platforms":{"gba":{` +
		`"by_basename":{"Pokemon - Emerald Version (USA, Europe) (RomM)":100},` +
		`"by_fsname":{"Pokemon - Emerald Version (USA, Europe).gba":100},"by_id":{}}}}`
	if err := os.WriteFile(filepath.Join(base, "pak", "catalog-index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Hosts:      []config.Host{{RootURI: srvURL, Token: "t", DeviceID: "dev-1"}},
		MirrorMode: config.MirrorModeMerge, // the pak consent prompt wrote it
		DirectoryMappings: map[string]config.DirMapping{
			"gba": {Slug: "gba", RelativePath: "Game Boy Advance (GBA)"},
		},
	}
	client := romm.NewClient(cfg.Hosts[0], 5*time.Second)
	return client, cfg, survivor, twin
}

func mkTestDir(t *testing.T, base, rel string) string {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMigrateTwinCleanup is the Smart Pro shape end to end: explicit merge over a
// card carrying "✓ Emerald.gba" (the user's V4-renamed real file) + "✘ Emerald
// (RomM).gba" (a 0-byte stub twin with its own save). The twin's save content is
// already on the server (verified funnel dedups), so the stub twin is removed,
// its save lineage survives — renamed to the survivor (which had none) — and the
// user's file is byte-identical.
func TestMigrateTwinCleanup(t *testing.T) {
	srv := migrateServer(t)
	defer srv.Close()
	client, cfg, survivor, twin := migrateEnv(t, srv.URL)

	migrateMirrorLayoutIfNeeded(client, cfg)

	if _, err := os.Stat(twin); !os.IsNotExist(err) {
		t.Fatalf("stub twin not removed: %v", err)
	}
	data, err := os.ReadFile(survivor)
	if err != nil || string(data) != "USER EMERALD ROM" {
		t.Fatalf("survivor modified (err=%v data=%q)", err, data)
	}
	// Save continuity: the twin's save now wears the survivor's basename.
	migrated := filepath.Join(os.Getenv("SDCARD_PATH"), "Saves", "GBA", filepath.Base(survivor)+".sav")
	got, serr := os.ReadFile(migrated)
	if serr != nil || string(got) != string(twinSaveBytes) {
		t.Fatalf("twin save lineage lost (err=%v data=%q)", serr, got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("SDCARD_PATH"), "Saves", "GBA", filepath.Base(twin)+".sav")); !os.IsNotExist(err) {
		t.Fatalf("twin save left orphaned under the old name")
	}
	// Manifest records the migrated mode — the next mirror won't re-migrate.
	if man := platform.LoadManifest(); man.Mode != config.MirrorModeMerge {
		t.Fatalf("manifest mode = %q, want merge", man.Mode)
	}
}

// TestMigrateDefersWhenPushCannotLand: the twin's save content is UNKNOWN to the
// server and the upload endpoint fails — push-before-remove must DEFER: twin,
// save, everything byte-identical.
func TestMigrateDefersWhenPushCannotLand(t *testing.T) {
	// Same server but the save list is empty and there's no upload route: the
	// funnel can neither dedup nor land the push.
	rom := romm.Rom{
		ID: 100, PlatformFsSlug: "gba",
		FsName: "Pokemon - Emerald Version (USA, Europe).gba",
		Files:  []romm.RomFile{{FileName: "Pokemon - Emerald Version (USA, Europe).gba"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/platforms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]romm.Platform{{ID: 1, FsSlug: "gba", Name: "Game Boy Advance"}})
	})
	mux.HandleFunc("/api/roms/100", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rom)
	})
	mux.HandleFunc("/api/saves", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode([]romm.Save{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client, cfg, survivor, twin := migrateEnv(t, srv.URL)

	migrateMirrorLayoutIfNeeded(client, cfg)

	if _, err := os.Stat(twin); err != nil {
		t.Fatalf("twin removed despite unpushed save (push-before-remove violated): %v", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("SDCARD_PATH"), "Saves", "GBA", filepath.Base(twin)+".sav")); err != nil {
		t.Fatalf("twin save touched despite deferred migration: %v", err)
	}
	if data, _ := os.ReadFile(survivor); string(data) != "USER EMERALD ROM" {
		t.Fatalf("survivor modified: %q", data)
	}
}

// TestMigratePromptOnly: a DEFAULTED merge (no explicit mirror_mode) must never
// restructure the card — nothing moves, nothing is removed.
func TestMigratePromptOnly(t *testing.T) {
	srv := migrateServer(t)
	defer srv.Close()
	client, cfg, _, twin := migrateEnv(t, srv.URL)
	cfg.MirrorMode = "" // defaulted (nextui -> merge), NOT explicit

	migrateMirrorLayoutIfNeeded(client, cfg)

	if _, err := os.Stat(twin); err != nil {
		t.Fatalf("defaulted mode migrated the card (prompt-only violated): %v", err)
	}
}
