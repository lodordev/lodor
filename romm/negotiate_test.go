package romm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lodor/config"
)

// realNegotiateBody is a raw fixture matching the live RomM 4.9.2 negotiate response
// shape (verified 2026-06-28): an INTEGER session_id, the per-op key "action" with the
// upload/download/conflict/no_op vocabulary, each op carrying slot/emulator/
// server_updated_at/server_content_hash, and the response total_* counters. Decoding
// this proves the wire tags on NegotiateResponse/SyncOp match the server.
const realNegotiateBody = `{
  "session_id": 42,
  "total_uploads": 1,
  "total_downloads": 1,
  "total_conflicts": 1,
  "total_no_ops": 1,
  "operations": [
    {"action": "download", "rom_id": 1, "save_id": 7, "slot": "autosave", "emulator": "mgba", "server_updated_at": "2026-06-28T12:00:00Z", "server_content_hash": "deadbeef"},
    {"action": "upload", "rom_id": 2, "slot": "autosave", "emulator": "mgba"},
    {"action": "conflict", "rom_id": 3, "reason": "both moved"},
    {"action": "no_op", "rom_id": 4}
  ]
}`

// TestNegotiateWire verifies the request is POSTed with the device_id + saves body and
// that the raw live-shape response decodes into NegotiateResponse with the corrected
// tags: int session_id, "action" vocabulary, the extra per-op fields, and total_*.
func TestNegotiateWire(t *testing.T) {
	var gotBody NegotiateRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sync/negotiate" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, realNegotiateBody)
	}))
	defer ts.Close()

	c := NewClient(config.Host{RootURI: ts.URL, DeviceID: "dev1"}, 5*time.Second)
	resp, err := c.Negotiate(NegotiateRequest{
		DeviceID: "dev1",
		Saves:    []NegotiateSaveRef{{RomID: 1, Hash: "abc", Emulator: "mgba", Slot: "autosave"}},
	})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if gotBody.DeviceID != "dev1" || len(gotBody.Saves) != 1 || gotBody.Saves[0].Hash != "abc" {
		t.Errorf("request body not sent correctly: %+v", gotBody)
	}

	// Integer session id (was a string in the pre-fix code, which failed to unmarshal).
	if resp.SessionID != 42 {
		t.Errorf("session_id decode wrong: got %d want 42", resp.SessionID)
	}
	// Server total_* counters.
	if resp.TotalUploads != 1 || resp.TotalDownloads != 1 || resp.TotalConflicts != 1 || resp.TotalNoOps != 1 {
		t.Errorf("total_* counters decode wrong: %+v", resp)
	}
	if len(resp.Operations) != 4 {
		t.Fatalf("operations decode wrong: %+v", resp.Operations)
	}
	// Each "action" maps to its const.
	wantOps := []string{SyncOpDownload, SyncOpUpload, SyncOpConflict, SyncOpNoOp}
	for i, want := range wantOps {
		if resp.Operations[i].Op != want {
			t.Errorf("op[%d].action = %q want %q", i, resp.Operations[i].Op, want)
		}
	}
	// The download op's extra fields decode.
	dl := resp.Operations[0]
	if dl.SaveID != 7 || dl.Slot != "autosave" || dl.Emulator != "mgba" || dl.ServerContentHash != "deadbeef" {
		t.Errorf("download op extra fields wrong: %+v", dl)
	}
	if dl.ServerUpdatedAt.IsZero() {
		t.Errorf("server_updated_at did not decode: %+v", dl)
	}
}

// TestCompleteSyncSessionWire verifies the completion path is hit with the integer
// session id rendered into the path. There is no DELETE for sync sessions.
func TestCompleteSyncSessionWire(t *testing.T) {
	var hitPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	c := NewClient(config.Host{RootURI: ts.URL}, 5*time.Second)
	if err := c.CompleteSyncSession(42); err != nil {
		t.Fatalf("CompleteSyncSession: %v", err)
	}
	if hitPath != "/api/sync/sessions/42/complete" {
		t.Errorf("completion path = %q", hitPath)
	}
}

// TestGetHeartbeatCapabilities verifies the heartbeat -> version -> capabilities path
// against a representative nested body.
func TestGetHeartbeatCapabilities(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/heartbeat" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"system":{"version":"4.9.0"}}`))
	}))
	defer ts.Close()

	c := NewClient(config.Host{RootURI: ts.URL}, 5*time.Second)
	hb, err := c.GetHeartbeat()
	if err != nil {
		t.Fatalf("GetHeartbeat: %v", err)
	}
	if hb.Version.String() != "4.9.0" {
		t.Errorf("version = %s", hb.Version)
	}
	if !hb.Capabilities.SupportsSyncNegotiate || !hb.Capabilities.TrustsServerHash {
		t.Errorf("4.9.0 caps wrong: %+v", hb.Capabilities)
	}
	if hb.Capabilities.SupportsDeviceAuth {
		t.Errorf("4.9.0 must NOT have device-auth")
	}
}
