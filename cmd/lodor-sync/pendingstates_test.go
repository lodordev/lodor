package main

// pending-states.txt queue: enqueue dedup, drain semantics (ok drains,
// offline keeps, dark-manifest drops — the queue must never wedge).

import (
	"encoding/json"
	"fmt"
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
	"lodor/sync"
)

func pendingStatesEnv(t *testing.T, srvURL string) (*romm.Client, *config.Config, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	pak := filepath.Join(base, "pak")
	t.Setenv("LODOR_PAK_DIR", pak)
	stateRoot := filepath.Join(base, "stateroot")
	t.Setenv("LODOR_STATE_ROOT", stateRoot)
	if err := os.MkdirAll(pak, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := `{"version":1,"platforms":{"gamegear":{"by_basename":{"Woody Pop (USA, Europe, Brazil) (En)":9752},"by_fsname":{"Woody Pop (USA, Europe, Brazil) (En).gg":9752},"by_id":{}}}}`
	if err := os.WriteFile(filepath.Join(pak, "catalog-index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Hosts:             []config.Host{{RootURI: srvURL, Token: "t", DeviceID: "abcdef1234567890"}},
		MirrorMode:        config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{"gamegear": {RelativePath: "Sega Game Gear"}},
	}
	// manifest + one local state file so a live drain has something to push
	m := `{"version":1,"frontend":"nextui","arch":"arm64","systems":{"gamegear":{"core":"picodrive","dir":"GG-picodrive"}}}`
	if err := os.WriteFile(filepath.Join(pak, "statecores.json"), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateRoot, "GG-picodrive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	romBase := "Woody Pop (USA, Europe, Brazil) (En).gg"
	slot0 := romBase + ".st0"
	if platform.StatesUseRANaming() {
		slot0 = strings.TrimSuffix(romBase, filepath.Ext(romBase)) + ".state"
	}
	if err := os.WriteFile(filepath.Join(dir, slot0), []byte("QUEUED-PAYLOAD"), 0o644); err != nil {
		t.Fatal(err)
	}
	romPath := filepath.Join(base, "Roms", "Sega Game Gear", romBase)
	return romm.NewClient(cfg.Hosts[0], 2*time.Second), cfg, romPath
}

func TestEnqueuePendingStateDedup(t *testing.T) {
	pak := t.TempDir()
	t.Setenv("LODOR_PAK_DIR", pak)
	if added, err := enqueuePendingState("/roms/a.gg"); err != nil || !added {
		t.Fatalf("first enqueue: added=%v err=%v", added, err)
	}
	if added, err := enqueuePendingState("/roms/a.gg"); err != nil || added {
		t.Fatalf("dup enqueue must dedup: added=%v err=%v", added, err)
	}
	if added, err := enqueuePendingState("/roms/b.gg"); err != nil || !added {
		t.Fatalf("second rom: added=%v err=%v", added, err)
	}
	got := pendingReadFile(pendingStatesPath())
	if len(got) != 2 || got[0] != "/roms/a.gg" || got[1] != "/roms/b.gg" {
		t.Fatalf("queue = %v", got)
	}
}

func TestDrainPendingStatesPushesAndEmpties(t *testing.T) {
	uploads := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"SYSTEM": map[string]string{"VERSION": "4.9.2"}})
	})
	mux.HandleFunc("/api/roms/9752", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(romm.Rom{ID: 9752, PlatformFsSlug: "gamegear",
			FsName: "Woody Pop (USA, Europe, Brazil) (En).gg", FsNameNoExt: "Woody Pop (USA, Europe, Brazil) (En)",
			Files: []romm.RomFile{{FileName: "Woody Pop (USA, Europe, Brazil) (En).gg"}}})
	})
	mux.HandleFunc("/api/states", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			_ = json.NewEncoder(w).Encode([]romm.State{})
			return
		}
		uploads++
		_ = json.NewEncoder(w).Encode(romm.State{ID: 900 + uploads, RomID: 9752})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, cfg, romPath := pendingStatesEnv(t, srv.URL)
	if _, err := enqueuePendingState(romPath); err != nil {
		t.Fatal(err)
	}
	var lines []string
	total, drained, stuck, cancelled := drainPendingStates(client, cfg,
		func(rp string, res sync.PushStatesResult, kept bool) {
			lines = append(lines, fmt.Sprintf("%s|%s|kept=%v|pushed=%d", filepath.Base(rp), res.Reason, kept, res.Pushed))
		})
	if total != 1 || drained != 1 || len(stuck) != 0 || cancelled {
		t.Fatalf("drain: total=%d drained=%d stuck=%v cancelled=%v (%v)", total, drained, stuck, cancelled, lines)
	}
	if uploads != 1 {
		t.Fatalf("uploads = %d, want 1", uploads)
	}
	if left := pendingReadFile(pendingStatesPath()); len(left) != 0 {
		t.Fatalf("queue not emptied: %v", left)
	}
}

func TestDrainPendingStatesKeepsOfflineRom(t *testing.T) {
	// port 9 (discard) — nothing listens; every request fails → reason=offline.
	client, cfg, romPath := pendingStatesEnv(t, "http://127.0.0.1:9")
	if _, err := enqueuePendingState(romPath); err != nil {
		t.Fatal(err)
	}
	total, drained, stuck, _ := drainPendingStates(client, cfg, nil)
	if total != 1 || drained != 0 || len(stuck) != 1 {
		t.Fatalf("offline drain: total=%d drained=%d stuck=%v", total, drained, stuck)
	}
	if left := pendingReadFile(pendingStatesPath()); len(left) != 1 || left[0] != romPath {
		t.Fatalf("offline rom must stay queued: %v", left)
	}
}

