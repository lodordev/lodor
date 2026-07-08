//go:build !onion && !muos && !knulli && !android && !lodorandroid

package main

// THE SYNTHETIC USER-CARD FIXTURE (C1 design §8 / C2 scope item 5) — the
// beta-gate proof that merge-as-default cannot damage a user's card.
//
// A card full of the user's OWN content (every §8 tripwire: an adoptable exact
// match, a romhack, a 0-byte decoy at a canonical name, a real-bytes ✘-marker
// decoy, their box art, a same-tag sibling folder, a bare-TAG folder, a no-Emu-
// pak platform folder, their Collections + map.txt) goes through the REAL engine
// paths against a fake RomM: mirror → refresh → download → evict → uninstall.
// After EVERY stage, the sha256 + path inventory of every user file must be
// EXACTLY the baseline — byte-identical, never renamed, never moved.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"lodor/catalog"
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

var fixtureRomBytes = []byte("ZELDA-REAL-ROM-BYTES-FROM-SERVER")

// fixtureServer serves the full mirror surface: platforms, roms per platform,
// collections, saves (empty), and one downloadable game (Zelda, id 300).
func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	roms := map[string][]romm.Rom{
		"1": { // gba
			{ID: 100, PlatformFsSlug: "gba", FsName: "Pokemon - Emerald Version (USA).gba",
				Files: []romm.RomFile{{FileName: "Pokemon - Emerald Version (USA).gba"}}},
			{ID: 200, PlatformFsSlug: "gba", FsName: "Mario Kart.gba",
				Files: []romm.RomFile{{FileName: "Mario Kart.gba"}}},
			{ID: 300, PlatformFsSlug: "gba", FsName: "Zelda.gba",
				Files: []romm.RomFile{{FileName: "Zelda.gba"}}},
			{ID: 400, PlatformFsSlug: "gba", FsName: "Broken Copy.gba",
				Files: []romm.RomFile{{FileName: "Broken Copy.gba"}}},
		},
		"2": { // snes
			{ID: 500, PlatformFsSlug: "snes", FsName: "Super Mario World.smc",
				Files: []romm.RomFile{{FileName: "Super Mario World.smc"}}},
		},
		"3": { // pico-8 — mapped platform with NO emu pak on the card (V1 scoping)
			{ID: 600, PlatformFsSlug: "pico-8", FsName: "Celeste.p8",
				Files: []romm.RomFile{{FileName: "Celeste.p8"}}},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/platforms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]romm.Platform{
			{ID: 1, FsSlug: "gba", Name: "Game Boy Advance", RomCount: 4},
			{ID: 2, FsSlug: "snes", Name: "Super Nintendo", RomCount: 1},
			{ID: 3, FsSlug: "pico-8", Name: "PICO-8", RomCount: 1},
		})
	})
	mux.HandleFunc("/api/roms", func(w http.ResponseWriter, r *http.Request) {
		items := roms[r.URL.Query().Get("platform_ids")]
		_ = json.NewEncoder(w).Encode(romm.PaginatedRoms{Items: items, Total: len(items)})
	})
	mux.HandleFunc("/api/roms/300", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(roms["1"][2])
	})
	// rom 400 IS servable — the user's 0-byte "Broken Copy.gba" must be refused by
	// the V5 ownership gate, not by a resolve/fetch failure.
	mux.HandleFunc("/api/roms/400", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(roms["1"][3])
	})
	mux.HandleFunc("/api/roms/400/content/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("SERVER-COPY-MUST-NEVER-LAND"))
	})
	mux.HandleFunc("/api/roms/300/content/Zelda.gba", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(fixtureRomBytes)))
		_, _ = w.Write(fixtureRomBytes)
	})
	mux.HandleFunc("/api/collections", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]romm.Collection{{Name: "Server Picks", RomIDs: []int{100, 300}}})
	})
	mux.HandleFunc("/api/saves", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]romm.Save{})
	})
	return httptest.NewServer(mux)
}

