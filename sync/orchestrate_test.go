package sync

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
	"strings"
	"testing"
	"time"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

// fakeRomM is a minimal RomM API for the reconciler integration tests. It serves one
// rom (1234, gba), a configurable save history (newest first), and save content; it
// records uploads (POST /api/saves) without mutating the served history.
type fakeRomM struct {
	serverSaves []romm.Save    // newest first; ContentHash drives reconcile
	content     map[int][]byte // save id -> bytes (for pulls)
	uploads     int            // POST /api/saves count
}

func (f *fakeRomM) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/roms/1234", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(romm.Rom{
			ID: 1234, PlatformFsSlug: "gba",
			FsName: "Sonic (USA).gba", FsNameNoExt: "Sonic (USA)",
			Files: []romm.RomFile{{FileName: "Sonic (USA).gba"}},
		})
	})
	mux.HandleFunc("/api/saves", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			f.uploads++
			// Faithful server behavior: persist the uploaded bytes so a subsequent
			// content GET for the new record (ID 99) succeeds — this is what the engine's
			// post-upload ghost-prevention verify (#63) checks. A real RomM stores the
			// multipart saveFile; model that here so the verify sees retrievable content.
			if file, _, err := r.FormFile("saveFile"); err == nil {
				defer file.Close()
				if b, rerr := io.ReadAll(file); rerr == nil {
					if f.content == nil {
						f.content = map[int][]byte{}
					}
					f.content[99] = b
				}
			}
			_ = json.NewEncoder(w).Encode(romm.Save{ID: 99, RomID: 1234})
			return
		}
		_ = json.NewEncoder(w).Encode(f.serverSaves)
	})
	mux.HandleFunc("/api/saves/", func(w http.ResponseWriter, r *http.Request) {
		// /api/saves/{id}/content
		var id int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/api/saves/"), "%d/content", &id)
		if b, ok := f.content[id]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

// reconcileEnv wires a temp card + pak dir, the gba mapping, a catalog index resolving
// the rom, writes the local save bytes, and returns cfg + the on-disk rom path.
func reconcileEnv(t *testing.T, serverURL string, localBytes []byte) (*config.Config, string) {
	t.Helper()
	base := t.TempDir()
	pak := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_PAK_DIR", pak)

	cfg := &config.Config{
		MirrorMode:        config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{"gba": {RelativePath: "GBA"}},
		Hosts:             []config.Host{{RootURI: serverURL, DeviceID: "dev1"}},
	}

	// Catalog index so ResolveRomID(romPath) -> 1234.
	idx := map[string]any{"version": 1, "platforms": map[string]any{
		"gba": map[string]any{
			"by_basename": map[string]int{"Sonic (USA)": 1234},
			"by_fsname":   map[string]int{"Sonic (USA).gba": 1234},
		}}}
	idxBytes, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(pak, "catalog-index.json"), idxBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	romPath := filepath.Join(platform.RomsDir(), "GBA", "Sonic (USA).gba")
	_ = os.MkdirAll(filepath.Dir(romPath), 0o755)
	_ = os.WriteFile(romPath, []byte("ROM"), 0o644)

	if localBytes != nil {
		saveDir := platform.SaveDirectory("gba")
		_ = os.MkdirAll(saveDir, 0o755)
		savePath := filepath.Join(saveDir, "Sonic (USA).gba.sav")
		if err := os.WriteFile(savePath, localBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, romPath
}

func localSaveBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(platform.SaveDirectory("gba"), "Sonic (USA).gba.sav"))
	if err != nil {
		return nil
	}
	return b
}

func newClient(ts *httptest.Server) *romm.Client {
	return romm.NewClient(config.Host{RootURI: ts.URL, DeviceID: "dev1"}, 10*time.Second)
}

// TestReconcileConflictNeverClobbers is the end-to-end safety proof: when local and
// server diverged from a third anchor, the engine must NOT overwrite the local save and
// must NOT upload — it surfaces a conflict and touches nothing.
func TestReconcileConflictNeverClobbers(t *testing.T) {
	local := []byte("LOCAL-WORK")
	server := []byte("SERVER-WORK")
	f := &fakeRomM{
		serverSaves: []romm.Save{{ID: 7, RomID: 1234, FileExtension: "sav", UpdatedAt: time.Now(), ContentHash: ptr(md5hex(server))}},
		content:     map[int][]byte{7: server},
	}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()

	cfg, romPath := reconcileEnv(t, ts.URL, local)
	// Anchor = a THIRD content (both sides moved away from it).
	_ = SaveAnchor(1234, Anchor{Hash: md5hex([]byte("ANCESTOR"))})

	res := SyncSaveReconciled(newClient(ts), cfg, romPath, ReconcileOpts{})

	if res.Decision != DecisionConflict || !res.Conflict {
		t.Fatalf("expected Conflict, got %s (conflict=%v)", res.Decision, res.Conflict)
	}
	if res.Pulled || res.Pushed {
		t.Fatalf("conflict must not transfer: pulled=%v pushed=%v", res.Pulled, res.Pushed)
	}
	if f.uploads != 0 {
		t.Fatalf("conflict must not upload, got %d uploads", f.uploads)
	}
	if got := localSaveBytes(t); string(got) != string(local) {
		t.Fatalf("conflict CLOBBERED local save: got %q want %q", got, local)
	}
}

// TestReconcileKeepServerPulls: local==anchor, server moved -> pull server bytes, anchor advances.
func TestReconcileKeepServerPulls(t *testing.T) {
	local := []byte("OLD")
	server := []byte("NEW")
	f := &fakeRomM{
		serverSaves: []romm.Save{{ID: 8, RomID: 1234, FileExtension: "sav", UpdatedAt: time.Now(), ContentHash: ptr(md5hex(server))}},
		content:     map[int][]byte{8: server},
	}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()

	cfg, romPath := reconcileEnv(t, ts.URL, local)
	_ = SaveAnchor(1234, Anchor{Hash: md5hex(local)}) // local == anchor

	res := SyncSaveReconciled(newClient(ts), cfg, romPath, ReconcileOpts{})
	if res.Decision != DecisionKeepServer || !res.Pulled {
		t.Fatalf("expected KeepServer+pulled, got %s pulled=%v reason=%q", res.Decision, res.Pulled, res.Reason)
	}
	if got := localSaveBytes(t); string(got) != string(server) {
		t.Fatalf("pull did not write server bytes: got %q", got)
	}
	if a, ok := LoadAnchor(1234); !ok || !strings.EqualFold(a.Hash, md5hex(server)) {
		t.Fatalf("anchor not advanced to server hash: %+v", a)
	}
}

// TestReconcileKeepLocalPushes: server==anchor, local moved -> push, server bytes untouched on disk.
func TestReconcileKeepLocalPushes(t *testing.T) {
	local := []byte("MY-NEW-SAVE")
	server := []byte("OLD-SERVER")
	f := &fakeRomM{
		serverSaves: []romm.Save{{ID: 9, RomID: 1234, FileExtension: "sav", UpdatedAt: time.Now(), ContentHash: ptr(md5hex(server))}},
		content:     map[int][]byte{9: server},
	}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()

	cfg, romPath := reconcileEnv(t, ts.URL, local)
	_ = SaveAnchor(1234, Anchor{Hash: md5hex(server)}) // server == anchor

	res := SyncSaveReconciled(newClient(ts), cfg, romPath, ReconcileOpts{})
	if res.Decision != DecisionKeepLocal || !res.Pushed {
		t.Fatalf("expected KeepLocal+pushed, got %s pushed=%v reason=%q", res.Decision, res.Pushed, res.Reason)
	}
	if f.uploads != 1 {
		t.Fatalf("expected exactly 1 upload, got %d", f.uploads)
	}
	if got := localSaveBytes(t); string(got) != string(local) {
		t.Fatalf("push must not change local save: got %q", got)
	}
	if a, ok := LoadAnchor(1234); !ok || !strings.EqualFold(a.Hash, md5hex(local)) {
		t.Fatalf("anchor not advanced to local hash: %+v", a)
	}
}

// TestReconcileInSync: identical content -> no transfer, anchor refreshed.
func TestReconcileInSync(t *testing.T) {
	same := []byte("SAME")
	f := &fakeRomM{
		serverSaves: []romm.Save{{ID: 10, RomID: 1234, FileExtension: "sav", UpdatedAt: time.Now(), ContentHash: ptr(md5hex(same))}},
		content:     map[int][]byte{10: same},
	}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()

	cfg, romPath := reconcileEnv(t, ts.URL, same)
	res := SyncSaveReconciled(newClient(ts), cfg, romPath, ReconcileOpts{})
	if res.Decision != DecisionInSync {
		t.Fatalf("expected InSync, got %s", res.Decision)
	}
	if res.Pulled || res.Pushed || f.uploads != 0 {
		t.Fatalf("in-sync must do nothing: pulled=%v pushed=%v uploads=%d", res.Pulled, res.Pushed, f.uploads)
	}
	if a, ok := LoadAnchor(1234); !ok || !strings.EqualFold(a.Hash, md5hex(same)) {
		t.Fatalf("anchor not set on in-sync: %+v", a)
	}
}

// TestReconcilePendingForcesKeepLocal: a pending-upload rom keeps local even when the
// server has advanced (would otherwise be KeepServer).
func TestReconcilePendingForcesKeepLocal(t *testing.T) {
	local := []byte("PENDING-LOCAL")
	server := []byte("SERVER-NEWER")
	f := &fakeRomM{
		serverSaves: []romm.Save{{ID: 11, RomID: 1234, FileExtension: "sav", UpdatedAt: time.Now(), ContentHash: ptr(md5hex(server))}},
		content:     map[int][]byte{11: server},
	}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()

	cfg, romPath := reconcileEnv(t, ts.URL, local)
	_ = SaveAnchor(1234, Anchor{Hash: md5hex(local)}) // local==anchor (would be KeepServer)

	res := SyncSaveReconciled(newClient(ts), cfg, romPath, ReconcileOpts{PendingUpload: true})
	if res.Decision != DecisionKeepLocal || !res.Pushed {
		t.Fatalf("pending must force KeepLocal+push, got %s pushed=%v", res.Decision, res.Pushed)
	}
	if got := localSaveBytes(t); string(got) != string(local) {
		t.Fatalf("pending local was modified: got %q", got)
	}
}

// TestReconcileLegacyFallbackNoHash: a server save with no content_hash falls back to
// newest-wins (HashCompared=false), preserving old behavior for old servers.
func TestReconcileLegacyFallbackNoHash(t *testing.T) {
	local := []byte("LOCAL")
	server := []byte("SERVER")
	f := &fakeRomM{
		// ContentHash nil -> cannot hash-reconcile.
		serverSaves: []romm.Save{{ID: 12, RomID: 1234, FileExtension: "sav", UpdatedAt: time.Now().Add(time.Hour), ContentHash: nil}},
		content:     map[int][]byte{12: server},
	}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()

	cfg, romPath := reconcileEnv(t, ts.URL, local)
	// Make the local file older than the server save so newest-wins pulls.
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(filepath.Join(platform.SaveDirectory("gba"), "Sonic (USA).gba.sav"), old, old)

	res := SyncSaveReconciled(newClient(ts), cfg, romPath, ReconcileOpts{})
	if res.HashCompared {
		t.Fatalf("expected legacy fallback (HashCompared=false)")
	}
	if !res.Pulled {
		t.Fatalf("legacy newest-wins should pull the newer server save")
	}
}

func ptr(s string) *string { return &s }