func TestDrainPendingStatesDropsWhenDark(t *testing.T) {
	client, cfg, romPath := pendingStatesEnv(t, "http://127.0.0.1:9")
	// remove the manifest: feature dark → nothing will ever push → line must
	// drop (a wedged queue is worse than a dark no-op).
	if err := os.Remove(filepath.Join(os.Getenv("LODOR_PAK_DIR"), "statecores.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := enqueuePendingState(romPath); err != nil {
		t.Fatal(err)
	}
	total, drained, stuck, _ := drainPendingStates(client, cfg, nil)
	if total != 1 || drained != 1 || len(stuck) != 0 {
		t.Fatalf("dark drain: total=%d drained=%d stuck=%v", total, drained, stuck)
	}
	if left := pendingReadFile(pendingStatesPath()); len(left) != 0 {
		t.Fatalf("dark rom must drop from queue: %v", left)
	}
}

// TestPushAllStatesOfflineEnqueues drives the bulk sweep exactly as
// runPushAllStates does — sync.PushAllLocalStates with the offline-enqueue
// callback — against a dead server, and asserts the eligible ROM (the one with
// local state files) lands in pending-states.txt. Two ROMs are indexed; only
// 9752 has a local state, so ONLY 9752 may be queued (the local pre-filter must
// keep the stateless 9753 out of the network path entirely).
func TestPushAllStatesOfflineEnqueues(t *testing.T) {
	// dead server (port 9, discard) → every resolve fails → reason=offline.
	client, cfg, romPath := pendingStatesEnv(t, "http://127.0.0.1:9")

	// pendingStatesEnv writes an index with an empty by_id; the bulk sweep walks
	// by_id, so overwrite it with two mapped ROMs on the gamegear platform.
	idx := `{"version":1,"platforms":{"gamegear":{` +
		`"by_basename":{"Woody Pop (USA, Europe, Brazil) (En)":9752,"Columns (USA, Europe)":9753},` +
		`"by_fsname":{"Woody Pop (USA, Europe, Brazil) (En).gg":9752,"Columns (USA, Europe).gg":9753},` +
		`"by_id":{` +
		`"9752":"Roms/Sega Game Gear/Woody Pop (USA, Europe, Brazil) (En).gg",` +
		`"9753":"Roms/Sega Game Gear/Columns (USA, Europe).gg"}}}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("LODOR_PAK_DIR"), "catalog-index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mirror runPushAllStates' callback: enqueue every offline per-ROM push.
	res := sync.PushAllLocalStates(client, cfg, func(rp string, pr sync.PushStatesResult) {
		if pr.Reason == "offline" {
			_, _ = enqueuePendingState(rp)
		}
	})
	if res.Reason != "ok" || res.RomsWithStates != 1 || res.Queued != 1 {
		t.Fatalf("offline bulk sweep: %+v", res)
	}
	queued := pendingReadFile(pendingStatesPath())
	if len(queued) != 1 || queued[0] != romPath {
		t.Fatalf("pending-states.txt = %v, want exactly [%s] (only the ROM with local states)", queued, romPath)
	}
}

// TestDrainPendingStatesCancelKeepsQueue (lodor#42): a cancel BETWEEN entries
// stops the drain, reports cancelled=true, and keeps every not-yet-attempted
// entry queued — a B-press must never silently drop a parked state.
func TestDrainPendingStatesCancelKeepsQueue(t *testing.T) {
	client, cfg, romPath := pendingStatesEnv(t, "http://127.0.0.1:9")
	if _, err := enqueuePendingState(romPath); err != nil {
		t.Fatal(err)
	}
	client.CancelCheck = func() bool { return true } // sentinel already down
	reported := 0
	total, drained, stuck, cancelled := drainPendingStates(client, cfg,
		func(string, sync.PushStatesResult, bool) { reported++ })
	if !cancelled || total != 1 || drained != 0 || len(stuck) != 1 || reported != 0 {
		t.Fatalf("cancelled drain: total=%d drained=%d stuck=%v cancelled=%v reported=%d",
			total, drained, stuck, cancelled, reported)
	}
	if left := pendingReadFile(pendingStatesPath()); len(left) != 1 || left[0] != romPath {
		t.Fatalf("cancelled entry must stay queued: %v", left)
	}
}

// TestPushAllStatesCancelStopsSweep (lodor#42): the bulk sweep honors the
// sentinel BETWEEN ROMs — Cancelled reported, no per-ROM push attempted.
func TestPushAllStatesCancelStopsSweep(t *testing.T) {
	client, cfg, _ := pendingStatesEnv(t, "http://127.0.0.1:9")
	// pendingStatesEnv's index has an empty by_id; the sweep walks by_id, so give
	// it one mapped ROM (same shape as TestPushAllStatesOfflineEnqueues).
	idx := `{"version":1,"platforms":{"gamegear":{` +
		`"by_basename":{"Woody Pop (USA, Europe, Brazil) (En)":9752},` +
		`"by_fsname":{"Woody Pop (USA, Europe, Brazil) (En).gg":9752},` +
		`"by_id":{"9752":"Roms/Sega Game Gear/Woody Pop (USA, Europe, Brazil) (En).gg"}}}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("LODOR_PAK_DIR"), "catalog-index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	client.CancelCheck = func() bool { return true }
	called := 0
	res := sync.PushAllLocalStates(client, cfg, func(string, sync.PushStatesResult) { called++ })
	if !res.Cancelled || res.Pushed != 0 || called != 0 {
		t.Fatalf("cancelled sweep: %+v called=%d", res, called)
	}
}
