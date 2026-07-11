//go:build !onion && !muos && !knulli && !android && !lodorandroid

package main

// Multi-disc folder-rom download coverage — DISC-1-FIRST (lodor#7 hybrid) plus the
// original task #49/#74 shapes. Proofs against a fake RomM:
//
//   1. LAUNCH PATH (budget=1): a fresh N-disc game downloads ONLY disc 1, writes the
//      FULL .m3u, and leaves discs 2..N as honest 0-byte stubs (fast time-to-play,
//      card space paid per disc reached).
//   2. NEXT-DISC ORDERING: with disc 1 present, the next budget=1 run fetches disc 2
//      (m3u order), never re-pulling a verified disc (idempotent per-disc resume —
//      asserted by counting server hits).
//   3. FETCH-ALL (budget<0, --fetch-discs / daemon prefetch): completes the whole
//      set; from fresh it downloads every disc (the pre-#7 behavior, still needed).
//   4. RESUME AFTER INTERRUPT: a failed disc-2 transfer KEEPS the landed disc 1
//      (pre-#7 the whole folder was RemoveAll'd) and a later run completes the set
//      without re-downloading disc 1.
//   5. OFFLINE COMPLETENESS CENSUS: catalog.M3UCompleteness + the manifest walk
//      (IncompleteMultiDiscDownloads) that --check-rom and --prefetch-discs ride.
//   6. EVICT ON PARTIAL: "Delete from card" on a disc-1-only game removes the real
//      disc AND the stubs, leaving the canonical 0-byte cloud stub shape.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lodor/catalog"
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// discBytes returns deterministic, disc-distinct, non-empty content for a disc index.
func discBytes(disc int) []byte {
	return []byte(strings.Repeat(fmt.Sprintf("D%d-", disc), 64))
}

// mdServer wraps the fake RomM so tests can count per-disc content requests,
// force per-disc transfer failures (500 or a mid-stream cut), and inspect the
// Range headers the engine sent (the per-disc resume contract).
type mdServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   map[string]int      // file_ids selector -> content request count
	fail   map[string]bool     // file_ids selector -> serve a 500
	cut    map[string]bool     // file_ids selector -> send half the body, then abort
	ranges map[string][]string // file_ids selector -> Range header per request ("" = none)
}

func (m *mdServer) hitCount(fileID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[fileID]
}

func (m *mdServer) setFail(fileID string, fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail[fileID] = fail
}

func (m *mdServer) setCut(fileID string, cut bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cut[fileID] = cut
}

func (m *mdServer) rangeHeaders(fileID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.ranges[fileID]...)
}

