package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lodor/config"
	"lodor/romm"
)

// continueFake is a romClient that ALSO satisfies the GetPlatforms + GetSaves
// assertions the Continue mirror makes, driving MirrorCollections end-to-end
// without a live server.
type continueFake struct {
	platforms []romm.Platform
	saves     map[int][]romm.Save // platform id -> its saves
	cols      []romm.Collection
}

func (f *continueFake) GetRoms(q romm.GetRomsQuery) (romm.PaginatedRoms, error) {
	return romm.PaginatedRoms{}, nil
}
func (f *continueFake) GetCollections() ([]romm.Collection, error) { return f.cols, nil }
func (f *continueFake) DownloadCover(p string) ([]byte, error)     { return nil, os.ErrNotExist }
func (f *continueFake) DownloadCoverCtx(_ context.Context, p string) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (f *continueFake) GetPlatforms() ([]romm.Platform, error) { return f.platforms, nil }
func (f *continueFake) GetSaves(q romm.SaveQuery) ([]romm.Save, error) {
	return f.saves[q.PlatformID], nil
}

// continueTestEnv builds a card with a mapped GBA folder, N on-card stub files
// (rom ids 1..n), and a catalog index whose by_id points at them. Returns the
// cfg, the platform folder, and the SDCARD-relative path for a rom id.
func continueTestEnv(t *testing.T, n int) (*config.Config, string, func(id int) string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	t.Setenv("LODOR_PAK_DIR", base)

	cfg := &config.Config{
		DirectoryMappings: map[string]config.DirMapping{"gba": {RelativePath: "Game Boy Advance (GBA)"}},
	}
	dir := filepath.Join(base, "Roms", "Game Boy Advance (GBA)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rel := func(id int) string {
		return "/" + filepath.Join("Roms", "Game Boy Advance (GBA)", fmt.Sprintf("✘ Game %d.gba", id))
	}
	pi := platformIndex{ByBasename: map[string]int{}, ByFsname: map[string]int{}, ByID: map[int]string{}}
	for id := 1; id <= n; id++ {
		if err := os.WriteFile(filepath.Join(base, rel(id)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		pi.ByID[id] = rel(id)
	}
	idx := index{Version: 1, Platforms: map[string]platformIndex{"gba": pi}}
	if err := writeIndexAtomic(IndexPath(cfg), idx); err != nil {
		t.Fatal(err)
	}
	return cfg, dir, rel
}

func at(h int) time.Time { return time.Date(2026, 7, 1, h, 0, 0, 0, time.UTC) }

// TestContinueCollectionNewestFirstDedupedGhostFree is the core contract: the
// Continue collection lists each game ONCE, ordered by its newest CROSS-DEVICE
// server save, ghosts never drive recency, and a game whose on-card file is
// missing is skipped — all riding the normal MirrorCollections pass (which must
// also keep pruning stale collection files WITHOUT pruning Continue itself).
func TestContinueCollectionNewestFirstDedupedGhostFree(t *testing.T) {
	cfg, _, rel := continueTestEnv(t, 2)
	// rom 3 has a save but NO on-card file: patch it into the index pointing at a
	// path that doesn't exist.
	idx, _ := loadIndex(IndexPath(cfg))
	pi := idx.Platforms["gba"]
	pi.ByID[3] = "/" + filepath.Join("Roms", "Game Boy Advance (GBA)", "✘ Game 3.gba")
	idx.Platforms["gba"] = pi
	if err := writeIndexAtomic(IndexPath(cfg), idx); err != nil {
		t.Fatal(err)
	}

	fake := &continueFake{
		platforms: []romm.Platform{{ID: 10, FsSlug: "gba", Name: "Game Boy Advance"}},
		saves: map[int][]romm.Save{10: {
			{ID: 1, RomID: 1, FileSizeBytes: 100, UpdatedAt: at(3)}, // rom1 newest real
			{ID: 2, RomID: 1, FileSizeBytes: 100, UpdatedAt: at(0)}, // rom1 older (dedupe)
			{ID: 3, RomID: 2, FileSizeBytes: 100, UpdatedAt: at(1)}, // rom2 real
			{ID: 4, RomID: 2, FileSizeBytes: 0, UpdatedAt: at(5)},   // rom2 GHOST — must not promote
			{ID: 5, RomID: 3, FileSizeBytes: 100, UpdatedAt: at(2)}, // rom3 — file missing on card
		}},
		cols: []romm.Collection{{Name: "Favorites", RomIDs: []int{1}}},
	}

	colDir := filepath.Join(os.Getenv("SDCARD_PATH"), "Collections")
	if err := os.MkdirAll(colDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(colDir, "Stale.txt"), []byte("/Roms/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The prune is manifest-scoped now (STEP 0b -> C2): only a file the mirror itself
	// wrote on a previous pass may be removed. Claim Stale.txt (via the legacy-ledger
	// import path) so this test keeps proving the prune path; the user-data-survives
	// contract has its own test (collections_prune_test.go).
	seedLegacyLedger(t, "Stale.txt")

	written, _, _, cont, err := MirrorCollections(fake, cfg, nil)
	if err != nil {
		t.Fatalf("MirrorCollections: %v", err)
	}
	if written != 1 {
		t.Fatalf("written=%d want 1 (Favorites)", written)
	}
	wantCont := 2
	if !hostUsesContinueFile { // #187: no collection file on muOS builds
		wantCont = 0
	}
	if cont != wantCont {
		t.Fatalf("cont=%d want %d (rom1 + rom2; rom3 skipped, ghost ignored)", cont, wantCont)
	}
	if !hostUsesContinueFile {
		return
	}
	data, rerr := os.ReadFile(filepath.Join(colDir, "0) Continue.txt"))
	if rerr != nil {
		t.Fatalf("Continue collection not written: %v", rerr)
	}
	want := rel(1) + "\n" + rel(2) + "\n" // rom1 (newest real save) first
	if string(data) != want {
		t.Fatalf("Continue content = %q, want %q", data, want)
	}
	// Prune removed the stale file but spared Continue + Favorites.
	if _, err := os.Stat(filepath.Join(colDir, "Stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale collection not pruned")
	}
	if _, err := os.Stat(filepath.Join(colDir, "Favorites.txt")); err != nil {
		t.Fatalf("Favorites pruned or unwritten: %v", err)
	}
}

// TestContinueEmptyFeedRemovesFile: no server saves -> NO Continue file, and a
// leftover from a previous run is pruned (the chosen empty-feed UX: the browser
// shows no stale "Continue" entry at all).
func TestContinueEmptyFeedRemovesFile(t *testing.T) {
	cfg, _, _ := continueTestEnv(t, 1)
	fake := &continueFake{
		platforms: []romm.Platform{{ID: 10, FsSlug: "gba"}},
		saves:     map[int][]romm.Save{},
		cols:      []romm.Collection{{Name: "Favorites", RomIDs: []int{1}}},
	}
	colDir := filepath.Join(os.Getenv("SDCARD_PATH"), "Collections")
	if err := os.MkdirAll(colDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(colDir, "0) Continue.txt"), []byte("/Roms/old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A real previous pass would have recorded its Continue file in the ownership
	// record (STEP 0b ledger, imported into the manifest) — seed it, or the scoped
	// prune (correctly) refuses to touch it.
	seedLegacyLedger(t, "0) Continue.txt")
	_, _, _, cont, err := MirrorCollections(fake, cfg, nil)
	if err != nil {
		t.Fatalf("MirrorCollections: %v", err)
	}
	if cont != 0 {
		t.Fatalf("cont=%d want 0", cont)
	}
	if _, err := os.Stat(filepath.Join(colDir, "0) Continue.txt")); !os.IsNotExist(err) {
		t.Fatalf("empty feed must remove the previous Continue file")
	}
}

// TestContinueCapsAtTwelve: more recent games than the cap -> exactly 12 entries,
// the 12 newest.
func TestContinueCapsAtTwelve(t *testing.T) {
	const n = 15
	cfg, _, rel := continueTestEnv(t, n)
	saves := make([]romm.Save, 0, n)
	for id := 1; id <= n; id++ {
		saves = append(saves, romm.Save{ID: id, RomID: id, FileSizeBytes: 10, UpdatedAt: at(id)})
	}
	fake := &continueFake{
		platforms: []romm.Platform{{ID: 10, FsSlug: "gba"}},
		saves:     map[int][]romm.Save{10: saves},
	}
	_, _, _, cont, err := MirrorCollections(fake, cfg, nil)
	if err != nil {
		t.Fatalf("MirrorCollections: %v", err)
	}
	wantCont := continueCap
	if !hostUsesContinueFile { // #187: no collection file on muOS builds
		wantCont = 0
	}
	if cont != wantCont {
		t.Fatalf("cont=%d want %d", cont, wantCont)
	}
	if !hostUsesContinueFile {
		return
	}
	data, rerr := os.ReadFile(filepath.Join(os.Getenv("SDCARD_PATH"), "Collections", "0) Continue.txt"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != continueCap {
		t.Fatalf("lines=%d want %d", len(lines), continueCap)
	}
	if lines[0] != rel(n) || lines[continueCap-1] != rel(n-continueCap+1) {
		t.Fatalf("order wrong: first=%q last=%q", lines[0], lines[continueCap-1])
	}
}

// ---------------------------------------------------------------------------------
// Cross-device recents merge (task #132) + the light Sync-now refresh (task #133).
// ---------------------------------------------------------------------------------

// minuiDir creates the MinUI-family shared recents dir on the fake card (the
// capability gate MergeRecents checks) and returns the recent.txt path.
func minuiDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("SDCARD_PATH"), ".userdata", "shared", ".minui")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "recent.txt")
}

// TestMergeRecentsAppendsBelowLocalDeduped is the merge contract: local lines keep
// their exact order AND aliases, server-only entries append below newest-first,
// entries already present are never duplicated (NextUI does not dedupe on read).
func TestMergeRecentsAppendsBelowLocalDeduped(t *testing.T) {
	_, _, rel := continueTestEnv(t, 3)
	rp := minuiDir(t)
	local := "/Roms/Game Boy (GB)/Tetris.gb\tTetris\n" + rel(2) + "\n"
	if err := os.WriteFile(rp, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	merged, total := MergeRecents([]string{rel(1), rel(2), rel(3)}) // rel(2) already local
	if merged != 2 || total != 4 {
		t.Fatalf("merged=%d total=%d want 2/4", merged, total)
	}
	data, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	want := "/Roms/Game Boy (GB)/Tetris.gb\tTetris\n" + rel(2) + "\n" + rel(1) + "\n" + rel(3) + "\n"
	if string(data) != want {
		t.Fatalf("recent.txt = %q, want %q", data, want)
	}
}

// TestMergeRecentsDropsDispatcherDummies (B3): the GM/Continue root-entry dummy
// rows are affordances the pak scrubs from recents — a merge must never carry
// them back into the file, while real rows survive verbatim.
func TestMergeRecentsDropsDispatcherDummies(t *testing.T) {
	_, _, rel := continueTestEnv(t, 1)
	rp := minuiDir(t)
	local := "/Roms/0) Continue (LODORCT)/Continue.ct\tContinue\n" +
		"/Roms/Game Manager (LODORGM)/Open Game Manager.gm\n" +
		"/Roms/Game Boy (GB)/Tetris.gb\tTetris\n"
	if err := os.WriteFile(rp, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	merged, total := MergeRecents([]string{rel(1)})
	if merged != 1 || total != 2 {
		t.Fatalf("merged=%d total=%d want 1/2 (dummies dropped)", merged, total)
	}
	data, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	want := "/Roms/Game Boy (GB)/Tetris.gb\tTetris\n" + rel(1) + "\n"
	if string(data) != want {
		t.Fatalf("recent.txt = %q, want %q", data, want)
	}
}

// TestMergeRecentsCapAndLocalPriority: injection never evicts local lines and the
// file never exceeds the host's MAX_RECENTS (24).
func TestMergeRecentsCapAndLocalPriority(t *testing.T) {
	_, _, rel := continueTestEnv(t, 3)
	rp := minuiDir(t)
	var local strings.Builder
	for i := 0; i < nextuiMaxRecents-1; i++ { // 23 local lines, room for exactly 1
		fmt.Fprintf(&local, "/Roms/Game Boy (GB)/Local %02d.gb\n", i)
	}
	if err := os.WriteFile(rp, []byte(local.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, total := MergeRecents([]string{rel(1), rel(2), rel(3)})
	if merged != 1 || total != nextuiMaxRecents {
		t.Fatalf("merged=%d total=%d want 1/%d", merged, total, nextuiMaxRecents)
	}
	data, _ := os.ReadFile(rp)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != nextuiMaxRecents {
		t.Fatalf("lines=%d want %d", len(lines), nextuiMaxRecents)
	}
	if lines[0] != "/Roms/Game Boy (GB)/Local 00.gb" || lines[nextuiMaxRecents-1] != rel(1) {
		t.Fatalf("local priority broken: first=%q last=%q", lines[0], lines[nextuiMaxRecents-1])
	}
}

// TestMergeRecentsNoMinuiDirNoop: not a MinUI-family card -> (0,0) and no file
// sprayed onto the layout.
func TestMergeRecentsNoMinuiDirNoop(t *testing.T) {
	_, _, rel := continueTestEnv(t, 1)
	merged, total := MergeRecents([]string{rel(1)})
	if merged != 0 || total != 0 {
		t.Fatalf("merged=%d total=%d want 0/0", merged, total)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("SDCARD_PATH"), ".userdata", "shared", ".minui", "recent.txt")); !os.IsNotExist(err) {
		t.Fatalf("recent.txt must not be created on a non-MinUI card")
	}
}

// TestMergeRecentsNothingNewLeavesFileAlone: all entries already present -> merged=0
// and the file bytes are untouched (no pointless FAT32 rewrite).
func TestMergeRecentsNothingNewLeavesFileAlone(t *testing.T) {
	_, _, rel := continueTestEnv(t, 1)
	rp := minuiDir(t)
	orig := rel(1) + "\talias kept\n"
	if err := os.WriteFile(rp, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, total := MergeRecents([]string{rel(1)})
	if merged != 0 || total != 1 {
		t.Fatalf("merged=%d total=%d want 0/1", merged, total)
	}
	data, _ := os.ReadFile(rp)
	if string(data) != orig {
		t.Fatalf("file rewritten: %q want %q", data, orig)
	}
}

// TestSyncContinueLight is the --sync-continue contract (task #133): Continue file +
// recents merge from the LOCAL index only — and, unlike the full mirror, sibling
// collections are NEVER pruned and an empty feed leaves the existing Continue alone.
func TestSyncContinueLight(t *testing.T) {
	cfg, _, rel := continueTestEnv(t, 2)
	rp := minuiDir(t)
	fake := &continueFake{
		platforms: []romm.Platform{{ID: 10, FsSlug: "gba"}},
		saves: map[int][]romm.Save{10: {
			{ID: 1, RomID: 1, FileSizeBytes: 100, UpdatedAt: at(2)},
			{ID: 2, RomID: 2, FileSizeBytes: 100, UpdatedAt: at(1)},
		}},
	}
	colDir := filepath.Join(os.Getenv("SDCARD_PATH"), "Collections")
	if err := os.MkdirAll(colDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(colDir, "Sibling.txt"), []byte("/Roms/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, merged, total := SyncContinue(fake, cfg)
	// #187: muOS builds deliver Continue via the native History menu and write
	// NO collection file — entries reports 0 there by design.
	wantEntries := 2
	if !hostUsesContinueFile {
		wantEntries = 0
	}
	if entries != wantEntries || merged != 2 || total != 2 {
		t.Fatalf("entries=%d merged=%d total=%d want %d/2/2", entries, merged, total, wantEntries)
	}
	if _, err := os.Stat(filepath.Join(colDir, "Sibling.txt")); err != nil {
		t.Fatalf("light mode must NOT prune sibling collections: %v", err)
	}
	if hostUsesContinueFile {
		data, err := os.ReadFile(filepath.Join(colDir, "0) Continue.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if want := rel(1) + "\n" + rel(2) + "\n"; string(data) != want {
			t.Fatalf("Continue = %q want %q", data, want)
		}
	} else {
		if _, err := os.Stat(filepath.Join(colDir, "0) Continue.txt")); err == nil {
			t.Fatal("muOS build wrote the MinUI Continue collection file — #187 stray")
		}
	}
	if rdata, _ := os.ReadFile(rp); string(rdata) != rel(1)+"\n"+rel(2)+"\n" {
		t.Fatalf("recents = %q", rdata)
	}

	// Empty feed: existing Continue file survives a light refresh (transient-empty
	// must not erase a good list — only the full mirror's prune retires it).
	empty := &continueFake{platforms: []romm.Platform{{ID: 10, FsSlug: "gba"}}, saves: map[int][]romm.Save{}}
	entries, merged, _ = SyncContinue(empty, cfg)
	if entries != 0 || merged != 0 {
		t.Fatalf("empty feed: entries=%d merged=%d want 0/0", entries, merged)
	}
	if hostUsesContinueFile {
		if _, err := os.Stat(filepath.Join(colDir, "0) Continue.txt")); err != nil {
			t.Fatalf("light empty feed must leave the Continue file: %v", err)
		}
	}
}