// userTreeSnapshot walks the card and returns path -> sha256 for every USER file
// (everything laid down by buildUserCard). Lodor artifacts are excluded by
// construction: the snapshot is taken BEFORE any engine run and compared as an
// exact subset afterwards — same paths, same bytes.
func snapshotFiles(t *testing.T, base string, paths []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(base, rel))
		if err != nil {
			t.Fatalf("snapshot %s: %v", rel, err)
		}
		h := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(h[:])
	}
	return out
}

func assertUserTreeIdentical(t *testing.T, stage, base string, baseline map[string]string) {
	t.Helper()
	for rel, want := range baseline {
		data, err := os.ReadFile(filepath.Join(base, rel))
		if err != nil {
			t.Errorf("[%s] user file MISSING: %s (%v)", stage, rel, err)
			continue
		}
		h := sha256.Sum256(data)
		if got := hex.EncodeToString(h[:]); got != want {
			t.Errorf("[%s] user file MUTATED: %s", stage, rel)
		}
	}
}

// lodorInventory lists every on-card file that is NOT in the user baseline —
// the mirror's own artifacts — for the uninstall-leaves-nothing assertion.
func lodorInventory(t *testing.T, base string, baseline map[string]string) []string {
	t.Helper()
	var extra []string
	for _, top := range []string{"Roms", "Collections"} {
		root := filepath.Join(base, top)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			if _, user := baseline[rel]; !user {
				extra = append(extra, rel)
			}
			return nil
		})
	}
	sort.Strings(extra)
	return extra
}