// multiDiscServer serves one folder-rom (id 900, 3 CHD discs) — GetRom + per-disc
// content selected by ?file_ids=. Content is served ONLY for a single file_ids
// selector (the disc-by-disc path the engine uses), which is all downloadMultiDiscCore
// exercises.
func multiDiscServer(t *testing.T) *mdServer {
	t.Helper()
	rom := romm.Rom{
		ID:               900,
		PlatformFsSlug:   "psx",
		FsName:           "Final Fantasy VII (USA)",
		FsNameNoExt:      "Final Fantasy VII (USA)",
		Name:             "Final Fantasy VII",
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
	ms := &mdServer{hits: map[string]int{}, fail: map[string]bool{}, cut: map[string]bool{}, ranges: map[string][]string{}}
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
		ms.mu.Lock()
		ms.hits[ids]++
		ms.ranges[ids] = append(ms.ranges[ids], r.Header.Get("Range"))
		failNow := ms.fail[ids]
		cutNow := ms.cut[ids]
		ms.mu.Unlock()
		if failNow {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if cutNow {
			// Mid-stream interrupt: promise the full length, send half (flushed past
			// the server's buffer so the client really receives it), then abort the
			// connection — the client's io.Copy errors with a partial on disk.
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = w.Write(body[:len(body)/2])
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		// Honor a single "bytes=N-" range like RomM's FileResponse: 206 with the
		// remainder. An at/past-EOF offset is a 416; anything unparseable falls back
		// to the full 200 body (the shape doRawStreamResumeTo self-heals around).
		if rng := r.Header.Get("Range"); rng != "" {
			var off int
			if n, _ := fmt.Sscanf(rng, "bytes=%d-", &off); n == 1 && off >= 0 {
				if off >= len(body) {
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(body)-off))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(body[off:])
				return
			}
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	})
	ms.srv = httptest.NewServer(mux)
	return ms
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

// mdPaths returns the canonical card paths for the fixture game.
func mdPaths(base string) (psDir, discDir, m3u string) {
	psDir = filepath.Join(base, "Roms", "PlayStation (PS)")
	discDir = filepath.Join(psDir, "Final Fantasy VII (USA)")
	m3u = filepath.Join(psDir, "Final Fantasy VII (USA).m3u")
	return
}

const fullM3U = "Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 1).chd\n" +
	"Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 2).chd\n" +
	"Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 3).chd\n"

// assertDisc checks one disc file's exact state: "real" (fixture bytes), "stub"
// (exists, 0 bytes) or "absent".
func assertDisc(t *testing.T, discDir string, rom romm.Rom, i int, want string) {
	t.Helper()
	p := filepath.Join(discDir, rom.Files[i].FileName)
	fi, err := os.Stat(p)
	switch want {
	case "absent":
		if err == nil {
			t.Errorf("disc %d: expected absent, found %d bytes", i+1, fi.Size())
		}
	case "stub":
		if err != nil {
			t.Errorf("disc %d: expected 0-byte stub, got stat error %v", i+1, err)
		} else if fi.Size() != 0 {
			t.Errorf("disc %d: expected 0-byte stub, has %d bytes", i+1, fi.Size())
		}
	case "real":
		got, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Errorf("disc %d: expected real bytes, got %v", i+1, rerr)
		} else if string(got) != string(discBytes(i+1)) {
			t.Errorf("disc %d: bytes mismatch", i+1)
		}
	}
}

// TestDownloadMultiDiscCore_Disc1FirstLaunch is the lodor#7 core proof: the launch
// path (budget=1) on a fresh game lands ONLY disc 1, writes the FULL .m3u, stubs
// discs 2/3, and reports honest total/present/fetched accounting.
func TestDownloadMultiDiscCore_Disc1FirstLaunch(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	man := platform.LoadManifest()
	var st discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, 1, &st); !ok {
		t.Fatalf("downloadMultiDiscCore(budget=1) returned false (expected disc 1 to land)")
	}

	_, discDir, m3u := mdPaths(base)
	assertDisc(t, discDir, rom, 0, "real")
	assertDisc(t, discDir, rom, 1, "stub")
	assertDisc(t, discDir, rom, 2, "stub")

	data, rerr := os.ReadFile(m3u)
	if rerr != nil {
		t.Fatalf("m3u not written: %v", rerr)
	}
	if string(data) != fullM3U {
		t.Errorf("m3u contents:\n%q\nwant FULL playlist:\n%q", string(data), fullM3U)
	}
	if st.total != 3 || st.present != 1 || st.fetched != 1 || !st.multi {
		t.Errorf("stats = %+v, want {multi:true total:3 present:1 fetched:1}", st)
	}
	if st.complete() {
		t.Errorf("stats claim complete with 2 discs missing")
	}
	// Exactly ONE content transfer: disc 1. Discs 2/3 were stubbed, not fetched.
	for id, want := range map[string]int{"9001": 1, "9002": 0, "9003": 0} {
		if got := ms.hitCount(id); got != want {
			t.Errorf("server hits for file %s = %d, want %d", id, got, want)
		}
	}
}

