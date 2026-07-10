// align_salvage_test.go — re-land coverage for the b1342a5 salvage (play-sessions +
// resumable downloads) adapted to today's engine: the resolved-ActiveHost device_id,
// the keep-partial-.tmp-on-error contract, and the Range resume through the hardened
// client path. Reuses the roundtrip e2e scaffolding (same package): e2eEnv + md5hex.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"lodor/catalog"
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// salvageMock is a minimal RomM: one GBA platform, one rom, with a mode-switchable
// content endpoint (truncate = die mid-stream after partialLen bytes; range = honest
// 200/206/416 Range support) and an optional play-sessions sink.
type salvageMock struct {
	mu         sync.Mutex
	rom        romm.Rom
	full       []byte
	partialLen int
	mode       string // "truncate" | "range"
	lastRange  string
	psDevice   string
	psHits     int
	playOK     bool // register POST /api/play-sessions
}

func (m *salvageMock) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"SYSTEM":{"VERSION":"4.9.2"}}`)
	})
	mux.HandleFunc("GET /api/platforms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]romm.Platform{{ID: 3, FsSlug: "gba", Name: "Game Boy Advance", RomCount: 1}})
	})
	mux.HandleFunc("GET /api/roms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(romm.PaginatedRoms{Items: []romm.Rom{m.rom}, Total: 1, Limit: 250})
	})
	mux.HandleFunc("GET /api/roms/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(m.rom)
	})
	mux.HandleFunc("GET /api/collections", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("GET /api/roms/{id}/content/{fsname}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		mode := m.mode
		m.lastRange = r.Header.Get("Range")
		m.mu.Unlock()
		if mode == "truncate" {
			// Declare the full length but stop mid-body: the client sees a dropped
			// transfer (unexpected EOF / short read), exactly like a Wi-Fi cutout.
			w.Header().Set("Content-Length", strconv.Itoa(len(m.full)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(m.full[:m.partialLen])
			return
		}
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(m.full)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(m.full)
			return
		}
		var start int
		_, _ = fmt.Sscanf(rng, "bytes=%d-", &start)
		if start >= len(m.full) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		rest := m.full[start:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(m.full)-1, len(m.full)))
		w.Header().Set("Content-Length", strconv.Itoa(len(rest)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(rest)
	})
	if m.playOK {
		mux.HandleFunc("POST /api/play-sessions", func(w http.ResponseWriter, r *http.Request) {
			var body romm.PlaySessionIngestPayload
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.psDevice = body.DeviceID
			m.psHits++
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(romm.PlaySessionIngestResponse{CreatedCount: 1})
		})
	}
	return httptest.NewServer(mux)
}

func newSalvageMock(full []byte) *salvageMock {
	return &salvageMock{
		full:       full,
		partialLen: 100,
		mode:       "range",
		rom: romm.Rom{
			ID: 7, PlatformID: 3, PlatformFsSlug: "gba",
			FsName: "Metroid (USA).gba", FsNameNoExt: "Metroid (USA)",
			Name: "Metroid", Md5Hash: md5hex(full),
			Files: []romm.RomFile{{ID: 71, FileName: "Metroid (USA).gba", Md5Hash: md5hex(full)}},
		},
	}
}

// mirrorOneStub mirrors the mock's library and returns the on-card stub path.
func mirrorOneStub(t *testing.T, client *romm.Client, cfg *config.Config, rom romm.Rom) string {
	t.Helper()
	created, _, _, _, _, _, err := catalog.MirrorCatalog(client, cfg, &catalog.Reporter{}, false)
	if err != nil || created != 1 {
		t.Fatalf("mirror: created=%d err=%v (want 1 stub)", created, err)
	}
	return platform.LocalRomPath(cfg, rom)
}

// TestDownloadKeepsPartialTmpThenResumes is the re-land's .tmp contract: a transfer
// that dies mid-stream KEEPS the partial .tmp (no delete), and the next --download of
// the same rom resumes from that offset via a Range request (206 append), landing a
// hash-verified complete file and clearing the .tmp.
func TestDownloadKeepsPartialTmpThenResumes(t *testing.T) {
	full := []byte(strings.Repeat("RESUME-BYTES-", 200))
	m := newSalvageMock(full)
	m.mode = "truncate"
	srv := m.server()
	defer srv.Close()
	cfg, client, _ := e2eEnv(t, srv.URL)
	stubPath := mirrorOneStub(t, client, cfg, m.rom)

	// 1) Interrupted transfer: must FAIL and must KEEP the partial .tmp.
	if downloadRomCore(client, cfg, stubPath) {
		t.Fatalf("interrupted download reported success")
	}
	tmp := stubPath + ".tmp"
	fi, err := os.Stat(tmp)
	if err != nil {
		t.Fatalf("partial .tmp deleted on transfer error (want kept for resume): %v", err)
	}
	if fi.Size() != int64(m.partialLen) {
		t.Fatalf(".tmp size = %d, want the %d partial bytes", fi.Size(), m.partialLen)
	}

	// 2) Retry with a healthy server: must resume FROM the partial's offset (Range
	// header), append the remainder, verify the hash, and land the real file.
	m.mu.Lock()
	m.mode = "range"
	m.mu.Unlock()
	if !downloadRomCore(client, cfg, stubPath) {
		t.Fatalf("resume download failed")
	}
	m.mu.Lock()
	gotRange := m.lastRange
	m.mu.Unlock()
	if want := fmt.Sprintf("bytes=%d-", m.partialLen); gotRange != want {
		t.Errorf("resume Range = %q, want %q (did not resume from the partial)", gotRange, want)
	}
	got, _ := os.ReadFile(stubPath)
	if string(got) != string(full) {
		t.Fatalf("resumed file mismatch: %d bytes, want %d", len(got), len(full))
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf(".tmp still present after a landed download (err=%v)", err)
	}
}

// TestReportSessionCoreWindowAndDeviceID: (a) a non-positive window is rejected LOCALLY
// (no request spent — the server 422s end<=start, we never send it); (b) a valid session
// posts a single-entry batch attributed to the RESOLVED host's device_id — the ActiveHost
// adaptation (main moved off Hosts[0] in 4f8a96e). The always-exit-0 contract lives in
// runReportSession (unconditional os.Exit(0) on both branches; binary-level smoke in the
// release gate) — the core is exit-free by construction, which this drives directly.
func TestReportSessionCoreWindowAndDeviceID(t *testing.T) {
	full := []byte("ROMBYTES")
	m := newSalvageMock(full)
	m.playOK = true
	srv := m.server()
	defer srv.Close()
	cfg, client, _ := e2eEnv(t, srv.URL)
	stubPath := mirrorOneStub(t, client, cfg, m.rom)

	host := config.Host{RootURI: srv.URL, Token: "t", DeviceID: "dev-42"}

	// (a) end == start -> local refusal, zero requests.
	if reportSessionCore(client, host, cfg, stubPath, 1000, 1000) {
		t.Fatalf("end==start window accepted (server would 422)")
	}
	m.mu.Lock()
	hits := m.psHits
	m.mu.Unlock()
	if hits != 0 {
		t.Fatalf("bad window still spent %d requests, want 0", hits)
	}

	// (b) valid window -> reported, attributed to the resolved host's device.
	if !reportSessionCore(client, host, cfg, stubPath, 1000, 1600) {
		t.Fatalf("valid session not reported")
	}
	m.mu.Lock()
	dev, hits := m.psDevice, m.psHits
	m.mu.Unlock()
	if hits != 1 || dev != "dev-42" {
		t.Fatalf("hits=%d device_id=%q, want 1 report attributed to dev-42", hits, dev)
	}
}

// TestReportSessionCore404Graceful: an older RomM without /api/play-sessions (404)
// degrades to reported=false with no error escalation — the PSSKIP no-op path.
func TestReportSessionCore404Graceful(t *testing.T) {
	full := []byte("ROMBYTES")
	m := newSalvageMock(full) // playOK=false: POST /api/play-sessions -> 404
	srv := m.server()
	defer srv.Close()
	cfg, client, _ := e2eEnv(t, srv.URL)
	stubPath := mirrorOneStub(t, client, cfg, m.rom)

	host := config.Host{RootURI: srv.URL, Token: "t", DeviceID: "dev-42"}
	if reportSessionCore(client, host, cfg, stubPath, 1000, 1600) {
		t.Fatalf("404 endpoint reported success")
	}
}