func TestMergeFixtureUserCardSurvivesEverything(t *testing.T) {
	srv := fixtureServer(t)
	defer srv.Close()

	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_HOST_OS", "nextui")
	pak := filepath.Join(base, "Tools", "tg5040", "Lodor.pak")
	t.Setenv("LODOR_PAK_DIR", pak)
	if err := os.MkdirAll(pak, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(pak)
	// Emu paks: GBA + SFC installed; NO P8 pak (pico-8 is the V1 scoping proof).
	for _, tag := range []string{"GBA", "SFC"} {
		if err := os.MkdirAll(filepath.Join(base, ".system", "tg5040", "paks", "Emus", tag+".pak"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// ---- The user's card (§8 fixture) ----
	userFiles := map[string]string{
		"Roms/Game Boy Advance (GBA)/Pokemon - Emerald Version (USA).gba":        "USER-EMERALD",         // adopted
		"Roms/Game Boy Advance (GBA)/My Romhack.gba":                             "USER-ROMHACK",         // no server match
		"Roms/Game Boy Advance (GBA)/Broken Copy.gba":                            "",                     // 0-byte decoy
		"Roms/Game Boy Advance (GBA)/.media/Pokemon - Emerald Version (USA).png": "USER-BOXART",          // their art
		"Roms/Game Boy Advance (GBA)/✘ Decoy.gba":                                "REAL-BYTES-LOOKALIKE", // marker decoy, not a server rom
		"Roms/GBA (GBA)/Mario Kart.gba":                                          "USER-MARIOKART",       // same-tag sibling
		"Roms/SFC/Super Mario World.smc":                                         "USER-SMW",             // bare-TAG folder
		"Roms/PICO (P8)/celeste-fanport.p8":                                      "USER-PICO",            // no-pak platform
		"Roms/PICO (P8)/placeholder.p8":                                          "",                     // user 0-byte in no-pak folder (V1 tripwire)
		"Collections/My Favorites.txt":                                           "/Roms/GBA (GBA)/Mario Kart.gba\n",
		"Collections/map.txt":                                                    "My Favorites\tThe Good Stuff\n",
	}
	for rel, content := range userFiles {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rels := make([]string, 0, len(userFiles))
	for rel := range userFiles {
		rels = append(rels, rel)
	}
	baseline := snapshotFiles(t, base, rels)

	// Fresh card: NO mappings — the first mirror generates them (adopt-by-tag).
	cfg := &config.Config{
		Hosts:      []config.Host{{RootURI: srv.URL, Token: "t", DeviceID: "dev-1"}},
		MirrorMode: config.MirrorModeMerge, // explicit (onboarding writes it)
	}
	client := romm.NewClient(cfg.Hosts[0], 5*time.Second)

	run := func(stage string, f func()) {
		t.Helper()
		f()
		assertUserTreeIdentical(t, stage, base, baseline)
	}

	// ---- Stage 1: first mirror (mappings generate + adopt; stubs land) ----
	run("mirror", func() {
		_, _, _, _, _, adopted, err := catalog.MirrorCatalog(client, cfg, nil, false)
		if err != nil {
			t.Fatalf("mirror: %v", err)
		}
		if adopted != 3 {
			t.Errorf("adopted=%d want 3 (Emerald, sibling Mario Kart, bare-TAG SMW)", adopted)
		}
	})
	// Adoption + stubs assertions.
	gbaDir := filepath.Join(base, "Roms", "Game Boy Advance (GBA)")
	zStub := filepath.Join(gbaDir, platform.MarkerCloud+"Zelda.gba")
	if fi, err := os.Stat(zStub); err != nil || fi.Size() != 0 {
		t.Fatalf("Zelda stub not created: %v", err)
	}
	for _, bad := range []string{
		platform.MarkerCloud + "Pokemon - Emerald Version (USA).gba",
		platform.MarkerCloud + "Mario Kart.gba",
		platform.MarkerCloud + "Broken Copy.gba",
		platform.MarkerCloud + "My Romhack.gba",
	} {
		if _, err := os.Stat(filepath.Join(gbaDir, bad)); !os.IsNotExist(err) {
			t.Errorf("duplicate stub created: %s", bad)
		}
	}
	if id, ok := catalog.ResolveRomID(cfg, filepath.Join(gbaDir, "Pokemon - Emerald Version (USA).gba")); !ok || id != 100 {
		t.Errorf("adopted Emerald resolves (%d,%v), want (100,true)", id, ok)
	}
	if id, ok := catalog.ResolveRomID(cfg, filepath.Join(base, "Roms", "GBA (GBA)", "Mario Kart.gba")); !ok || id != 200 {
		t.Errorf("sibling Mario Kart resolves (%d,%v), want (200,true)", id, ok)
	}
	// V1 scoping: the no-pak PICO platform pruned NO user files (both survive via
	// baseline check) and the user's folder itself survives.
	if _, err := os.Stat(filepath.Join(base, "Roms", "PICO (P8)")); err != nil {
		t.Errorf("user PICO folder removed by unplayable-prune: %v", err)
	}

	// Stale mapping injection (V1 scoping proof): an old config once mapped
	// pico-8 while a P8 pak existed; the pak is gone now — the next refresh runs
	// pruneUnplayableStubs over the USER's folder and must take nothing.
	cfg.DirectoryMappings["pico-8"] = config.DirMapping{Slug: "pico-8", RelativePath: "PICO (P8)"}

	// ---- Stage 2: collections mirror + refresh (idempotence + V2) ----
	run("collections", func() {
		if _, _, _, _, err := catalog.MirrorCollections(client, cfg, nil); err != nil {
			t.Fatalf("collections: %v", err)
		}
	})
	if _, err := os.Stat(filepath.Join(base, "Collections", "Server Picks.txt")); err != nil {
		t.Errorf("server collection not written: %v", err)
	}
	run("refresh", func() {
		if _, _, _, _, _, _, err := catalog.MirrorCatalog(client, cfg, nil, false); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if _, _, _, _, err := catalog.MirrorCollections(client, cfg, nil); err != nil {
			t.Fatalf("refresh collections: %v", err)
		}
	})

	// ---- Stage 3: download a Lodor game (the real downloadRomCore) + evict ----
	run("download", func() {
		if !downloadRomCore(client, cfg, zStub) {
			t.Fatalf("download of Zelda stub failed")
		}
	})
	if data, err := os.ReadFile(zStub); err != nil || string(data) != string(fixtureRomBytes) {
		t.Fatalf("downloaded bytes wrong (err=%v)", err)
	}
	run("evict", func() {
		evicted, reason := catalog.EvictToStub(cfg, zStub)
		if !evicted {
			t.Fatalf("evict refused: %s", reason)
		}
	})
	if fi, err := os.Stat(zStub); err != nil || fi.Size() != 0 {
		t.Fatalf("evict did not restore the 0-byte stub: %v", err)
	}
	// The V3 kill-shot inside the fixture: evicting the ADOPTED user file refuses.
	run("evict-user-file", func() {
		evicted, reason := catalog.EvictToStub(cfg, filepath.Join(gbaDir, "Pokemon - Emerald Version (USA).gba"))
		if evicted || reason != "not-lodor-managed" {
			t.Fatalf("evict on user file = (%v,%s)", evicted, reason)
		}
	})
	// And the V5 kill-shot: downloading over the user's 0-byte decoy refuses.
	run("download-user-decoy", func() {
		if downloadRomCore(client, cfg, filepath.Join(gbaDir, "Broken Copy.gba")) {
			t.Fatalf("download filled the user's 0-byte file (V5)")
		}
	})

	// ---- Stage 4: mode flip merge -> separate -> merge (migration both ways) ----
	run("flip-to-separate", func() {
		cfg.MirrorMode = config.MirrorModeSeparate
		migrateMirrorLayoutIfNeeded(client, cfg)
		if _, _, _, _, _, _, err := catalog.MirrorCatalog(client, cfg, nil, false); err != nil {
			t.Fatalf("separate mirror: %v", err)
		}
	})
	run("flip-back-to-merge", func() {
		cfg.MirrorMode = config.MirrorModeMerge
		migrateMirrorLayoutIfNeeded(client, cfg)
		if _, _, _, _, _, _, err := catalog.MirrorCatalog(client, cfg, nil, false); err != nil {
			t.Fatalf("merge re-mirror: %v", err)
		}
	})

	// ---- Stage 5: uninstall — the card ends EXACTLY as it started ----
	run("uninstall", func() {
		res := catalog.UninstallMirror(cfg, true)
		if !res.Ok {
			t.Fatalf("uninstall not ok: %+v", res)
		}
	})
	if extra := lodorInventory(t, base, baseline); len(extra) != 0 {
		t.Errorf("Lodor artifacts survived uninstall: %v", extra)
	}
	// Corrupt-manifest run deletes nothing (fail-safe, §3.1) — re-mirror, corrupt,
	// then hit every destructive path.
	if _, _, _, _, _, _, err := catalog.MirrorCatalog(client, cfg, nil, false); err != nil {
		t.Fatalf("re-mirror: %v", err)
	}
	if err := os.WriteFile(platform.ManifestPath(), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("corrupt-manifest", func() {
		if ok, _ := catalog.EvictToStub(cfg, filepath.Join(gbaDir, "Pokemon - Emerald Version (USA).gba")); ok {
			t.Fatalf("corrupt-manifest evict destroyed a file")
		}
		if res := catalog.UninstallMirror(cfg, true); res.Ok || res.Removed != 0 {
			t.Fatalf("corrupt-manifest uninstall removed things: %+v", res)
		}
	})

	// Final belt: the user path INVENTORY is exactly the baseline set — nothing
	// renamed into or out of existence.
	for rel := range baseline {
		if _, err := os.Stat(filepath.Join(base, rel)); err != nil {
			t.Errorf("final inventory missing %s", rel)
		}
	}
}
