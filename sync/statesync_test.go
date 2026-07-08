package sync

// PushStates end-to-end against an httptest RomM: manifest gating, minarch
// naming enumeration, statefmt normalization on ingest (an RASTATE-wrapped
// local file uploads as RAW), ledger dedup across runs.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

func statesyncEnv(t *testing.T, srvURL string) (*romm.Client, *config.Config, string, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	pak := filepath.Join(base, "pak")
	t.Setenv("LODOR_PAK_DIR", pak)
	stateRoot := filepath.Join(base, "stateroot")
	t.Setenv("LODOR_STATE_ROOT", stateRoot)

	cfg := &config.Config{
		Hosts:             []config.Host{{RootURI: srvURL, Token: "t", DeviceID: "abcdef1234567890"}},
		MirrorMode:        config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{"gamegear": {RelativePath: "Sega Game Gear"}},
	}
	if err := os.MkdirAll(pak, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := `{"version":1,"platforms":{"gamegear":{"by_basename":{"Woody Pop (USA, Europe, Brazil) (En)":9752},"by_fsname":{"Woody Pop (USA, Europe, Brazil) (En).gg":9752},"by_id":{}}}}`
	if err := os.WriteFile(filepath.Join(pak, "catalog-index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	romPath := filepath.Join(base, "Roms", "Sega Game Gear", "Woody Pop (USA, Europe, Brazil) (En).gg")
	return romm.NewClient(cfg.Hosts[0], 5*time.Second), cfg, romPath, stateRoot
}

func writeManifest(t *testing.T, systems string) {
	t.Helper()
	m := fmt.Sprintf(`{"version":1,"frontend":"nextui","arch":"arm64","systems":%s}`, systems)
	if err := os.WriteFile(filepath.Join(os.Getenv("LODOR_PAK_DIR"), "statecores.json"), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
}

func statesyncServer(t *testing.T, uploads *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"SYSTEM": map[string]string{"VERSION": "4.9.2"}})
	})
	mux.HandleFunc("/api/roms/9752", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(romm.Rom{ID: 9752, PlatformFsSlug: "gamegear",
			FsName: "Woody Pop (USA, Europe, Brazil) (En).gg", FsNameNoExt: "Woody Pop (USA, Europe, Brazil) (En)",
			Files: []romm.RomFile{{FileName: "Woody Pop (USA, Europe, Brazil) (En).gg"}}})
	})
	id := 500
	mux.HandleFunc("/api/states", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			_ = json.NewEncoder(w).Encode([]romm.State{})
			return
		}
		_ = r.ParseMultipartForm(1 << 20)
		f, hdr, err := r.FormFile("stateFile")
		if err != nil {
			w.WriteHeader(400)
			return
		}
		buf := make([]byte, 4)
		n, _ := f.Read(buf)
		id++
		*uploads = append(*uploads, fmt.Sprintf("%s|%s|%s|%dB-head=%q",
			r.URL.Query().Get("rom_id"), r.URL.Query().Get("emulator"), hdr.Filename, n, buf[:n]))
		_ = json.NewEncoder(w).Encode(romm.State{ID: id, RomID: 9752, FileName: hdr.Filename})
	})
	return httptest.NewServer(mux)
}

// tiny RASTATE wrapper for a payload, mirroring statefmt's builder shape
func wrapRastate(raw []byte) []byte {
	out := append([]byte("RASTATE"), 1)
	add := func(marker string, body []byte) {
		out = append(out, marker...)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(body)))
		out = append(out, b[:]...)
		out = append(out, body...)
		if pad := (8 - len(body)%8) % 8; pad > 0 {
			out = append(out, make([]byte, pad)...)
		}
	}
	add("MEM ", raw)
	add("END ", nil)
	return out
}