// TestDownloadMultiDiscCore_NextDiscOrdering: with disc 1 present behind a real
// .m3u, the next budget=1 run fetches disc 2 (m3u order), and the one after that
// disc 3 — never re-pulling a verified disc. This is exactly the hooks'
// --fetch-next-disc relaunch loop.
func TestDownloadMultiDiscCore_NextDiscOrdering(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	_, discDir, _ := mdPaths(base)

	// Launch 1: disc 1 lands.
	man := platform.LoadManifest()
	var st1 discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, 1, &st1); !ok {
		t.Fatalf("launch 1 failed")
	}
	// Launch 2 (re-trigger): disc 2 lands, disc 3 still a stub.
	man = platform.LoadManifest()
	var st2 discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, 1, &st2); !ok {
		t.Fatalf("launch 2 failed")
	}
	assertDisc(t, discDir, rom, 0, "real")
	assertDisc(t, discDir, rom, 1, "real")
	assertDisc(t, discDir, rom, 2, "stub")
	if st2.present != 2 || st2.fetched != 1 {
		t.Errorf("launch 2 stats = %+v, want present:2 fetched:1", st2)
	}
	// Launch 3: disc 3 completes the set.
	man = platform.LoadManifest()
	var st3 discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, 1, &st3); !ok {
		t.Fatalf("launch 3 failed")
	}
	assertDisc(t, discDir, rom, 2, "real")
	if !st3.complete() || st3.fetched != 1 {
		t.Errorf("launch 3 stats = %+v, want complete fetched:1", st3)
	}
	// Launch 4 (idempotent): nothing to fetch, still complete.
	man = platform.LoadManifest()
	var st4 discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, 1, &st4); !ok {
		t.Fatalf("idempotent relaunch failed")
	}
	if !st4.complete() || st4.fetched != 0 {
		t.Errorf("idempotent stats = %+v, want complete fetched:0", st4)
	}
	// Every disc transferred exactly once across all four runs.
	for _, id := range []string{"9001", "9002", "9003"} {
		if got := ms.hitCount(id); got != 1 {
			t.Errorf("server hits for file %s = %d, want exactly 1 (per-disc resume)", id, got)
		}
	}
}

// TestDownloadMultiDiscCore_FetchAllCompletes: budget<0 (--fetch-discs / daemon
// prefetch) downloads every missing disc in one call — from fresh, all of them.
func TestDownloadMultiDiscCore_FetchAllCompletes(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	man := platform.LoadManifest()
	var st discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, -1, &st); !ok {
		t.Fatalf("fetch-all failed")
	}
	_, discDir, m3u := mdPaths(base)
	for i := range rom.Files {
		assertDisc(t, discDir, rom, i, "real")
	}
	if data, _ := os.ReadFile(m3u); string(data) != fullM3U {
		t.Errorf("m3u not the full playlist after fetch-all")
	}
	if !st.complete() || st.fetched != 3 {
		t.Errorf("stats = %+v, want complete fetched:3", st)
	}
}

// TestDownloadMultiDiscCore_ResumeAfterInterrupt: a failed disc-2 transfer keeps the
// landed disc 1 (pre-lodor#7 the whole folder was RemoveAll'd — the regression this
// guards) and a later run completes the set WITHOUT re-downloading disc 1.
func TestDownloadMultiDiscCore_ResumeAfterInterrupt(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	_, discDir, m3u := mdPaths(base)

	// Interrupt: disc 2's transfer 500s mid-batch on a fetch-all run.
	ms.setFail("9002", true)
	man := platform.LoadManifest()
	var st discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, -1, &st); ok {
		t.Fatalf("fetch-all with a failing disc reported success")
	}
	assertDisc(t, discDir, rom, 0, "real") // the landed disc STAYS
	if _, serr := os.Stat(m3u); serr == nil {
		t.Errorf("m3u written despite the failed run (no stub existed before)")
	}
	if st.present != 1 || st.fetched != 1 {
		t.Errorf("interrupted stats = %+v, want present:1 fetched:1", st)
	}

	// Recovery: server healthy again — the set completes, disc 1 never re-pulled.
	ms.setFail("9002", false)
	man = platform.LoadManifest()
	var st2 discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, -1, &st2); !ok {
		t.Fatalf("recovery run failed")
	}
	for i := range rom.Files {
		assertDisc(t, discDir, rom, i, "real")
	}
	if !st2.complete() || st2.fetched != 2 {
		t.Errorf("recovery stats = %+v, want complete fetched:2", st2)
	}
	if got := ms.hitCount("9001"); got != 1 {
		t.Errorf("disc 1 transferred %d times, want exactly 1 (resume keeps verified discs)", got)
	}
}

