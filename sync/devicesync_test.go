package sync

// Best-effort / non-blocking proof for the device_save_sync integration (task #176) at
// the SYNC layer. The load-bearing guarantee: a failing or absent ledger endpoint NEVER
// turns a successful download into a failure, and the whole feature is gated off below
// RomM 4.9.0. A mock RomM lets us assert the wire behavior without a live server.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"lodor/config"
	"lodor/romm"
)

type ledgerMock struct {
	mu               sync.Mutex
	version          string
	downloadedStatus int
	downloadedHits   int
}

func (m *ledgerMock) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fmt.Sprintf(`{"SYSTEM":{"VERSION":%q}}`, m.version))
	})
	mux.HandleFunc("GET /api/saves/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("SAVEBYTES"))
	})
	mux.HandleFunc("POST /api/saves/{id}/downloaded", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.downloadedHits++
		st := m.downloadedStatus
		m.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			_, _ = io.WriteString(w, `{"detail":"boom"}`)
			return
		}
		id, _ := strconv.Atoi(r.PathValue("id"))
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%d,"rom_id":7}`, id))
	})
	return httptest.NewServer(mux)
}

func cfgFor(url string) *config.Config {
	return &config.Config{Hosts: []config.Host{{RootURI: url, Token: "scopes:" + "assets.read,assets.write,devices.read,devices.write", DeviceID: "dev-1"}}}
}

func hits(m *ledgerMock) int { m.mu.Lock(); defer m.mu.Unlock(); return m.downloadedHits }

// A 500 on POST /downloaded must NEVER fail the download: writeSave still returns
// PullWritten with the bytes on disk, and the confirm was attempted (then swallowed).
func TestWriteSaveDownloadedConfirmIsBestEffort(t *testing.T) {
	m := &ledgerMock{version: "4.9.2", downloadedStatus: http.StatusInternalServerError}
	srv := m.server()
	defer srv.Close()
	cfg := cfgFor(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	local := filepath.Join(t.TempDir(), "game.sav")
	res := writeSave(c, cfg, 100, local)
	if res.Outcome != PullWritten {
		t.Fatalf("a 500 on /downloaded must not fail the write; got outcome %v reason=%q", res.Outcome, res.Reason)
	}
	if b, err := os.ReadFile(local); err != nil || string(b) != "SAVEBYTES" {
		t.Fatalf("save bytes not written: %q err=%v", string(b), err)
	}
	if hits(m) != 1 {
		t.Fatalf("confirm should have been attempted exactly once, got %d", hits(m))
	}
}

// Below 4.9.0 the confirm must NOT be attempted at all (the endpoint isn't there), and
// the download still succeeds.
func TestWriteSaveConfirmGatedOffBelow490(t *testing.T) {
	m := &ledgerMock{version: "4.8.0"}
	srv := m.server()
	defer srv.Close()
	cfg := cfgFor(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	local := filepath.Join(t.TempDir(), "game.sav")
	res := writeSave(c, cfg, 100, local)
	if res.Outcome != PullWritten {
		t.Fatalf("write should succeed regardless of version; got %v", res.Outcome)
	}
	if hits(m) != 0 {
		t.Fatalf("confirm must be gated off below 4.9.0, but it was called %d time(s)", hits(m))
	}
}

// The explicit track/untrack user control is gated off below 4.9.0 too — it reports
// reason=unsupported without ever hitting the wire (so it can't 404-spam an old server).
func TestTrackSaveGatedOffBelow490(t *testing.T) {
	m := &ledgerMock{version: "4.8.0"}
	srv := m.server()
	defer srv.Close()
	cfg := cfgFor(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	res := TrackSaveForRom(c, cfg, "/roms/GBA/whatever.gba")
	if res.OK || res.Reason != "unsupported" {
		t.Fatalf("track below 4.9.0 should report unsupported, got %+v", res)
	}
}