func TestPushStatesDarkWithoutManifest(t *testing.T) {
	var ups []string
	srv := statesyncServer(t, &ups)
	defer srv.Close()
	client, cfg, romPath, _ := statesyncEnv(t, srv.URL)
	res := PushStates(client, cfg, romPath)
	if res.Reason != "no-manifest" || res.Pushed != 0 {
		t.Fatalf("dark launch broken: %+v", res)
	}
	if len(ups) != 0 {
		t.Fatal("uploaded without a manifest")
	}
}

func TestPushStatesUploadsNormalizedAndDedups(t *testing.T) {
	var ups []string
	srv := statesyncServer(t, &ups)
	defer srv.Close()
	client, cfg, romPath, stateRoot := statesyncEnv(t, srv.URL)
	writeManifest(t, `{"gamegear":{"core":"picodrive","version":"abc123","dir":"GG-picodrive"}}`)

	dir := filepath.Join(stateRoot, "GG-picodrive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	romBase := "Woody Pop (USA, Europe, Brazil) (En).gg"
	// File names follow THIS build tag's frontend naming (that's the point of the
	// per-tag providers). slot 0 = raw payload; slot auto = RASTATE-wrapped — a
	// shape this lane might not produce, but ingest must normalize regardless.
	stem := strings.TrimSuffix(romBase, filepath.Ext(romBase))
	slot0, slotAuto := romBase+".st0", romBase+".st9"
	if platform.StatesUseRANaming() {
		slot0, slotAuto = stem+".state", stem+".state.auto"
	}
	if err := os.WriteFile(filepath.Join(dir, slot0), []byte("RAW-PAYLOAD-0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, slotAuto), wrapRastate([]byte("AUTO-PAYLOAD")), 0o644); err != nil {
		t.Fatal(err)
	}

	res := PushStates(client, cfg, romPath)
	if res.Pushed != 2 || res.Failed != 0 || res.Reason != "ok" {
		t.Fatalf("push: %+v (uploads: %v)", res, ups)
	}
	joined := strings.Join(ups, "\n")
	if !strings.Contains(joined, "9752|lodor/nextui/picodrive@abc123/arm64|") {
		t.Fatalf("tuple wrong:\n%s", joined)
	}
	if !strings.Contains(joined, `4B-head="RAW-"`) || !strings.Contains(joined, `4B-head="AUTO"`) {
		t.Fatalf("payloads not normalized to raw:\n%s", joined)
	}
	if !strings.Contains(joined, "(lodor sauto abcdef12).state") || !strings.Contains(joined, "(lodor s0 abcdef12).state") {
		t.Fatalf("D6 naming wrong:\n%s", joined)
	}

	// second run: everything deduped via the ledger, nothing re-uploads
	res2 := PushStates(client, cfg, romPath)
	if res2.Pushed != 0 || res2.Skipped != 2 {
		t.Fatalf("dedup: %+v", res2)
	}
	if len(ups) != 2 {
		t.Fatalf("re-uploaded: %v", ups)
	}
}

func TestPushStatesNoSystemAndNoStates(t *testing.T) {
	var ups []string
	srv := statesyncServer(t, &ups)
	defer srv.Close()
	client, cfg, romPath, stateRoot := statesyncEnv(t, srv.URL)

	writeManifest(t, `{"gba":{"core":"gpsp","dir":"GBA-gpsp"}}`)
	if res := PushStates(client, cfg, romPath); res.Reason != "no-system" {
		t.Fatalf("want no-system: %+v", res)
	}
	writeManifest(t, `{"gamegear":{"core":"picodrive","dir":"GG-picodrive"}}`)
	_ = os.MkdirAll(filepath.Join(stateRoot, "GG-picodrive"), 0o755)
	if res := PushStates(client, cfg, romPath); res.Reason != "no-states" {
		t.Fatalf("want no-states: %+v", res)
	}
}

// ── Pull side (statepull.go) ─────────────────────────────────────────────

// pullServer: states list w/ mixed tuples + content served on both routes.
func pullServer(t *testing.T, uploads *[]string, content []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"SYSTEM": map[string]string{"VERSION": "4.9.2"}})
	})
	mux.HandleFunc("/api/roms/9752", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(romm.Rom{ID: 9752, PlatformFsSlug: "gamegear",
			FsName: "Woody Pop (USA, Europe, Brazil) (En).gg", FsNameNoExt: "Woody Pop (USA, Europe, Brazil) (En)",
			Files: []romm.RomFile{{FileName: "Woody Pop (USA, Europe, Brazil) (En).gg"}}})
	})
	now := time.Now()
	mux.HandleFunc("/api/states", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseMultipartForm(1 << 20)
			f, hdr, _ := r.FormFile("stateFile")
			b := make([]byte, 64)
			n, _ := f.Read(b)
			*uploads = append(*uploads, hdr.Filename+"|"+string(b[:n]))
			_ = json.NewEncoder(w).Encode(romm.State{ID: 900 + len(*uploads), RomID: 9752})
			return
		}
		_ = json.NewEncoder(w).Encode([]romm.State{
			{ID: 71, RomID: 9752, FileName: "W [ts] (lodor sauto rg40).state", Emulator: "lodor/knulli/picodrive@abc123/arm64",
				DownloadPath: "/api/raw/assets/x/71.state", FileSizeBytes: 12, UpdatedAt: now},
			{ID: 72, RomID: 9752, FileName: "W [ts] (lodor s0 flip).state", Emulator: "lodor/lodoros/picodrive@abc123/armhf",
				DownloadPath: "/api/raw/assets/x/72.state", FileSizeBytes: 12, UpdatedAt: now},
			{ID: 73, RomID: 9752, FileName: "W [old].state.auto", Emulator: "builtin",
				DownloadPath: "/api/raw/assets/x/73.state", FileSizeBytes: 12, UpdatedAt: now},
		})
	})
	mux.HandleFunc("/api/raw/assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	})
	return httptest.NewServer(mux)
}

