package romm

// Contract tests for the device_save_sync integration (task #176), driven by a MOCK
// RomM that mirrors the verified 4.9.x wire shape: heartbeat version, device-attributed
// save upload/download with SCOPE ENFORCEMENT (403 without devices.*), the
// track/untrack/downloaded ledger endpoints (incl. the track-can't-bootstrap asymmetry
// and untrack-creates-the-row), the 409 stale-upload interlock, and the saves summary.
//
// RomM is unreachable from the build host, so this proves the WIRE SHAPE and the
// non-blocking contract at the client layer — not a live round-trip. A live 4.9.x RomM
// is still the only thing that can confirm real server semantics.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lodor/config"
)

// ---- mock RomM ------------------------------------------------------------------------

type syncState struct {
	exists     bool
	tracked    bool
	lastSynced time.Time
}

type mockRomM struct {
	mu       sync.Mutex
	version  string
	saves    map[int]Save
	rows     map[string]syncState // key "<saveID>|<deviceID>"
	nextID   int
	updated  time.Time
	conflict bool // next upload 409s unless overwrite=true
	// call knobs / counters
	downloadedStatus int // if non-zero, /downloaded returns this status (best-effort test)
	downloadedHits   int
	trackHits        int
}

func newMock(version string) *mockRomM {
	return &mockRomM{
		version: version,
		saves:   map[int]Save{},
		rows:    map[string]syncState{},
		nextID:  100,
		updated: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
	}
}

func rowKey(saveID int, dev string) string { return fmt.Sprintf("%d|%s", saveID, dev) }

func (m *mockRomM) row(saveID int, dev string) syncState { return m.rows[rowKey(saveID, dev)] }

func scopesOf(r *http.Request) map[string]bool {
	auth := r.Header.Get("Authorization")
	auth = strings.TrimPrefix(auth, "Bearer ")
	auth = strings.TrimPrefix(auth, "scopes:")
	set := map[string]bool{}
	for _, s := range strings.Split(auth, ",") {
		if s = strings.TrimSpace(s); s != "" {
			set[s] = true
		}
	}
	return set
}

func deny(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `{"detail":"insufficient permissions"}`)
}

func requireScopes(w http.ResponseWriter, r *http.Request, need ...string) bool {
	have := scopesOf(r)
	for _, n := range need {
		if !have[n] {
			deny(w)
			return false
		}
	}
	return true
}

func writeSaveJSON(w http.ResponseWriter, s Save) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s)
}

func (m *mockRomM) server() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"SYSTEM":{"VERSION":%q}}`, m.version))
	})

	// Upload: device-attributed (device_id in query) → needs assets.write + devices.write.
	mux.HandleFunc("POST /api/saves", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "assets.write", "devices.write") {
			return
		}
		q := r.URL.Query()
		dev := q.Get("device_id")
		overwrite := q.Get("overwrite") == "true"
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.conflict && !overwrite {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"conflict","message":"newer server save","save_id":100}`)
			return
		}
		romID, _ := strconv.Atoi(q.Get("rom_id"))
		id := m.nextID
		m.nextID++
		hash := "abc123"
		s := Save{ID: id, RomID: romID, FileName: q.Get("file_name"), UpdatedAt: m.updated, ContentHash: &hash}
		m.saves[id] = s
		// UPSERT the device_save_sync row (the base upload's citizen behavior).
		m.rows[rowKey(id, dev)] = syncState{exists: true, tracked: true, lastSynced: m.updated}
		writeSaveJSON(w, s)
	})

	// List saves for a rom → assets.read.
	mux.HandleFunc("GET /api/saves", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "assets.read") {
			return
		}
		romID, _ := strconv.Atoi(r.URL.Query().Get("rom_id"))
		m.mu.Lock()
		defer m.mu.Unlock()
		var out []Save
		for _, s := range m.saves {
			if romID == 0 || s.RomID == romID {
				out = append(out, s)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	// Summary → assets.read.
	mux.HandleFunc("GET /api/saves/summary", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "assets.read") {
			return
		}
		romID, _ := strconv.Atoi(r.URL.Query().Get("rom_id"))
		m.mu.Lock()
		defer m.mu.Unlock()
		var latest Save
		count := 0
		for _, s := range m.saves {
			if s.RomID == romID {
				count++
				if s.UpdatedAt.After(latest.UpdatedAt) {
					latest = s
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SavesSummary{
			TotalCount: count,
			Slots:      []SaveSlotSummary{{Slot: "autosave", Count: count, Latest: latest}},
		})
	})

	// Download content: device-attributed → assets.read + devices.read. optimistic=true
	// upserts the row; optimistic=false does NOT.
	mux.HandleFunc("GET /api/saves/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "assets.read", "devices.read") {
			return
		}
		id, _ := strconv.Atoi(r.PathValue("id"))
		dev := r.URL.Query().Get("device_id")
		optimistic := r.URL.Query().Get("optimistic") == "true"
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := m.saves[id]; !ok {
			http.NotFound(w, r)
			return
		}
		if optimistic {
			m.rows[rowKey(id, dev)] = syncState{exists: true, tracked: true, lastSynced: m.updated}
		}
		_, _ = w.Write([]byte("SAVEBYTES"))
	})

	// track → devices.write; only UPDATES an existing row (404 if none — can't bootstrap).
	mux.HandleFunc("POST /api/saves/{id}/track", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "devices.write") {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.trackHits++
		id, dev, s, ok := m.decodeSaveAction(w, r)
		if !ok {
			return
		}
		cur := m.rows[rowKey(id, dev)]
		if !cur.exists {
			http.NotFound(w, r) // asymmetry: track cannot create a row
			return
		}
		cur.tracked = true
		m.rows[rowKey(id, dev)] = cur
		writeSaveJSON(w, s)
	})

	// untrack → devices.write; CREATES the row if absent.
	mux.HandleFunc("POST /api/saves/{id}/untrack", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "devices.write") {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		id, dev, s, ok := m.decodeSaveAction(w, r)
		if !ok {
			return
		}
		m.rows[rowKey(id, dev)] = syncState{exists: true, tracked: false, lastSynced: m.updated}
		writeSaveJSON(w, s)
	})

	// downloaded → devices.write; upserts. A test knob can force a non-2xx for the
	// best-effort proof.
	mux.HandleFunc("POST /api/saves/{id}/downloaded", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "devices.write") {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.downloadedHits++
		if m.downloadedStatus != 0 {
			w.WriteHeader(m.downloadedStatus)
			_, _ = io.WriteString(w, `{"detail":"boom"}`)
			return
		}
		id, dev, s, ok := m.decodeSaveAction(w, r)
		if !ok {
			return
		}
		m.rows[rowKey(id, dev)] = syncState{exists: true, tracked: true, lastSynced: m.updated}
		writeSaveJSON(w, s)
	})

	return httptest.NewServer(mux)
}

