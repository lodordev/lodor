//go:build !onion && !muos && !knulli && !android && !lodorandroid

package main

// FULL-LIFECYCLE end-to-end regression (task #185).
//
// WHY THIS EXISTS: the per-feature suites were all green while real hardware bugs
// shipped (muOS seed-timing #180, muOS directory_mappings DLFAIL #182, my355 wifi
// false-positive #184). Every feature was tested in isolation; nothing drove ONE mock
// RomM through the COMPLETE journey a device actually takes. This test does exactly
// that, in sequence, asserting each hop — so a regression anywhere along
// register → mirror → download → push → cross-device reconcile → conflict is caught
// by `go test ./...`, not by Jonathan's card.
//
// It is DETERMINISTIC and mock-based on purpose: live RomM is infra-blocked (network
// sidecar) and is Jonathan's PRODUCTION library — never write to it. The mock mirrors
// the verified 4.9.x wire shape (heartbeat gate, device register, catalog listing,
// hash-verified rom content, additive datetime-tagged save timeline with per-device
// attribution, the 409 foreign-device interlock). The wire shape reuses the design of
// romm/devicesync_test.go's mock; it is re-expressed here because that mock is
// unexported and this test lives in package main (it drives the real downloadRomCore).

import (
	"crypto/md5"
	"encoding/hex"
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

	"lodor/catalog"
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
	syncpkg "lodor/sync"
)

// ---- mock RomM (full library + save timeline) -----------------------------------------

func md5hex(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }

// e2eRomM is one server holding: a device registry, a rom library (with real content
// bytes for hash-verified download), and an ADDITIVE save timeline keyed by rom_id.
type e2eRomM struct {
	mu         sync.Mutex
	version    string
	platforms  []romm.Platform
	roms       map[int]romm.Rom
	romBytes   map[int][]byte
	saves      map[int]romm.Save
	saveBytes  map[int][]byte
	nextSaveID int
	rows       map[string]romm.DeviceSaveSync // key "saveID|deviceID"
	devices    map[string]romm.Device
	devNames   map[string]string
	nextDevSeq int
	updated    time.Time
	conflict   bool // next non-overwrite upload 409s
	lastUpload romm.Save
}

func newE2E() *e2eRomM {
	return &e2eRomM{
		version:    "4.9.2",
		roms:       map[int]romm.Rom{},
		romBytes:   map[int][]byte{},
		saves:      map[int]romm.Save{},
		saveBytes:  map[int][]byte{},
		nextSaveID: 500,
		rows:       map[string]romm.DeviceSaveSync{},
		devices:    map[string]romm.Device{},
		devNames:   map[string]string{},
		updated:    time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC),
	}
}

func rowKey(id int, dev string) string { return fmt.Sprintf("%d|%s", id, dev) }

// injectSave appends a NEW datetime-tagged revision as if `dev` pushed `data` at `at` —
// the additive server timeline. Caller must NOT hold the lock.
func (m *e2eRomM) injectSave(romID int, data []byte, dev, devName string, at time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.insertSaveLocked(romID, data, dev, devName, at)
}

func (m *e2eRomM) insertSaveLocked(romID int, data []byte, dev, devName string, at time.Time) int {
	id := m.nextSaveID
	m.nextSaveID++
	h := md5hex(data)
	s := romm.Save{ID: id, RomID: romID, FileName: "save.sav", FileExtension: "sav",
		FileSizeBytes: int64(len(data)), ContentHash: &h, UpdatedAt: at}
	m.saves[id] = s
	m.saveBytes[id] = append([]byte(nil), data...)
	if dev != "" {
		m.rows[rowKey(id, dev)] = romm.DeviceSaveSync{DeviceID: dev, DeviceName: devName, LastSyncedAt: at, IsCurrent: true}
		if devName != "" {
			m.devNames[dev] = devName
		}
	}
	return id
}

func (m *e2eRomM) saveCount(romID int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.saves {
		if s.RomID == romID {
			n++
		}
	}
	return n
}

func (m *e2eRomM) hasRow(saveID int, dev string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.rows[rowKey(saveID, dev)]
	return ok
}

