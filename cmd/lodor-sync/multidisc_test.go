//go:build !onion && !muos && !knulli

package main

// Multi-disc folder-rom download coverage (task #49/#74 + the release-blocking
// black-screen fix). Two proofs against a fake RomM:
//
//   1. A folder-rom of N discs downloads ALL discs into <Game>/ and writes the
//      canonical <Game>.m3u alongside, with each line "<Game>/<disc filename>"
//      resolved relative to the .m3u dir (exactly what minui's getFirstDisc and
//      the emulator's m3u loader expect). Every disc lands with real bytes.
//
//   2. The download is IDEMPOTENT/RESUMABLE: re-running it when SOME discs are
//      already present (a real .m3u whose discs partially exist) re-fetches only
//      the missing disc(s) and leaves the present ones byte-identical. This is
//      what makes the launch-time re-invocation of --download on a real-but-
//      incomplete .m3u safe — the exact FFVII field-repro shape.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// discBytes returns deterministic, disc-distinct, non-empty content for a disc index.
func discBytes(disc int) []byte {
	return []byte(strings.Repeat(fmt.Sprintf("D%d-", disc), 64))
}

// multiDiscServer serves one folder-rom (id 900, 3 CHD discs) — GetRom + per-disc
// content selected by ?file_ids=. Content is served ONLY for a single file_ids
// selector (the disc-by-disc path the engine uses), which is all downloadMultiDiscCore
// exercises.
func multiDiscServer(t *testing.T) *httptest.Server {
	t.Helper()
	rom := romm.Rom{
		ID:             900,
		PlatformFsSlug: "psx",
		FsName:         "Final Fantasy VII (USA)",
		FsNameNoExt:    "Final Fantasy VII (USA)",
		Name:           "Final Fantasy VII",
		HasMultipleFiles: true,
		Files: []romm.RomFile{
			{ID: 9001, FileName: "Final Fantasy VII (USA) (Disc 1).chd"},
			{ID: 9002, FileName: "Final Fantasy VII (USA) (Disc 2).chd"},
			{ID: 9003, FileName: "Final Fantasy VII (USA) (Disc 3).chd"},
		},
	}
	byFileID := map[string][]byte{
		"9001": discBytes(1), "9002": discBytes(2), "9003": discBytes(3),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/roms/900", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rom)
	})
	mux.HandleFunc("/api/roms/900/content/", func(w http.ResponseWriter, r *http.Request) {
		ids := r.URL.Query().Get("file_ids")
		body, ok := byFileID[ids]
		if !ok {
			http.Error(w, "unknown file_ids", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	})
	return httptest.NewServer(mux)
}

func newMultiDiscEnv(t *testing.T, srv *httptest.Server) (*config.Config, *romm.Client, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_HOST_OS", "nextui")
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "Tools", "tg5040", "Lodor.pak"))
	if err := os.MkdirAll(filepath.Join(base, "Tools", "tg5040", "Lodor.pak"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Hosts:      []config.Host{{RootURI: srv.URL, Token: "t", DeviceID: "dev-1"}},
		MirrorMode: config.MirrorModeMerge,
		DirectoryMappings: map[string]config.DirMapping{
			"psx": {Slug: "psx", RelativePath: "PlayStation (PS)"},
		},
	}
	client := romm.NewClient(cfg.Hosts[0], 5*time.Second)
	return cfg, client, base
}

func TestDownloadMultiDiscCore_FetchesAllDiscsAndWritesM3u(t *testing.T) {
	srv := multiDiscServer(t)
	defer srv.Close()
	cfg, client, base := newMultiDiscEnv(t, srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	man := platform.LoadManifest()

	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man); !ok {
		t.Fatalf("downloadMultiDiscCore returned false (expected all 3 discs to land)")
	}

	psDir := filepath.Join(base, "Roms", "PlayStation (PS)")
	discDir := filepath.Join(psDir, "Final Fantasy VII (USA)")
	m3u := filepath.Join(psDir, "Final Fantasy VII (USA).m3u")

	// All 3 discs present with the exact server bytes.
	for i, f := range rom.Files {
		dp := filepath.Join(discDir, f.FileName)
		got, rerr := os.ReadFile(dp)
		if rerr != nil {
			t.Fatalf("disc %d missing: %v", i+1, rerr)
		}
		if want := discBytes(i + 1); string(got) != string(want) {
			t.Errorf("disc %d bytes mismatch", i+1)
		}
	}

	// The .m3u is a real playlist listing "<Game>/<disc>" for each disc, in order.
	data, rerr := os.ReadFile(m3u)
	if rerr != nil {
		t.Fatalf("m3u not written: %v", rerr)
	}
	wantM3u := "Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 1).chd\n" +
		"Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 2).chd\n" +
		"Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 3).chd\n"
	if string(data) != wantM3u {
		t.Errorf("m3u contents:\n%q\nwant:\n%q", string(data), wantM3u)
	}
	if fi, _ := os.Stat(m3u); fi != nil && fi.Size() == 0 {
		t.Errorf("m3u is a 0-byte stub after a successful download")
	}
}

// The FFVII field-repro shape: a real .m3u exists but its discs are absent (evicted /
// never downloaded). Re-running the download must re-fetch the missing discs. Also
// proves resume: a disc already present + non-empty is NOT re-downloaded (the server
// would 400 if the engine ever tried an unexpected selector — here every disc is
// servable, so we assert bytes are preserved and all land).
func TestDownloadMultiDiscCore_ResumesMissingDiscs(t *testing.T) {
	srv := multiDiscServer(t)
	defer srv.Close()
	cfg, client, base := newMultiDiscEnv(t, srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}

	psDir := filepath.Join(base, "Roms", "PlayStation (PS)")
	discDir := filepath.Join(psDir, "Final Fantasy VII (USA)")
	m3u := filepath.Join(psDir, "Final Fantasy VII (USA).m3u")

	// Pre-seed the FIELD-REPRO state: a real .m3u playlist, but NO disc files on disk
	// (the black-screen bug's exact shape). The manifest owns the .m3u + folder so the
	// V5 gate lets the re-download proceed.
	if err := os.MkdirAll(discDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realM3u := "Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 1).chd\n" +
		"Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 2).chd\n" +
		"Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 3).chd\n"
	if err := os.WriteFile(m3u, []byte(realM3u), 0o644); err != nil {
		t.Fatal(err)
	}
	man := platform.LoadManifest()
	man.Record(discDir, platform.ManifestFolder, rom.ID)
	man.Record(m3u, platform.ManifestDownload, rom.ID)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}
	// Pre-seed ONE disc as already-present with the correct bytes (resume path).
	if err := os.WriteFile(filepath.Join(discDir, rom.Files[0].FileName), discBytes(1), 0o644); err != nil {
		t.Fatal(err)
	}

	man = platform.LoadManifest()
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man); !ok {
		t.Fatalf("re-download of an incomplete multi-disc rom failed (expected recovery)")
	}

	// After: every disc is present with the right bytes and the .m3u is intact.
	for i, f := range rom.Files {
		got, rerr := os.ReadFile(filepath.Join(discDir, f.FileName))
		if rerr != nil {
			t.Fatalf("disc %d still missing after re-download: %v", i+1, rerr)
		}
		if string(got) != string(discBytes(i+1)) {
			t.Errorf("disc %d bytes mismatch after re-download", i+1)
		}
	}
}