// decodeSaveAction reads {device_id} from the body and looks up the save; it writes a
// 422/404 and returns ok=false on a bad body / missing save. Caller holds m.mu.
func (m *mockRomM) decodeSaveAction(w http.ResponseWriter, r *http.Request) (int, string, Save, bool) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var body struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DeviceID == "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return 0, "", Save{}, false
	}
	s, ok := m.saves[id]
	if !ok {
		http.NotFound(w, r)
		return 0, "", Save{}, false
	}
	return id, body.DeviceID, s, true
}

func testClient(t *testing.T, url, scopes string) *Client {
	t.Helper()
	return NewClient(config.Host{RootURI: url, Token: "scopes:" + scopes, DeviceID: "dev-1"}, 10*time.Second)
}

const fullScopes = "assets.read,assets.write,devices.read,devices.write"

// ---- tests ----------------------------------------------------------------------------

func TestUploadUpsertsSyncRowWithScopes(t *testing.T) {
	m := newMock("4.9.2")
	srv := m.server()
	defer srv.Close()
	c := testClient(t, srv.URL, fullScopes)

	tmp := filepath.Join(t.TempDir(), "game.sav")
	if err := os.WriteFile(tmp, []byte("SAVEBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved, err := c.UploadSave(UploadSaveQuery{RomID: 7, DeviceID: "dev-1", FileName: "game.sav"}, tmp)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if saved.ID == 0 {
		t.Fatalf("no save id returned")
	}
	// (a) with correct scopes the sync row is written.
	if row := m.row(saved.ID, "dev-1"); !row.exists || !row.tracked {
		t.Fatalf("upload did not upsert a tracked sync row: %+v", row)
	}
}

func TestScopeTrap403WithoutDeviceScopes(t *testing.T) {
	m := newMock("4.9.2")
	srv := m.server()
	defer srv.Close()
	// A token WITHOUT devices.* — the exact latent-403 the scope trap warns about.
	c := testClient(t, srv.URL, "assets.read,assets.write")

	tmp := filepath.Join(t.TempDir(), "game.sav")
	_ = os.WriteFile(tmp, []byte("x"), 0o644)
	if _, err := c.UploadSave(UploadSaveQuery{RomID: 7, DeviceID: "dev-1", FileName: "game.sav"}, tmp); err == nil {
		t.Fatalf("device-attributed upload without devices.write should 403, got nil")
	}
	// And it must NOT be classified as a pairing-expired auth error (it's a scope 403).
	if _, err := c.DownloadSaveContent(100, "dev-1", false); err == nil {
		t.Fatalf("device-attributed download without devices.read should fail")
	} else if IsAuthError(err) {
		t.Fatalf("scope 403 must not read as an auth/pairing-expired error: %v", err)
	}
}

func TestDownloadOptimisticFalseThenDownloadedConfirms(t *testing.T) {
	m := newMock("4.9.2")
	srv := m.server()
	defer srv.Close()
	c := testClient(t, srv.URL, fullScopes)
	m.saves[100] = Save{ID: 100, RomID: 7, UpdatedAt: m.updated}

	// optimistic=false must NOT write the row (our real download semantics).
	if _, err := c.DownloadSaveContent(100, "dev-1", false); err != nil {
		t.Fatalf("download: %v", err)
	}
	if row := m.row(100, "dev-1"); row.exists {
		t.Fatalf("optimistic=false must not upsert the sync row, got %+v", row)
	}
	// The explicit /downloaded confirm then writes it.
	if _, err := c.MarkSaveDownloaded(100, "dev-1"); err != nil {
		t.Fatalf("downloaded: %v", err)
	}
	if row := m.row(100, "dev-1"); !row.exists || !row.tracked {
		t.Fatalf("/downloaded did not upsert the sync row: %+v", row)
	}
}

func TestTrackAsymmetryAndUntrackCreates(t *testing.T) {
	m := newMock("4.9.2")
	srv := m.server()
	defer srv.Close()
	c := testClient(t, srv.URL, fullScopes)
	m.saves[100] = Save{ID: 100, RomID: 7, UpdatedAt: m.updated}

	// track on a save with NO existing row → 404 (cannot bootstrap).
	if _, err := c.TrackSave(100, "dev-1"); err == nil {
		t.Fatalf("track with no existing row should 404, got nil")
	}
	if m.row(100, "dev-1").exists {
		t.Fatalf("track must NOT create a row")
	}
	// untrack creates the row.
	if _, err := c.UntrackSave(100, "dev-1"); err != nil {
		t.Fatalf("untrack: %v", err)
	}
	row := m.row(100, "dev-1")
	if !row.exists || row.tracked {
		t.Fatalf("untrack should create an is_untracked row: %+v", row)
	}
	// now track succeeds (row exists) and flips it back to tracked.
	if _, err := c.TrackSave(100, "dev-1"); err != nil {
		t.Fatalf("track after untrack: %v", err)
	}
	if row := m.row(100, "dev-1"); !row.tracked {
		t.Fatalf("track should set tracked=true: %+v", row)
	}
}

func TestUploadConflict409(t *testing.T) {
	m := newMock("4.9.2")
	m.conflict = true
	srv := m.server()
	defer srv.Close()
	c := testClient(t, srv.URL, fullScopes)
	tmp := filepath.Join(t.TempDir(), "g.sav")
	_ = os.WriteFile(tmp, []byte("x"), 0o644)
	_, err := c.UploadSave(UploadSaveQuery{RomID: 7, DeviceID: "dev-1", FileName: "g.sav"}, tmp)
	if _, ok := err.(*ConflictError); !ok {
		t.Fatalf("stale upload should return *ConflictError, got %T: %v", err, err)
	}
}

func TestSavesSummary(t *testing.T) {
	m := newMock("4.9.2")
	srv := m.server()
	defer srv.Close()
	c := testClient(t, srv.URL, fullScopes)
	m.saves[100] = Save{ID: 100, RomID: 7, UpdatedAt: m.updated}
	m.saves[101] = Save{ID: 101, RomID: 7, UpdatedAt: m.updated.Add(time.Hour)}
	sum, err := c.GetSavesSummary(7)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.TotalCount != 2 || len(sum.Slots) != 1 || sum.Slots[0].Latest.ID != 101 {
		t.Fatalf("unexpected summary: %+v", sum)
	}
}

func TestSupportsDeviceSaveSyncGate(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"4.9.2", true},
		{"4.9.0", true},
		{"4.10.0", true},
		{"5.0.0", true},
		{"4.8.9", false},
		{"4.8.0", false},
		{"3.0.0", false},
		{"", false},
	} {
		m := newMock(tc.version)
		srv := m.server()
		c := testClient(t, srv.URL, fullScopes)
		if got := c.SupportsDeviceSaveSync(); got != tc.want {
			t.Errorf("version %q: SupportsDeviceSaveSync = %v, want %v", tc.version, got, tc.want)
		}
		srv.Close()
	}
}

func TestVersionAtLeast(t *testing.T) {
	min := [3]int{4, 9, 0}
	cases := map[string]bool{
		"4.9.0": true, "4.9.2": true, "4.10.0": true, "5.0.0": true,
		"v4.9.0": true, "4.9.0-rc1": true, "4.9.2+build.7": true, "4.9": true,
		"4.8.9": false, "4.8": false, "3.9.9": false, "": false, "garbage": false,
	}
	for ver, want := range cases {
		if got := versionAtLeast(ver, min); got != want {
			t.Errorf("versionAtLeast(%q) = %v, want %v", ver, got, want)
		}
	}
}