func TestListStatesCompatPolicy(t *testing.T) {
	var ups []string
	srv := pullServer(t, &ups, []byte("STATE-CONTENT"))
	defer srv.Close()
	client, cfg, romPath, _ := statesyncEnv(t, srv.URL)
	// local tuple: nextui frontend, picodrive@abc123, arm64
	writeManifest(t, `{"gamegear":{"core":"picodrive","version":"abc123","dir":"GG-picodrive"}}`)

	res := ListStates(client, cfg, romPath)
	if res.Reason != "ok" || len(res.Offers) != 3 {
		t.Fatalf("list: %+v", res)
	}
	byID := map[int]StateOffer{}
	for _, o := range res.Offers {
		byID[o.ID] = o
	}
	// 71: same core+ver+arch, DIFFERENT frontend (knulli) -> compatible
	if !byID[71].Compatible || byID[71].Slot != "auto" {
		t.Fatalf("71 should be compatible cross-frontend: %+v", byID[71])
	}
	// 72: same core, different ARCH -> incompatible
	if byID[72].Compatible || !strings.Contains(byID[72].Why, "architecture") {
		t.Fatalf("72 should be arch-incompatible: %+v", byID[72])
	}
	// 73: Grout builtin -> foreign, incompatible, slot parsed from suffix
	if byID[73].Compatible || byID[73].Slot != "auto" || !strings.HasPrefix(byID[73].Origin, "foreign:") {
		t.Fatalf("73 foreign handling: %+v", byID[73])
	}
}

