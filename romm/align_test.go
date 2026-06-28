package romm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lodor/config"
)

func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClient(config.Host{RootURI: srv.URL}, 10*time.Second)
}

// TestReportPlaySessions locks the POST /api/play-sessions wire shape against the
// server's pydantic model: a top-level device_id plus a sessions[] batch whose entries
// carry rom_id, RFC3339 start_time/end_time, and duration_ms (milliseconds).
func TestReportPlaySessions(t *testing.T) {
	var gotBody PlaySessionIngestPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/play-sessions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(PlaySessionIngestResponse{CreatedCount: 1, SkippedCount: 0})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	entry := PlaySessionEntry{
		RomID:      42,
		StartTime:  "2026-06-28T01:00:00Z",
		EndTime:    "2026-06-28T01:30:00Z",
		DurationMs: 1800000,
	}
	resp, err := c.ReportPlaySessions("dev-123", []PlaySessionEntry{entry})
	if err != nil {
		t.Fatalf("ReportPlaySessions error: %v", err)
	}
	if resp.CreatedCount != 1 {
		t.Errorf("CreatedCount = %d, want 1", resp.CreatedCount)
	}
	if gotBody.DeviceID != "dev-123" {
		t.Errorf("device_id = %q, want dev-123", gotBody.DeviceID)
	}
	if len(gotBody.Sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(gotBody.Sessions))
	}
	s := gotBody.Sessions[0]
	if s.RomID != 42 || s.DurationMs != 1800000 || s.StartTime != entry.StartTime || s.EndTime != entry.EndTime {
		t.Errorf("session round-trip mismatch: %+v", s)
	}
}

// TestReportPlaySessions404 verifies an older RomM without the endpoint surfaces a
// *StatusError{Code:404} so the caller can no-op instead of failing.
func TestReportPlaySessions404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := testClient(t, srv)
	_, err := c.ReportPlaySessions("d", []PlaySessionEntry{{RomID: 1, StartTime: "x", EndTime: "y", DurationMs: 1}})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("want *StatusError code 404, got %v", err)
	}
}

// TestGetVirtualAndSmartCollections checks the optional shelf endpoints: virtual sends
// the required type param and both parse name+rom_ids; a 404 surfaces as StatusError.
func TestGetVirtualAndSmartCollections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/collections/virtual":
			if r.URL.Query().Get("type") != "all" {
				t.Errorf("virtual type = %q, want all", r.URL.Query().Get("type"))
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "Action", "rom_ids": []int{1, 2}},
			})
		case "/api/collections/smart":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "Favorites", "rom_ids": []int{3}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv)

	v, err := c.GetVirtualCollections("all")
	if err != nil {
		t.Fatalf("GetVirtualCollections: %v", err)
	}
	if len(v) != 1 || v[0].Name != "Action" || len(v[0].RomIDs) != 2 {
		t.Errorf("virtual parse mismatch: %+v", v)
	}
	sm, err := c.GetSmartCollections()
	if err != nil {
		t.Fatalf("GetSmartCollections: %v", err)
	}
	if len(sm) != 1 || sm[0].Name != "Favorites" || len(sm[0].RomIDs) != 1 {
		t.Errorf("smart parse mismatch: %+v", sm)
	}
}

// TestResumeDownload206 verifies a partial file is resumed via a Range request: the
// server returns only the remaining bytes (206) and the engine appends them, yielding
// the complete file.
func TestResumeDownload206(t *testing.T) {
	full := []byte("0123456789ABCDEF")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(full)
			return
		}
		// parse "bytes=N-"
		var start int
		fmt.Sscanf(rng, "bytes=%d-", &start)
		if start >= len(full) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		rest := full[start:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(full)-1, len(full)))
		w.Header().Set("Content-Length", strconv.Itoa(len(rest)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(rest)
	}))
	defer srv.Close()
	c := testClient(t, srv)

	dir := t.TempDir()
	tmp := filepath.Join(dir, "rom.bin.tmp")
	if err := os.WriteFile(tmp, full[:6], 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.DownloadRomContentResumeTo(7, "rom.bin", f, 6, nil)
	f.Close()
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n != int64(len(full)) {
		t.Errorf("total bytes = %d, want %d", n, len(full))
	}
	got, _ := os.ReadFile(tmp)
	if string(got) != string(full) {
		t.Errorf("resumed file = %q, want %q", got, full)
	}
}

// TestResumeDownload200FullRewrite verifies that when the server ignores Range and
// returns the whole body (200), the engine discards the partial and rewrites a clean
// file (no doubled/garbled bytes).
func TestResumeDownload200FullRewrite(t *testing.T) {
	full := []byte("HELLO-WORLD-DATA")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK) // ignore Range entirely
		_, _ = w.Write(full)
	}))
	defer srv.Close()
	c := testClient(t, srv)

	dir := t.TempDir()
	tmp := filepath.Join(dir, "rom.bin.tmp")
	// stale partial of wrong content/size
	if err := os.WriteFile(tmp, []byte("XXXXX"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY, 0o644)
	n, err := c.DownloadRomContentResumeTo(7, "rom.bin", f, 5, nil)
	f.Close()
	if err != nil {
		t.Fatalf("resume-to-full: %v", err)
	}
	if n != int64(len(full)) {
		t.Errorf("bytes = %d, want %d", n, len(full))
	}
	got, _ := os.ReadFile(tmp)
	if string(got) != string(full) {
		t.Errorf("rewritten file = %q, want %q", got, full)
	}
}

// TestResumeDownload416SelfHeal verifies a stale partial at/beyond EOF (server replies
// 416) self-heals: the engine truncates and re-fetches the whole file from byte 0.
func TestResumeDownload416SelfHeal(t *testing.T) {
	full := []byte("ABCDEFGHIJ")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng != "" {
			var start int
			fmt.Sscanf(rng, "bytes=%d-", &start)
			if start >= len(full) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full)
	}))
	defer srv.Close()
	c := testClient(t, srv)

	dir := t.TempDir()
	tmp := filepath.Join(dir, "rom.bin.tmp")
	// partial LARGER than the real file -> offset beyond EOF -> 416
	if err := os.WriteFile(tmp, []byte(strings.Repeat("Z", 99)), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY, 0o644)
	n, err := c.DownloadRomContentResumeTo(7, "rom.bin", f, 99, nil)
	f.Close()
	if err != nil {
		t.Fatalf("416 self-heal: %v", err)
	}
	if n != int64(len(full)) {
		t.Errorf("bytes = %d, want %d", n, len(full))
	}
	got, _ := os.ReadFile(tmp)
	if string(got) != string(full) {
		t.Errorf("healed file = %q, want %q", got, full)
	}
}
