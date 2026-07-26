package romm

// Contract tests for the managed sync endpoints (decision 2026-07-16), driven by a mock
// RomM mirroring the verified 5.0.0 wire shape from source (endpoints/sync.py):
// SyncNegotiatePayload / ClientSaveState in, SyncNegotiateResponse / SyncOperationSchema
// out, SyncCompletePayload in / SyncCompleteResponse out. RomM is unreachable from the
// build host, so this proves the WIRE SHAPE — field names, the required updated_at +
// file_size_bytes, the slot carried through — not live server semantics.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lodor/config"
)

// syncMock records the last negotiate/complete request bodies and serves canned
// responses plus a heartbeat version.
type syncMock struct {
	version      string
	lastNegReq   syncNegotiatePayload
	lastNegRaw   map[string]any
	lastCompReq  syncCompletePayload
	completePath string
}

func (m *syncMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"SYSTEM": map[string]string{"VERSION": m.version}})
	})
	mux.HandleFunc("/api/sync/negotiate", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &m.lastNegReq)
		_ = json.Unmarshal(raw, &m.lastNegRaw)
		_ = json.NewEncoder(w).Encode(SyncNegotiateResponse{
			SessionID: 349,
			Operations: []SyncOperation{
				{Action: "upload", RomID: 7, FileName: "G.sav", Reason: "client-only"},
				{Action: "download", RomID: 9, SaveID: ptrInt(52), FileName: "H.srm", Reason: "server-newer"},
				{Action: "no_op", RomID: 11, FileName: "I.sav", Reason: "in-sync"},
			},
			TotalUpload: 1, TotalDownload: 1, TotalConflict: 0, TotalNoOp: 1,
		})
	})
	mux.HandleFunc("/api/sync/sessions/349/complete", func(w http.ResponseWriter, r *http.Request) {
		m.completePath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &m.lastCompReq)
		_ = json.NewEncoder(w).Encode(syncCompleteResponse{Session: SyncSession{
			ID: 349, DeviceID: "dev-1", Status: "COMPLETED",
			OperationsPlanned: 2, OperationsCompleted: r2completed(m.lastCompReq), OperationsFailed: m.lastCompReq.OperationsFailed,
		}})
	})
	return httptest.NewServer(mux)
}

func r2completed(p syncCompletePayload) int { return p.OperationsCompleted }
func ptrInt(i int) *int                     { return &i }

func syncTestClient(url string) *Client {
	return NewClient(config.Host{RootURI: url, Token: "scopes:assets.read devices.read devices.write", DeviceID: "dev-1"}, 10*time.Second)
}

func TestSupportsManagedSync(t *testing.T) {
	for _, tc := range []struct {
		ver  string
		want bool
	}{{"5.0.0", true}, {"5.1.2", true}, {"4.9.2", false}, {"", false}, {"garbage", false}} {
		m := &syncMock{version: tc.ver}
		srv := m.server(t)
		c := syncTestClient(srv.URL)
		if got := c.SupportsManagedSync(); got != tc.want {
			t.Errorf("version %q: SupportsManagedSync=%v want %v", tc.ver, got, tc.want)
		}
		srv.Close()
	}
}

func TestNegotiateSendsInventoryAndDecodesPlan(t *testing.T) {
	m := &syncMock{version: "5.0.0"}
	srv := m.server(t)
	defer srv.Close()
	c := syncTestClient(srv.URL)

	inv := []ClientSaveState{{
		RomID: 7, FileName: "G.sav", Slot: "autosave", Emulator: "gpsp",
		ContentHash: "abc", UpdatedAt: time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC), FileSizeBytes: 2048,
	}}
	resp, err := c.Negotiate("dev-1", inv)
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}

	// Request shape: device_id + saves item carrying the required fields.
	if m.lastNegReq.DeviceID != "dev-1" {
		t.Errorf("device_id = %q want dev-1", m.lastNegReq.DeviceID)
	}
	if len(m.lastNegReq.Saves) != 1 || m.lastNegReq.Saves[0].RomID != 7 {
		t.Fatalf("saves not round-tripped: %+v", m.lastNegReq.Saves)
	}
	// The server 422s without updated_at / file_size_bytes — assert they are on the wire.
	saveRaw, _ := m.lastNegRaw["saves"].([]any)
	if len(saveRaw) != 1 {
		t.Fatalf("raw saves = %v", m.lastNegRaw["saves"])
	}
	item := saveRaw[0].(map[string]any)
	for _, k := range []string{"rom_id", "file_name", "updated_at", "file_size_bytes", "slot"} {
		if _, ok := item[k]; !ok {
			t.Errorf("negotiate save item missing required key %q (item=%v)", k, item)
		}
	}
	if item["slot"] != "autosave" {
		t.Errorf("slot on wire = %v want autosave (a null slot always negotiates as upload)", item["slot"])
	}

	// Response decode.
	if resp.SessionID != 349 || len(resp.Operations) != 3 || resp.TotalUpload != 1 || resp.TotalDownload != 1 {
		t.Fatalf("bad decode: %+v", resp)
	}
	if resp.Operations[1].SaveID == nil || *resp.Operations[1].SaveID != 52 {
		t.Errorf("download op save_id not decoded: %+v", resp.Operations[1])
	}
}

func TestNegotiateEmptyInventorySendsArrayNotNull(t *testing.T) {
	m := &syncMock{version: "5.0.0"}
	srv := m.server(t)
	defer srv.Close()
	c := syncTestClient(srv.URL)
	if _, err := c.Negotiate("dev-1", nil); err != nil {
		t.Fatalf("Negotiate(nil): %v", err)
	}
	if raw, ok := m.lastNegRaw["saves"]; !ok || raw == nil {
		t.Errorf("empty inventory must serialize as [] not null (got %v)", raw)
	}
}

func TestCompleteSyncSession(t *testing.T) {
	m := &syncMock{version: "5.0.0"}
	srv := m.server(t)
	defer srv.Close()
	c := syncTestClient(srv.URL)

	sess, err := c.CompleteSyncSession(349, 5, 1)
	if err != nil {
		t.Fatalf("CompleteSyncSession: %v", err)
	}
	if m.completePath != "/api/sync/sessions/349/complete" {
		t.Errorf("complete path = %q", m.completePath)
	}
	if m.lastCompReq.OperationsCompleted != 5 || m.lastCompReq.OperationsFailed != 1 {
		t.Errorf("complete counts on wire = %+v", m.lastCompReq)
	}
	if sess.Status != "COMPLETED" || sess.ID != 349 {
		t.Errorf("session decode = %+v", sess)
	}
}

func TestCompleteSyncSessionRejectsBadID(t *testing.T) {
	c := syncTestClient("http://127.0.0.1:0")
	if _, err := c.CompleteSyncSession(0, 0, 0); err == nil || !strings.Contains(err.Error(), "invalid session id") {
		t.Errorf("expected invalid-session-id error, got %v", err)
	}
}