func TestPullStatePlacesAndProtectsOccupant(t *testing.T) {
	var ups []string
	srv := pullServer(t, &ups, []byte("STATE-CONTENT"))
	defer srv.Close()
	client, cfg, romPath, stateRoot := statesyncEnv(t, srv.URL)
	writeManifest(t, `{"gamegear":{"core":"picodrive","version":"abc123","dir":"GG-picodrive"}}`)
	dir := filepath.Join(stateRoot, "GG-picodrive")
	_ = os.MkdirAll(dir, 0o755)

	romBase := "Woody Pop (USA, Europe, Brazil) (En).gg"
	target := platform.StateFileForSlot(dir, romBase, "auto")
	// an occupant the server does NOT know: must be uploaded first + .bak'd
	if err := os.WriteFile(target, []byte("LOCAL-UNPUSHED-OCCUPANT"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := PullState(client, cfg, romPath, 71, "")
	if !res.Placed || res.Reason != "ok" {
		t.Fatalf("pull: %+v", res)
	}
	if len(ups) != 1 || !strings.Contains(ups[0], "LOCAL-UNPUSHED-OCCUPANT") {
		t.Fatalf("occupant was not uploaded first: %v", ups)
	}
	bak, err := os.ReadFile(target + ".bak")
	if err != nil || string(bak) != "LOCAL-UNPUSHED-OCCUPANT" {
		t.Fatalf(".bak missing/wrong: %v %q", err, bak)
	}
	placed, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if platform.StatesUseRANaming() {
		if !strings.HasPrefix(string(placed), "RASTATE") {
			t.Fatalf("RA host should get wrapped delivery: %q", placed[:12])
		}
	} else if string(placed) != "STATE-CONTENT" {
		t.Fatalf("minarch host should get raw delivery: %q", placed)
	}
	// pulled entry recorded -> re-pull sees occupant KNOWN, no second upload
	res2 := PullState(client, cfg, romPath, 71, "")
	if !res2.Placed || len(ups) != 1 {
		t.Fatalf("re-pull: %+v uploads=%v", res2, ups)
	}
}

func TestPullStateRefusals(t *testing.T) {
	var ups []string
	srv := pullServer(t, &ups, []byte("STATE-CONTENT"))
	defer srv.Close()
	client, cfg, romPath, stateRoot := statesyncEnv(t, srv.URL)
	writeManifest(t, `{"gamegear":{"core":"picodrive","version":"abc123","dir":"GG-picodrive"}}`)
	_ = os.MkdirAll(filepath.Join(stateRoot, "GG-picodrive"), 0o755)

	if res := PullState(client, cfg, romPath, 72, ""); res.Placed || res.Reason != "incompatible" {
		t.Fatalf("arch mismatch must refuse: %+v", res)
	}
	if res := PullState(client, cfg, romPath, 73, ""); res.Placed || res.Reason != "incompatible" {
		t.Fatalf("foreign must refuse: %+v", res)
	}
	if res := PullState(client, cfg, romPath, 999, ""); res.Placed || res.Reason != "not-found" {
		t.Fatalf("unknown id: %+v", res)
	}
}

func TestPullStateStrictSize(t *testing.T) {
	var ups []string
	srv := pullServer(t, &ups, []byte("STATE-CONTENT")) // 13 bytes
	defer srv.Close()
	client, cfg, romPath, stateRoot := statesyncEnv(t, srv.URL)
	// manifest declares fixed size 99 -> 13-byte payload refused (D11)
	writeManifest(t, `{"gamegear":{"core":"picodrive","version":"abc123","dir":"GG-picodrive","size":99}}`)
	_ = os.MkdirAll(filepath.Join(stateRoot, "GG-picodrive"), 0o755)
	if res := PullState(client, cfg, romPath, 71, ""); res.Placed || res.Reason != "size-mismatch" {
		t.Fatalf("strict size must refuse: %+v", res)
	}
}

// ── Retention (design 6.4) ───────────────────────────────────────────────

// retentionServer: mutable states listing + the delete endpoint. Uploads land
// as ID 501 and join the listing (matching real server behavior: retention's
// fresh GET sees the just-pushed record).
func retentionServer(t *testing.T, listing map[int]string, deletedIDs *[]int) *httptest.Server {
	t.Helper()
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
		if r.Method == http.MethodPost {
			_ = r.ParseMultipartForm(1 << 20)
			_, hdr, err := r.FormFile("stateFile")
			if err != nil {
				w.WriteHeader(400)
				return
			}
			listing[501] = r.URL.Query().Get("emulator")
			_ = json.NewEncoder(w).Encode(romm.State{ID: 501, RomID: 9752, FileName: hdr.Filename})
			return
		}
		out := []romm.State{}
		for id, emu := range listing {
			out = append(out, romm.State{ID: id, RomID: 9752, Emulator: emu, FileName: "x.state"})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/states/delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			States []int `json:"states"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, id := range body.States {
			delete(listing, id)
			*deletedIDs = append(*deletedIDs, id)
		}
		_, _ = w.Write([]byte("{}"))
	})
	return httptest.NewServer(mux)
}

func TestPushStatesRetiresOwnOldUploads(t *testing.T) {
	listing := map[int]string{}
	var deleted []int
	srv := retentionServer(t, listing, &deleted)
	defer srv.Close()
	client, cfg, romPath, stateRoot := statesyncEnv(t, srv.URL)
	writeManifest(t, `{"gamegear":{"core":"picodrive","dir":"GG-picodrive"}}`)

	dir := filepath.Join(stateRoot, "GG-picodrive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	romBase := "Woody Pop (USA, Europe, Brazil) (En).gg"
	slot0 := romBase + ".st0"
	if platform.StatesUseRANaming() {
		slot0 = strings.TrimSuffix(romBase, filepath.Ext(romBase)) + ".state"
	}
	if err := os.WriteFile(filepath.Join(dir, slot0), []byte("FRESH-PAYLOAD"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Slot-0 ledger history, oldest→newest: 99 own but absent server-side (heal
	// only); 100 own per ledger but the server record is non-lodor (contested —
	// never delete); 101..106 own lodor uploads. 300 is a PULLED record (not
	// Own) older than everything — invariant 7.9 says it is untouchable.
	base := time.Now().Add(-time.Hour)
	own := func(id, ageMin int) StateLedgerEntry {
		return StateLedgerEntry{MD5: fmt.Sprintf("md5-%d", id), Size: 1, ServerID: id, Slot: "0",
			Origin: "lodor/nextui/picodrive/arm64", Own: true, RecordedAt: base.Add(time.Duration(ageMin) * time.Minute)}
	}
	if err := RecordState(9752, own(99, 0)); err != nil {
		t.Fatal(err)
	}
	_ = RecordState(9752, own(100, 1))
	for i, id := range []int{101, 102, 103, 104, 105, 106} {
		_ = RecordState(9752, own(id, 2+i))
	}
	_ = RecordState(9752, StateLedgerEntry{MD5: "md5-300", Size: 1, ServerID: 300, Slot: "0",
		Origin: "lodor/lodoros/picodrive/armhf", RecordedAt: base.Add(-time.Hour)})

	listing[100] = "builtin"
	for _, id := range []int{101, 102, 103, 104, 105, 106} {
		listing[id] = "lodor/nextui/picodrive/arm64"
	}
	listing[300] = "lodor/lodoros/picodrive/armhf"

	res := PushStates(client, cfg, romPath)
	if res.Pushed != 1 || res.Reason != "ok" {
		t.Fatalf("push: %+v", res)
	}
	// Own slot-0 entries at retention time, newest first: 501(new),106..101,100,99.
	// Keep 5 (501,106,105,104,103); victims 102,101,100,99 → two deletions.
	if res.Retired != 2 {
		t.Fatalf("retired = %d, want 2 (%+v; deleted %v)", res.Retired, res, deleted)
	}
	sort.Ints(deleted)
	if len(deleted) != 2 || deleted[0] != 101 || deleted[1] != 102 {
		t.Fatalf("deleted %v, want [101 102]", deleted)
	}
	if _, alive := listing[300]; !alive {
		t.Fatal("deletion propagated to a foreign (pulled) record")
	}
	if _, alive := listing[100]; !alive {
		t.Fatal("deleted a contested-identity record")
	}
	liveIDs := map[int]bool{}
	for _, e := range StateLedgerEntries(9752) {
		if e.ServerID != 0 {
			liveIDs[e.ServerID] = true
		}
	}
	for _, gone := range []int{99, 101, 102} {
		if liveIDs[gone] {
			t.Fatalf("ledger still holds retired/absent server id %d", gone)
		}
	}
	if !liveIDs[100] || !liveIDs[300] || !liveIDs[501] {
		t.Fatalf("ledger lost ids it must keep: %v", liveIDs)
	}

	// Second push: nothing new lands (dedup), so retention must NOT run — the
	// remaining victims (100, and 99's healed entry) stay untouched.
	res2 := PushStates(client, cfg, romPath)
	if res2.Pushed != 0 || res2.Retired != 0 {
		t.Fatalf("retention ran without a landed push: %+v", res2)
	}
}

// ── mkstatecores.sh contract ─────────────────────────────────────────────

// The emitter (release/mkstatecores.sh) and loadStateCores share a contract;
// this fixture is byte-for-byte what the script emits for a 3-system knulli
// lane (verified live 2026-07-07). If the loader ever tightens, this test
// forces the emitter to move with it.
func TestLoadStateCoresAcceptsMkstatecoresOutput(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LODOR_PAK_DIR", base)
	emitted := `{
  "version": 1,
  "frontend": "knulli",
  "arch": "arm64",
  "systems": {
    "gba": {"core": "gpsp", "version": "0.91-42f5", "dir": "gba"},
    "gamegear": {"core": "picodrive", "version": "1.9.6", "dir": "gamegear"},
    "snes": {"core": "snes9x2005_plus", "dir": "snes", "size": 262144}
  }
}
`
	if err := os.WriteFile(filepath.Join(base, "statecores.json"), []byte(emitted), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, ok := loadStateCores()
	if !ok {
		t.Fatal("loader rejected mkstatecores.sh output")
	}
	if got := sc.tupleFor(sc.Systems["gba"]); got != "lodor/knulli/gpsp@0.91-42f5/arm64" {
		t.Fatalf("tuple = %q", got)
	}
	if got := sc.tupleFor(sc.Systems["snes"]); got != "lodor/knulli/snes9x2005_plus/arm64" {
		t.Fatalf("versionless tuple = %q", got)
	}
	if sc.Systems["snes"].Size != 262144 {
		t.Fatalf("size = %d", sc.Systems["snes"].Size)
	}
}

func TestListStatesKnownFlag(t *testing.T) {
	var ups []string
	srv := pullServer(t, &ups, []byte("STATE-CONTENT"))
	defer srv.Close()
	client, cfg, romPath, stateRoot := statesyncEnv(t, srv.URL)
	writeManifest(t, `{"gamegear":{"core":"picodrive","version":"abc123","dir":"GG-picodrive"}}`)
	_ = os.MkdirAll(filepath.Join(stateRoot, "GG-picodrive"), 0o755)

	first := ListStates(client, cfg, romPath)
	if first.Reason != "ok" || len(first.Offers) == 0 {
		t.Fatalf("list: %+v", first)
	}
	for _, o := range first.Offers {
		if o.Known {
			t.Fatalf("empty ledger but id %d known", o.ID)
		}
	}
	// Record one offer's server id in the ledger (as a pull would) -> only that
	// offer flips to known (the launch card's "not news anymore" signal).
	target := first.Offers[0].ID
	if err := RecordState(9752, StateLedgerEntry{MD5: "md5-x", Size: 1, ServerID: target, Slot: "0"}); err != nil {
		t.Fatal(err)
	}
	second := ListStates(client, cfg, romPath)
	for _, o := range second.Offers {
		if o.ID == target && !o.Known {
			t.Fatalf("recorded id %d not known", target)
		}
		if o.ID != target && o.Known {
			t.Fatalf("unrecorded id %d known", o.ID)
		}
	}
}

// ── Auto-state retirement after restore (#28) ────────────────────────────

func TestRetireAutoStateAfterRestore(t *testing.T) {
	var ups []string
	srv := statesyncServer(t, &ups)
	defer srv.Close()
	client, cfg, romPath, stateRoot := statesyncEnv(t, srv.URL)
	writeManifest(t, `{"gamegear":{"core":"picodrive","dir":"GG-picodrive"}}`)
	dir := filepath.Join(stateRoot, "GG-picodrive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	romBase := "Woody Pop (USA, Europe, Brazil) (En).gg"
	stem := strings.TrimSuffix(romBase, filepath.Ext(romBase))
	autoName, slot1Name := romBase+".st9", romBase+".st1"
	if platform.StatesUseRANaming() {
		autoName, slot1Name = stem+".state.auto", stem+".state1"
	}
	autoPath := filepath.Join(dir, autoName)
	slot1Path := filepath.Join(dir, slot1Name)
	if err := os.WriteFile(autoPath, []byte("AUTO-STATE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slot1Path, []byte("MANUAL-SLOT-1"), 0o644); err != nil {
		t.Fatal(err)
	}

	retired, reason := RetireAutoStateAfterRestore(client, cfg, romPath)
	if !retired || reason != "retired" {
		t.Fatalf("want retired: got %v %q (uploads %v)", retired, reason, ups)
	}
	// auto-state RENAMED to .pre-sync (never deleted), gone at the original name
	if _, err := os.Stat(autoPath); !os.IsNotExist(err) {
		t.Fatal("auto-state still at original name — frontend would still auto-load it")
	}
	if b, err := os.ReadFile(autoPath + ".pre-sync"); err != nil || string(b) != "AUTO-STATE" {
		t.Fatalf("retired .pre-sync missing or corrupt: %v", err)
	}
	// manual slot UNTOUCHED
	if b, _ := os.ReadFile(slot1Path); string(b) != "MANUAL-SLOT-1" {
		t.Fatal("manual slot 1 was modified — retirement must touch only the auto slot")
	}
	// preserved-first: both states uploaded to the server BEFORE the local retire
	if len(ups) < 2 {
		t.Fatalf("states not pushed before retire (uploads=%v)", ups)
	}
}

func TestRetireAutoStateFailSafeAndGates(t *testing.T) {
	var ups []string
	srv := statesyncServer(t, &ups)
	defer srv.Close()
	client, cfg, romPath, stateRoot := statesyncEnv(t, srv.URL)

	// (a) no manifest → statesync dark → never retire.
	if r, why := RetireAutoStateAfterRestore(client, cfg, romPath); r || why != "no-manifest" {
		t.Fatalf("no-manifest must not retire: %v %q", r, why)
	}

	// (b) manifest + a MANUAL slot but NO auto-state → nothing to retire, slot safe.
	writeManifest(t, `{"gamegear":{"core":"picodrive","dir":"GG-picodrive"}}`)
	dir := filepath.Join(stateRoot, "GG-picodrive")
	_ = os.MkdirAll(dir, 0o755)
	romBase := "Woody Pop (USA, Europe, Brazil) (En).gg"
	slot1 := romBase + ".st1"
	if platform.StatesUseRANaming() {
		slot1 = strings.TrimSuffix(romBase, filepath.Ext(romBase)) + ".state1"
	}
	slotPath := filepath.Join(dir, slot1)
	if err := os.WriteFile(slotPath, []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, why := RetireAutoStateAfterRestore(client, cfg, romPath)
	if r || why != "no-auto-state" {
		t.Fatalf("no auto-state present must be a no-op: %v %q", r, why)
	}
	if b, _ := os.ReadFile(slotPath); string(b) != "KEEP" {
		t.Fatal("manual slot touched when there was no auto-state")
	}
}
