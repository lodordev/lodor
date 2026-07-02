package sync

// Content-hash lineage tests for PullSaveDirect (workstream A2, 2026-07-02).
// The pull decision must be provable from CONTENT (local MD5 vs the server save
// set's content_hash), never from clocks: RTC-less handhelds routinely write
// save mtimes from the future, which under the old mtime-vs-updated_at rule
// silently blocked every pull (Smart Pro field defect). The four lineage cases:
//
//	no local save                  → pull newest        (reason no-local)
//	local == newest revision       → no-op              (reason in-sync)
//	local == older revision        → pull newest, .bak  (reason older-lineage)
//	local matches no revision      → NEVER overwrite    (reason unpushed-local)
//
// plus skewed-mtime immunity: a local mtime far in the FUTURE must not block
// the older-lineage pull.

import (
	"crypto/md5"
	"encoding/hex"
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
)

var (
	newestBytes = []byte("NEWEST-SERVER-SAVE")
	olderBytes  = []byte("OLDER-SERVER-SAVE")
	localOnly   = []byte("UNPUSHED-LOCAL-PROGRESS")
)

func md5Hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

// lineageServer serves the minimal RomM API PullSaveDirect touches: one GBA rom
// (id 1234) with two save revisions — older (id 1, olderBytes) and newest
// (id 2, newestBytes).
func lineageServer(t *testing.T) *httptest.Server {
	t.Helper()
	oh, nh := md5Hex(olderBytes), md5Hex(newestBytes)
	saves := []romm.Save{
		{ID: 2, RomID: 1234, FileName: "Sonic (USA).gba.sav", FileExtension: "gba.sav",
			FileSizeBytes: int64(len(newestBytes)), ContentHash: &nh,
			UpdatedAt: time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)},
		{ID: 1, RomID: 1234, FileName: "Sonic (USA).gba.sav", FileExtension: "gba.sav",
			FileSizeBytes: int64(len(olderBytes)), ContentHash: &oh,
			UpdatedAt: time.Date(2026, 7, 2, 4, 0, 0, 0, time.UTC)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/roms/1234", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(romm.Rom{
			ID: 1234, PlatformFsSlug: "gba",
			FsName: "Sonic (USA).gba", FsNameNoExt: "Sonic (USA)",
			Files: []romm.RomFile{{FileName: "Sonic (USA).gba"}},
		})
	})
	mux.HandleFunc("/api/saves", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(saves)
	})
	mux.HandleFunc("/api/saves/2/content", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(newestBytes)
	})
	mux.HandleFunc("/api/saves/1/content", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(olderBytes)
	})
	return httptest.NewServer(mux)
}

// lineageEnv roots a temp SDCARD (own mode), writes the catalog index mapping
// "Sonic (USA)" → 1234, and returns the client, cfg and on-disk ROM path.
func lineageEnv(t *testing.T, srvURL string) (*romm.Client, *config.Config, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))

	cfg := &config.Config{
		Hosts:             []config.Host{{RootURI: srvURL, Token: "t"}},
		MirrorMode:        config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{"gba": {RelativePath: "GBA"}},
	}
	idxPath := filepath.Join(base, "pak", "catalog-index.json")
	if err := os.MkdirAll(filepath.Dir(idxPath), 0o755); err != nil {
		t.Fatal(err)
	}
	idx := fmt.Sprintf(`{"version":1,"platforms":{"gba":{"by_basename":{"Sonic (USA)":1234},"by_fsname":{"Sonic (USA).gba":1234},"by_id":{}}}}`)
	if err := os.WriteFile(idxPath, []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	romPath := filepath.Join(base, "Roms", "GBA", "Sonic (USA).gba")
	client := romm.NewClient(cfg.Hosts[0], 5*time.Second)
	return client, cfg, romPath
}

func localSavePath() string {
	return filepath.Join(platform.SaveDirectory("gba"), "Sonic (USA).gba.sav")
}