func (m *e2eRomM) server() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fmt.Sprintf(`{"SYSTEM":{"VERSION":%q}}`, m.version))
	})

	// device register / list.
	mux.HandleFunc("POST /api/devices", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.nextDevSeq++
		id := fmt.Sprintf("dev-%d", m.nextDevSeq)
		m.devices[id] = romm.Device{ID: id, Name: body.Name, CreatedAt: m.updated}
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"device_id": id, "name": body.Name})
	})
	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		out := make([]romm.Device, 0, len(m.devices))
		for _, d := range m.devices {
			out = append(out, d)
		}
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("GET /api/platforms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(m.platforms)
	})

	// GetRoms (paginated envelope), filtered by platform_ids.
	mux.HandleFunc("GET /api/roms", func(w http.ResponseWriter, r *http.Request) {
		pids := map[string]bool{}
		for _, v := range r.URL.Query()["platform_ids"] {
			pids[v] = true
		}
		m.mu.Lock()
		var items []romm.Rom
		for _, rom := range m.roms {
			if len(pids) == 0 || pids[strconv.Itoa(rom.PlatformID)] {
				items = append(items, rom)
			}
		}
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(romm.PaginatedRoms{Items: items, Total: len(items), Limit: 250})
	})

	mux.HandleFunc("GET /api/roms/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		m.mu.Lock()
		rom := m.roms[id]
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(rom)
	})

	// ROM content (single-file, streamed) — real bytes with Content-Length.
	mux.HandleFunc("GET /api/roms/{id}/content/{fsname}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		m.mu.Lock()
		b := m.romBytes[id]
		m.mu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
	})

	mux.HandleFunc("GET /api/collections", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})

	// Save upload (multipart saveFile). Device-attributed, ADDITIVE, with the 409
	// foreign-device interlock. The stored file_name is the multipart FILENAME (the
	// canonical, marker-stripped name the engine sends via UploadSaveQuery.FileName).
	mux.HandleFunc("POST /api/saves", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		overwrite := q.Get("overwrite") == "true"
		m.mu.Lock()
		if m.conflict && !overwrite {
			m.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"conflict","message":"newer server save","save_id":0}`)
			return
		}
		m.conflict = false
		m.mu.Unlock()

		if err := r.ParseMultipartForm(32 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f, fh, err := r.FormFile("saveFile")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(f)
		_ = f.Close()
		romID, _ := strconv.Atoi(q.Get("rom_id"))
		dev := q.Get("device_id")

		m.mu.Lock()
		id := m.nextSaveID
		m.nextSaveID++
		h := md5hex(data)
		m.updated = m.updated.Add(time.Minute) // each upload is strictly newer
		s := romm.Save{ID: id, RomID: romID, FileName: fh.Filename, FileExtension: "sav",
			FileSizeBytes: int64(len(data)), ContentHash: &h, UpdatedAt: m.updated}
		m.saves[id] = s
		m.saveBytes[id] = append([]byte(nil), data...)
		if dev != "" {
			m.rows[rowKey(id, dev)] = romm.DeviceSaveSync{DeviceID: dev, DeviceName: m.devNames[dev], LastSyncedAt: m.updated, IsCurrent: true}
		}
		m.lastUpload = s
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s)
	})

	// GetSaves (attribution when device_id present).
	mux.HandleFunc("GET /api/saves", func(w http.ResponseWriter, r *http.Request) {
		romID, _ := strconv.Atoi(r.URL.Query().Get("rom_id"))
		dev := r.URL.Query().Get("device_id")
		m.mu.Lock()
		var out []romm.Save
		for _, s := range m.saves {
			if s.RomID != romID {
				continue
			}
			if dev != "" {
				var syncs []romm.DeviceSaveSync
				for k, row := range m.rows {
					if strings.HasPrefix(k, strconv.Itoa(s.ID)+"|") {
						syncs = append(syncs, row)
					}
				}
				s.DeviceSyncs = syncs
			}
			out = append(out, s)
		}
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})

	// Save content download.
	mux.HandleFunc("GET /api/saves/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		m.mu.Lock()
		b, ok := m.saveBytes[id]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})

	// downloaded / track / untrack ledger writes (best-effort; upsert the row).
	ledger := func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		var body struct {
			DeviceID string `json:"device_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		s, ok := m.saves[id]
		if ok && body.DeviceID != "" {
			m.rows[rowKey(id, body.DeviceID)] = romm.DeviceSaveSync{DeviceID: body.DeviceID, DeviceName: m.devNames[body.DeviceID], LastSyncedAt: m.updated, IsCurrent: true}
		}
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s)
	}
	mux.HandleFunc("POST /api/saves/{id}/downloaded", ledger)
	mux.HandleFunc("POST /api/saves/{id}/track", ledger)
	mux.HandleFunc("POST /api/saves/{id}/untrack", ledger)

	return httptest.NewServer(mux)
}

// ---- env ------------------------------------------------------------------------------

