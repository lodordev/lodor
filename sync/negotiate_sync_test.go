package sync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lodor/romm"
)

// negotiateFake serves rom/saves/content/upload plus the negotiate+complete endpoints,
// driven by a fixed plan. It reuses the reconcileEnv card layout (rom 1234, gba).
type negotiateFake struct {
	plan       romm.NegotiateResponse
	server     []byte
	uploads    int
	completed  bool
	negotiated bool
}

func (f *negotiateFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/platforms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]romm.Platform{{ID: 5, FsSlug: "gba", Name: "GBA"}})
	})
	mux.HandleFunc("/api/roms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(romm.PaginatedRoms{
			Items: []romm.Rom{{ID: 1234, PlatformFsSlug: "gba", FsName: "Sonic (USA).gba", FsNameNoExt: "Sonic (USA)", Files: []romm.RomFile{{FileName: "Sonic (USA).gba"}}}},
			Total: 1,
		})
	})
	mux.HandleFunc("/api/roms/1234", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(romm.Rom{ID: 1234, PlatformFsSlug: "gba", FsName: "Sonic (USA).gba", FsNameNoExt: "Sonic (USA)", Files: []romm.RomFile{{FileName: "Sonic (USA).gba"}}})
	})
	mux.HandleFunc("/api/saves", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			f.uploads++
			_ = json.NewEncoder(w).Encode(romm.Save{ID: 50, RomID: 1234})
			return
		}
		_ = json.NewEncoder(w).Encode([]romm.Save{{ID: 8, RomID: 1234, FileExtension: "sav", UpdatedAt: time.Now(), ContentHash: ptr(md5hex(f.server))}})
	})
	mux.HandleFunc("/api/saves/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(f.server)
	})
	mux.HandleFunc("/api/sync/negotiate", func(w http.ResponseWriter, r *http.Request) {
		f.negotiated = true
		_ = json.NewEncoder(w).Encode(f.plan)
	})
	mux.HandleFunc("/api/sync/sessions/99/complete", func(w http.ResponseWriter, r *http.Request) {
		f.completed = true
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func TestExecuteNegotiationPull(t *testing.T) {
	server := []byte("SERVER-PULLED")
	f := &negotiateFake{server: server, plan: romm.NegotiateResponse{SessionID: 99, Operations: []romm.SyncOp{{Op: romm.SyncOpDownload, RomID: 1234, SaveID: 8}}}}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()
	cfg, _ := reconcileEnv(t, ts.URL, []byte("OLD-LOCAL"))

	sum := ExecuteNegotiation(newClient(ts), cfg, f.plan, NegotiateSummary{})
	if sum.Pulled != 1 || sum.Errors != 0 {
		t.Fatalf("pull op summary wrong: %+v", sum)
	}
	if got := localSaveBytes(t); string(got) != string(server) {
		t.Fatalf("pull op did not write server bytes: %q", got)
	}
}

func TestExecuteNegotiationPush(t *testing.T) {
	f := &negotiateFake{server: []byte("OLD"), plan: romm.NegotiateResponse{SessionID: 99, Operations: []romm.SyncOp{{Op: romm.SyncOpUpload, RomID: 1234}}}}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()
	cfg, _ := reconcileEnv(t, ts.URL, []byte("LOCAL-TO-PUSH"))

	sum := ExecuteNegotiation(newClient(ts), cfg, f.plan, NegotiateSummary{})
	if sum.Pushed != 1 || sum.Errors != 0 {
		t.Fatalf("push op summary wrong: %+v", sum)
	}
	if f.uploads != 1 {
		t.Fatalf("expected 1 upload, got %d", f.uploads)
	}
}

func TestExecuteNegotiationConflictNoop(t *testing.T) {
	f := &negotiateFake{server: []byte("S"), plan: romm.NegotiateResponse{Operations: []romm.SyncOp{{Op: romm.SyncOpConflict, RomID: 1234}, {Op: romm.SyncOpNoOp, RomID: 1234}}}}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()
	cfg, _ := reconcileEnv(t, ts.URL, []byte("L"))

	sum := ExecuteNegotiation(newClient(ts), cfg, f.plan, NegotiateSummary{})
	if sum.Conflicts != 1 || sum.Noops != 1 {
		t.Fatalf("conflict/noop summary wrong: %+v", sum)
	}
	if sum.Pulled != 0 || sum.Pushed != 0 || f.uploads != 0 {
		t.Fatalf("conflict/noop must do nothing: %+v uploads=%d", sum, f.uploads)
	}
}

// TestNegotiateLibrarySyncFull drives the whole flow: build request (from the local
// save), negotiate, execute a noop plan, complete the session.
func TestNegotiateLibrarySyncFull(t *testing.T) {
	f := &negotiateFake{server: []byte("S"), plan: romm.NegotiateResponse{SessionID: 99, Operations: []romm.SyncOp{{Op: romm.SyncOpNoOp, RomID: 1234}}}}
	ts := httptest.NewServer(f.handler())
	defer ts.Close()
	cfg, _ := reconcileEnv(t, ts.URL, []byte("LOCAL"))

	sum, err := NegotiateLibrarySync(newClient(ts), cfg, "dev1")
	if err != nil {
		t.Fatalf("NegotiateLibrarySync: %v", err)
	}
	if !f.negotiated {
		t.Error("negotiate endpoint was not hit")
	}
	if !sum.Completed || !f.completed {
		t.Errorf("session not completed: sum.Completed=%v server.completed=%v", sum.Completed, f.completed)
	}
	if sum.Noops != 1 {
		t.Errorf("expected 1 noop, got %+v", sum)
	}
}