func writeLocalSave(t *testing.T, b []byte) string {
	t.Helper()
	p := localSavePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLineageNoLocalPullsNewest(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullWritten || res.Reason != "no-local" {
		t.Fatalf("outcome=%s reason=%q, want Written/no-local", res.Outcome, res.Reason)
	}
	got, err := os.ReadFile(localSavePath())
	if err != nil || string(got) != string(newestBytes) {
		t.Fatalf("local save = %q err=%v, want newest bytes", got, err)
	}
}

func TestLineageInSyncNoOp(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	p := writeLocalSave(t, newestBytes)
	// Make the local file LOOK ancient — content, not clocks, must decide.
	old := time.Now().Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(p, old, old)

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullInSync || res.Reason != "in-sync" {
		t.Fatalf("outcome=%s reason=%q, want InSync/in-sync", res.Outcome, res.Reason)
	}
	if res.Pulled() {
		t.Fatal("in-sync must not report pulled")
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(newestBytes) {
		t.Fatalf("local save changed on a no-op: %q", got)
	}
}

func TestLineageOlderPullsNewestWithBak(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	p := writeLocalSave(t, olderBytes)

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullWritten || res.Reason != "older-lineage" {
		t.Fatalf("outcome=%s reason=%q, want Written/older-lineage", res.Outcome, res.Reason)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(newestBytes) {
		t.Fatalf("local save = %q, want newest bytes", got)
	}
	bak, err := os.ReadFile(p + ".bak")
	if err != nil || string(bak) != string(olderBytes) {
		t.Fatalf(".bak = %q err=%v, want the pre-pull local bytes", bak, err)
	}
}

// TestLineageSkewedMtimeImmunity is the RTC regression guard: the local save's
// mtime is YEARS in the future (a handheld that booted with a garbage clock),
// but its CONTENT matches the older server revision — the pull must proceed.
// Under the removed mtime rule this exact shape silently blocked forever.
func TestLineageSkewedMtimeImmunity(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	p := writeLocalSave(t, olderBytes)
	future := time.Now().Add(5 * 365 * 24 * time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullWritten || res.Reason != "older-lineage" {
		t.Fatalf("outcome=%s reason=%q, want Written/older-lineage despite future mtime", res.Outcome, res.Reason)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(newestBytes) {
		t.Fatalf("local save = %q, want newest bytes", got)
	}
}

func TestLineageUnpushedLocalNeverOverwritten(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	p := writeLocalSave(t, localOnly)

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullLocalUnpushed || res.Reason != "unpushed-local" {
		t.Fatalf("outcome=%s reason=%q, want LocalUnpushed/unpushed-local", res.Outcome, res.Reason)
	}
	if res.Pulled() {
		t.Fatal("unpushed-local must not report pulled")
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(localOnly) {
		t.Fatalf("UNPUSHED LOCAL PROGRESS WAS OVERWRITTEN: %q", got)
	}
	if _, err := os.Stat(p + ".bak"); err == nil {
		t.Fatal("no .bak should exist — nothing may touch the local file")
	}
}

// TestPushDedupUnchangedSave locks the A2 pre-upload dedup: pushing a save whose
// exact bytes already live on the server (non-ghost content_hash match) must
// report AlreadyOnServer WITHOUT POSTing a duplicate revision.
func TestPushDedupUnchangedSave(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	writeLocalSave(t, newestBytes)

	uploads := 0
	// lineageServer has no POST handler — a POST /api/saves would 404 and the
	// result would be UploadError; AlreadyOnServer without an upload is the pass.
	results := PushSaveDirect(client, cfg, romPath)
	if len(results) != 1 {
		t.Fatalf("results = %v, want exactly one", results)
	}
	if results[0].Outcome != OutcomeAlreadyOnServer {
		t.Fatalf("outcome = %s, want AlreadyOnServer (no duplicate upload)", results[0].Outcome)
	}
	if Uploaded(results) != uploads {
		t.Fatalf("Uploaded() = %d, want 0", Uploaded(results))
	}
}

// TestLineageMarkerNamedRomPath: the whole lineage flow through a ✓-marked,
// "(RomM)"-tagged on-disk path (separate mode) — resolution, save discovery and
// the pull write must all key off the decorated name end-to-end.
func TestLineageMarkerNamedRomPath(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, _ := lineageEnv(t, srv.URL)
	cfg.MirrorMode = config.MirrorModeSeparate

	// Re-key the index the way a separate-mode mirror writes it.
	idxPath := filepath.Join(os.Getenv("LODOR_PAK_DIR"), "catalog-index.json")
	idx := `{"version":1,"platforms":{"gba":{"by_basename":{"Sonic (USA) (RomM)":1234},"by_fsname":{"Sonic (USA).gba":1234},"by_id":{}}}}`
	if err := os.WriteFile(idxPath, []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	marked := filepath.Join(os.Getenv("SDCARD_PATH"), "Roms", "GBA",
		platform.MarkerOnDevice+"Sonic (USA) (RomM).gba")

	res := PullSaveDirect(client, cfg, marked)
	if res.Outcome != PullWritten || res.Reason != "no-local" {
		t.Fatalf("outcome=%s reason=%q, want Written/no-local", res.Outcome, res.Reason)
	}
	// The pulled save must live under the DECORATED on-disk name — exactly what
	// minarch derives its .sav name from.
	want := filepath.Join(platform.SaveDirectory("gba"), platform.MarkerOnDevice+"Sonic (USA) (RomM).gba.sav")
	if res.LocalPath != want {
		t.Fatalf("save written to %q, want %q", res.LocalPath, want)
	}
	if got, err := os.ReadFile(want); err != nil || string(got) != string(newestBytes) {
		t.Fatalf("save bytes = %q err=%v", got, err)
	}
	if !strings.HasPrefix(filepath.Base(res.LocalPath), platform.MarkerOnDevice) {
		t.Fatal("save name lost its marker")
	}
}