// e2eEnv roots a LodorOS-style card (native host: canonical names, no ✘/✓ markers so
// the round-trip's on-disk paths are deterministic), installs the GBA Emu pak so the
// mirror will stub the platform, and returns cfg + client + card root.
func e2eEnv(t *testing.T, srvURL string) (*config.Config, *romm.Client, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_HOST_OS", "lodoros") // native: canonical on-disk names
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))
	if err := os.MkdirAll(filepath.Join(base, "pak"), 0o755); err != nil {
		t.Fatal(err)
	}
	// HasEmuPak("GBA") looks for an installed Emus pak on the card.
	if err := os.MkdirAll(filepath.Join(base, "Emus", "tg5040", "GBA.pak"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Hosts:             []config.Host{{RootURI: srvURL, Token: "scopes:assets.read,assets.write,devices.read,devices.write"}},
		MirrorMode:        config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{"gba": {Slug: "gba", RelativePath: "GBA"}},
	}
	client := romm.NewClient(cfg.Hosts[0], 10*time.Second)
	return cfg, client, base
}

// ---- the full-lifecycle round-trip ----------------------------------------------------

func TestFullLifecycleRoundTrip(t *testing.T) {
	m := newE2E()
	m.platforms = []romm.Platform{{ID: 3, FsSlug: "gba", Name: "Game Boy Advance", RomCount: 1}}
	romContent := []byte(strings.Repeat("ROM-REAL-BYTES-", 300))
	rom := romm.Rom{
		ID: 7, PlatformID: 3, PlatformFsSlug: "gba",
		FsName: "Sonic (USA).gba", FsNameNoExt: "Sonic (USA)",
		Name: "Sonic", Md5Hash: md5hex(romContent),
		Files: []romm.RomFile{{ID: 71, FileName: "Sonic (USA).gba", Md5Hash: md5hex(romContent)}},
	}
	m.roms[rom.ID] = rom
	m.romBytes[rom.ID] = romContent

	srv := m.server()
	defer srv.Close()
	cfg, client, base := e2eEnv(t, srv.URL)

	// STEP 1 — device self-registers; the fresh id is threaded and appears server-side.
	dev, err := client.RegisterDevice("RG34XX")
	if err != nil || dev.ID == "" {
		t.Fatalf("register device: id=%q err=%v", dev.ID, err)
	}
	cfg.Hosts[0].DeviceID = dev.ID
	if devs, err := client.GetDevices(); err != nil || len(devs) != 1 || devs[0].ID != dev.ID {
		t.Fatalf("registered device not in server list: %+v err=%v", devs, err)
	}

	// STEP 2 — mirror the catalog: a 0-byte stub + a written catalog-index.json that
	// reverses the on-disk path to the rom_id.
	created, _, _, _, _, _, err := catalog.MirrorCatalog(client, cfg, &catalog.Reporter{}, false)
	if err != nil || created != 1 {
		t.Fatalf("mirror: created=%d err=%v (want 1 stub)", created, err)
	}
	stubPath := platform.LocalRomPath(cfg, rom) // native host: canonical, no marker
	if fi, serr := os.Stat(stubPath); serr != nil || fi.Size() != 0 {
		t.Fatalf("stub not a 0-byte file at %q: fi=%v err=%v", stubPath, fi, serr)
	}
	if id, ok := catalog.ResolveRomID(cfg, stubPath); !ok || id != rom.ID {
		t.Fatalf("mirrored path does not resolve: (%d,%v) want (%d,true)", id, ok, rom.ID)
	}

	// STEP 3 — download the rom: bytes fetched, hash-verified vs the server hash, stub
	// filled IN PLACE (same path, now the real bytes).
	if !downloadRomCore(client, cfg, stubPath) {
		t.Fatalf("downloadRomCore returned false")
	}
	got, _ := os.ReadFile(stubPath)
	if string(got) != string(romContent) {
		t.Fatalf("downloaded rom bytes mismatch (%d vs %d bytes)", len(got), len(romContent))
	}

	// STEP 4 — write a local save + push: a new server revision whose content_hash is
	// the local md5, attributed to THIS device.
	saveDir := platform.SaveDirectory("gba")
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	saveV1 := []byte("SAVE-progress-hub-world")
	localSave := filepath.Join(saveDir, platform.SaveFileName(filepath.Base(stubPath), "sav"))
	if err := os.WriteFile(localSave, saveV1, 0o644); err != nil {
		t.Fatal(err)
	}
	pr := syncpkg.PushSaveDirect(client, cfg, stubPath)
	if len(pr) != 1 || pr[0].Outcome != syncpkg.OutcomePushed {
		t.Fatalf("push v1: %+v (want one OutcomePushed)", pr)
	}
	if m.saveCount(rom.ID) != 1 {
		t.Fatalf("server save count = %d, want 1 after first push", m.saveCount(rom.ID))
	}
	m.mu.Lock()
	up := m.lastUpload
	m.mu.Unlock()
	if up.ContentHash == nil || *up.ContentHash != md5hex(saveV1) {
		t.Fatalf("server content_hash %v != local md5 %s", up.ContentHash, md5hex(saveV1))
	}
	if !m.hasRow(up.ID, dev.ID) {
		t.Fatalf("uploaded save %d not device-attributed to %s", up.ID, dev.ID)
	}
	v1ID := up.ID

	// STEP 5 — a SECOND device changes the same save server-side (a newer revision).
	// Reconcile on device 1: the local bytes match an OLDER revision (v1), so the
	// correct action is to pull the newest, keeping the pre-pull local as .bak.
	saveV2 := []byte("SAVE-progress-second-device-beat-boss")
	m.injectSave(rom.ID, saveV2, "dev-remote", "SmartPro", m.updated.Add(2*time.Hour))
	res := syncpkg.PullSaveDirect(client, cfg, stubPath)
	if res.Outcome != syncpkg.PullWritten || res.Reason != "older-lineage" {
		t.Fatalf("cross-device reconcile: outcome=%s reason=%q, want Written/older-lineage", res.Outcome, res.Reason)
	}
	if b, _ := os.ReadFile(localSave); string(b) != string(saveV2) {
		t.Fatalf("local save not updated to the newer cross-device revision")
	}
	if b, err := os.ReadFile(localSave + ".bak"); err != nil || string(b) != string(saveV1) {
		t.Fatalf(".bak must hold the pre-pull local bytes: %q err=%v", b, err)
	}

	// STEP 6 — BOTH-CHANGED (the moat's core promise). Local diverges from the anchor
	// (new unpushed progress) AND the server diverges (yet another remote revision).
	// The local bytes now match NO server revision → CONFLICT: keep the local,
	// NEVER clobber it, and never fabricate a .bak over untouched bytes.
	localDiverged := []byte("SAVE-local-unpushed-DO-NOT-LOSE")
	if err := os.WriteFile(localSave, localDiverged, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(localSave + ".bak") // clean slate to prove no .bak is created below
	m.injectSave(rom.ID, []byte("SAVE-remote-third-revision"), "dev-remote", "SmartPro", m.updated.Add(4*time.Hour))
	res = syncpkg.PullSaveDirect(client, cfg, stubPath)
	if res.Outcome != syncpkg.PullLocalUnpushed || res.Reason != "unpushed-local" {
		t.Fatalf("BOTH-CHANGED: outcome=%s reason=%q, want LocalUnpushed/unpushed-local (never clobber)", res.Outcome, res.Reason)
	}
	if b, _ := os.ReadFile(localSave); string(b) != string(localDiverged) {
		t.Fatalf("BOTH-CHANGED CLOBBERED unpushed local progress: %q", b)
	}
	if _, err := os.Stat(localSave + ".bak"); err == nil {
		t.Fatalf("BOTH-CHANGED: no .bak may be written when nothing was overwritten")
	}
	// Keep-both is realized by an ADDITIVE push: the diverged local lands as a NEW
	// revision ALONGSIDE the server timeline — nothing on the server is deleted.
	beforeKeepBoth := m.saveCount(rom.ID)
	pr = syncpkg.PushSaveDirect(client, cfg, stubPath)
	if len(pr) != 1 || pr[0].Outcome != syncpkg.OutcomePushed {
		t.Fatalf("keep-both push: %+v (want OutcomePushed)", pr)
	}
	if got := m.saveCount(rom.ID); got != beforeKeepBoth+1 {
		t.Fatalf("keep-both must be additive: save count %d -> %d (want +1, nothing deleted)", beforeKeepBoth, got)
	}
	if _, ok := m.saves[v1ID]; !ok {
		t.Fatalf("the original v1 revision was deleted — server timeline must be preserved")
	}

	// STEP 7 — 409 stale-upload interlock. RomM 409s a non-overwrite upload; the retry
	// with overwrite=true is ADDITIVE (insert one row, delete nothing).
	if err := os.WriteFile(localSave, []byte("SAVE-after-conflict-progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.conflict = true
	m.mu.Unlock()
	before409 := m.saveCount(rom.ID)
	pr = syncpkg.PushSaveDirect(client, cfg, stubPath)
	if len(pr) != 1 || pr[0].Outcome != syncpkg.OutcomePushed || !pr[0].Conflicted {
		t.Fatalf("409 push: %+v (want OutcomePushed + Conflicted)", pr)
	}
	if got := m.saveCount(rom.ID); got != before409+1 {
		t.Fatalf("409 overwrite must be additive: count %d -> %d (want +1)", before409, got)
	}

	_ = base
}

// ---- ✘-marker save-name lifecycle (the NextUI #126 class) ------------------------------

// TestMarkerSaveNeverUploadedWithMarker drives a real PushSaveDirect from a ✘-prefixed
// on-disk save name (the fetch-on-launch pre-reconcile state on a marker host) and
// asserts the server-stored file_name is the CANONICAL name with the marker STRIPPED —
// and that NO server save is ever created carrying a ✘/✓ marker. This is the boundary
// the #126 fix guards: a device-local display artifact must never enter the server's
// data model and split one game's save timeline into per-device-state name families.
func TestMarkerSaveNeverUploadedWithMarker(t *testing.T) {
	m := newE2E()
	rom := romm.Rom{
		ID: 9, PlatformID: 3, PlatformFsSlug: "gba",
		FsName: "Emerald (USA).gba", FsNameNoExt: "Emerald (USA)",
		Files: []romm.RomFile{{ID: 91, FileName: "Emerald (USA).gba"}},
	}
	m.roms[rom.ID] = rom
	srv := m.server()
	defer srv.Close()

	// Marker host (NOT lodoros): ✘/✓ markers are used on-disk.
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_HOST_OS", "nextui")
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))
	cfg := &config.Config{
		Hosts:             []config.Host{{RootURI: srv.URL, Token: "t", DeviceID: "dev-x"}},
		MirrorMode:        config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{"gba": {Slug: "gba", RelativePath: "GBA"}},
	}
	// Catalog index so the ✘-named path resolves to rom 9.
	idxDir := filepath.Join(base, "pak")
	if err := os.MkdirAll(idxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := `{"version":1,"platforms":{"gba":{"by_basename":{"Emerald (USA)":9},"by_fsname":{"Emerald (USA).gba":9},"by_id":{}}}}`
	if err := os.WriteFile(filepath.Join(idxDir, "catalog-index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	client := romm.NewClient(cfg.Hosts[0], 10*time.Second)

	// The ✘-marked on-disk ROM path (cloud stub filled in place) + the save minarch
	// derived from that marked name.
	markedRom := filepath.Join(base, "Roms", "GBA", platform.MarkerCloud+"Emerald (USA).gba")
	saveDir := platform.SaveDirectory("gba")
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	markedSave := filepath.Join(saveDir, platform.MarkerCloud+"Emerald (USA).gba.sav")
	if err := os.WriteFile(markedSave, []byte("marked-save-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	pr := syncpkg.PushSaveDirect(client, cfg, markedRom)
	if len(pr) != 1 || pr[0].Outcome != syncpkg.OutcomePushed {
		t.Fatalf("push marked save: %+v (want OutcomePushed)", pr)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastUpload.FileName != "Emerald (USA).gba.sav" {
		t.Fatalf("server file_name = %q, want canonical marker-free %q", m.lastUpload.FileName, "Emerald (USA).gba.sav")
	}
	for _, s := range m.saves {
		if platform.HasLeadingMarker(s.FileName) {
			t.Fatalf("a server save was created carrying a state marker: %q (#126 regression)", s.FileName)
		}
	}
}

// ---- device_id-not-preseeded -----------------------------------------------------------

// TestDeviceSelfRegisterFreshID proves a config without a device_id self-registers a
// FRESH server id (never an empty/stale one), and a second registration yields a
// DISTINCT id — the engine never reuses an absent id.
func TestDeviceSelfRegisterFreshID(t *testing.T) {
	m := newE2E()
	srv := m.server()
	defer srv.Close()

	host := config.Host{RootURI: srv.URL, Token: "t"} // NOTE: DeviceID intentionally empty
	if host.DeviceID != "" {
		t.Fatal("precondition: device_id must start empty")
	}
	c1 := romm.NewClient(host, 10*time.Second)
	d1, err := c1.RegisterDevice("DeviceA")
	if err != nil || d1.ID == "" {
		t.Fatalf("first self-register: id=%q err=%v (must mint a fresh id)", d1.ID, err)
	}
	c2 := romm.NewClient(host, 10*time.Second)
	d2, err := c2.RegisterDevice("DeviceB")
	if err != nil || d2.ID == "" {
		t.Fatalf("second self-register: id=%q err=%v", d2.ID, err)
	}
	if d1.ID == d2.ID {
		t.Fatalf("a fresh registration must not reuse an id (both %q)", d1.ID)
	}
	if _, ok := m.devices[d1.ID]; !ok {
		t.Fatalf("registered id %q not present server-side", d1.ID)
	}
}