// TestMultiDiscCompletenessCensus: the OFFLINE detection the hooks (--check-rom) and
// daemons (--prefetch-discs) ride — M3UCompleteness on the file shape, and the
// manifest walk finding exactly the downloaded-but-incomplete games.
func TestMultiDiscCompletenessCensus(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	_, _, m3u := mdPaths(base)

	// Nothing downloaded yet: the census finds no work (a 0-byte stub m3u is the
	// LAUNCH path's job, not the prefetcher's).
	man := platform.LoadManifest()
	man.Record(m3u, platform.ManifestStub, rom.ID)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}
	if inc := catalog.IncompleteMultiDiscDownloads(); len(inc) != 0 {
		t.Errorf("census on an empty card found %d games, want 0", len(inc))
	}

	// Disc-1-first launch: now the census must find one game, 2 discs missing.
	man = platform.LoadManifest()
	var st discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, 1, &st); !ok {
		t.Fatalf("launch download failed")
	}
	total, present := catalog.M3UCompleteness(m3u)
	if total != 3 || present != 1 {
		t.Errorf("M3UCompleteness = %d/%d, want 1/3 present", present, total)
	}
	inc := catalog.IncompleteMultiDiscDownloads()
	if len(inc) != 1 || inc[0].Total != 3 || inc[0].Present != 1 || inc[0].Path != m3u {
		t.Errorf("census = %+v, want the one incomplete game 1/3 at %s", inc, m3u)
	}

	// Complete the set: the census drains to empty (the daemon goes quiet).
	man = platform.LoadManifest()
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, -1, &st); !ok {
		t.Fatalf("completion failed")
	}
	if inc := catalog.IncompleteMultiDiscDownloads(); len(inc) != 0 {
		t.Errorf("census still reports %d games after completion, want 0", len(inc))
	}
	if tot, pres := catalog.M3UCompleteness(m3u); tot != 3 || pres != 3 {
		t.Errorf("M3UCompleteness after completion = %d/%d, want 3/3", pres, tot)
	}
}

// TestEvictPartialMultiDisc: "Delete from card" on a disc-1-only game removes the
// real disc AND the 0-byte stubs, drops the per-game folder, and leaves the m3u as
// the canonical 0-byte cloud stub — the disc-1-first partial state must never wedge
// the evict path (evictDiscFiles reads the m3u, which lists ALL discs).
func TestEvictPartialMultiDisc(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	man := platform.LoadManifest()
	var st discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, 1, &st); !ok {
		t.Fatalf("launch download failed")
	}
	_, discDir, m3u := mdPaths(base)

	evicted, reason := catalog.EvictToStub(cfg, m3u)
	if !evicted {
		t.Fatalf("evict of a partial multi-disc failed: reason=%s", reason)
	}
	// Every disc file (real + stubs) is gone and the folder fell with them.
	for i := range rom.Files {
		assertDisc(t, discDir, rom, i, "absent")
	}
	if _, serr := os.Stat(discDir); serr == nil {
		t.Errorf("disc folder still present after evict")
	}
	// A 0-byte .m3u stub remains (possibly under the flipped cloud-marker name).
	psDir := filepath.Dir(m3u)
	entries, _ := os.ReadDir(psDir)
	found := false
	for _, e := range entries {
		if strings.EqualFold(filepath.Ext(e.Name()), ".m3u") {
			if fi, ferr := e.Info(); ferr == nil && fi.Size() == 0 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no 0-byte .m3u stub left after evict")
	}
}
