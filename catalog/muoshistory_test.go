// Unit tests for the muOS native-History injector (task #181). The core
// (buildMuosHistTarget / injectMuosHistory) is dir-parameterized and tag-free, so
// these run on EVERY build tag — only the muosHistoryEnabled gate differs per tag
// (asserted in the per-tag gate tests).
package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lodor/platform"
)

// cFNV1a32 re-implements muOS frontend content.c fnv_hash_str LITERALLY (offset
// basis 2166136261, prime 16777619, xor-then-multiply) so the test proves our
// hash/fnv-based filename matches what muOS itself would write.
func cFNV1a32(s string) uint32 {
	hash := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		hash ^= uint32(s[i])
		hash *= 16777619
	}
	return hash
}

func testManifest() *platform.Manifest {
	return &platform.Manifest{Version: 1, Entries: map[string]platform.ManifestEntry{}}
}

func TestBuildMuosHistTargetFormat(t *testing.T) {
	ts := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	tgt, ok := buildMuosHistTarget("/mnt/mmc/ROMS", "/Roms/Sega Game Gear/✘ Aladdin (USA).gg", ts)
	if !ok {
		t.Fatal("target not built for a valid rom rel")
	}
	wantPath := "/mnt/mmc/ROMS/Sega Game Gear/✘ Aladdin (USA).gg"
	if tgt.path != wantPath {
		t.Fatalf("line1 path = %q, want %q (must be the REAL ROMS mount, native case)", tgt.path, wantPath)
	}
	// The exact 3-line, no-trailing-newline shape content.c writes.
	wantContent := wantPath + "\nSega Game Gear\n✘ Aladdin (USA)"
	if tgt.content != wantContent {
		t.Fatalf("content = %q, want %q", tgt.content, wantContent)
	}
	// Filename hash must equal the C algorithm's output over line 1.
	wantF := "✘ Aladdin (USA)-" + upperHex8(t, cFNV1a32(wantPath)) + ".cfg"
	if tgt.fname != wantF {
		t.Fatalf("fname = %q, want %q (FNV-1a mismatch with muOS)", tgt.fname, wantF)
	}
	if !tgt.t.Equal(ts) {
		t.Fatalf("t = %v, want feed time %v", tgt.t, ts)
	}
}

func upperHex8(t *testing.T, v uint32) string {
	t.Helper()
	const hex = "0123456789ABCDEF"
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = hex[v&0xF]
		v >>= 4
	}
	return string(b[:])
}

func TestBuildMuosHistTargetNestedAndRejects(t *testing.T) {
	ts := time.Now()
	tgt, ok := buildMuosHistTarget("/mnt/mmc/ROMS", "/Roms/Sega Game Gear/USA/Game.gg", ts)
	if !ok {
		t.Fatal("nested system subfolder rejected")
	}
	if want := "/mnt/mmc/ROMS/Sega Game Gear/USA/Game.gg\nSega Game Gear/USA\nGame"; tgt.content != want {
		t.Fatalf("nested content = %q, want %q", tgt.content, want)
	}
	for _, rel := range []string{
		"/Collections/0) Continue.txt", // not under Roms/
		"/Roms/Loose.gg",               // no system folder — muOS can't resolve a system
		"/Roms/Sega Game Gear/",        // no filename
		"",
	} {
		if _, ok := buildMuosHistTarget("/mnt/mmc/ROMS", rel, ts); ok {
			t.Fatalf("rel %q must be rejected", rel)
		}
	}
}

func TestInjectMuosHistoryWritesContentAndMtimeOrder(t *testing.T) {
	dir := t.TempDir()
	roms := "/mnt/mmc/ROMS"
	man := testManifest()
	t1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	entries := []ContinueEntry{
		{Rel: "/Roms/Sega Game Gear/Real Game (USA).gg", T: t2}, // newest first, like the feed
		{Rel: "/Roms/Sega Game Gear/✘ Zilion (USA).gg", T: t1},
	}
	wrote, rekeyed, skipped := injectMuosHistory(dir, roms, entries, man)
	if wrote != 2 || rekeyed != 0 || skipped != 0 {
		t.Fatalf("wrote/rekeyed/skipped = %d/%d/%d, want 2/0/0", wrote, rekeyed, skipped)
	}
	newer := filepath.Join(dir, "Real Game (USA)-"+upperHex8(t, cFNV1a32(roms+"/Sega Game Gear/Real Game (USA).gg"))+".cfg")
	older := filepath.Join(dir, "✘ Zilion (USA)-"+upperHex8(t, cFNV1a32(roms+"/Sega Game Gear/✘ Zilion (USA).gg"))+".cfg")
	nb, err := os.ReadFile(newer)
	if err != nil {
		t.Fatalf("newest entry not written: %v", err)
	}
	if want := roms + "/Sega Game Gear/Real Game (USA).gg\nSega Game Gear\nReal Game (USA)"; string(nb) != want {
		t.Fatalf("content = %q, want %q", nb, want)
	}
	nfi, err := os.Stat(newer)
	if err != nil {
		t.Fatal(err)
	}
	ofi, err := os.Stat(older)
	if err != nil {
		t.Fatalf("older entry not written: %v", err)
	}
	if !nfi.ModTime().Equal(t2) || !ofi.ModTime().Equal(t1) {
		t.Fatalf("mtimes = %v / %v, want the FEED times %v / %v (#147: never local now())",
			nfi.ModTime(), ofi.ModTime(), t2, t1)
	}
	if !nfi.ModTime().After(ofi.ModTime()) {
		t.Fatal("newest feed entry must carry the newest mtime (muxhistory orders by mtime)")
	}
	for _, p := range []string{newer, older} {
		if !man.OwnsKind(p, platform.ManifestHistory) {
			t.Fatalf("manifest does not own %s as kind history", p)
		}
	}
}

func TestInjectMuosHistoryForeignPreserved(t *testing.T) {
	dir := t.TempDir()
	roms := "/mnt/mmc/ROMS"
	man := testManifest()
	// The user's OWN history pointer for the same game: unmarked rom name, native
	// filename. Its bytes and mtime are sacred.
	path := roms + "/Sega Game Gear/Real Game (USA).gg"
	foreign := filepath.Join(dir, "Real Game (USA)-"+upperHex8(t, cFNV1a32(path))+".cfg")
	body := path + "\nSega Game Gear\nReal Game (USA)"
	if err := os.WriteFile(foreign, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 6, 20, 8, 0, 0, 0, time.UTC)
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatal(err)
	}

	wrote, rekeyed, skipped := injectMuosHistory(dir, roms,
		[]ContinueEntry{{Rel: "/Roms/Sega Game Gear/Real Game (USA).gg", T: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)}}, man)
	if wrote != 0 || rekeyed != 0 || skipped != 1 {
		t.Fatalf("wrote/rekeyed/skipped = %d/%d/%d, want 0/0/1 (foreign recency wins)", wrote, rekeyed, skipped)
	}
	got, err := os.ReadFile(foreign)
	if err != nil || string(got) != body {
		t.Fatalf("foreign pointer bytes changed: %q err=%v", got, err)
	}
	fi, _ := os.Stat(foreign)
	if !fi.ModTime().Equal(old) {
		t.Fatalf("foreign pointer mtime changed: %v, want %v", fi.ModTime(), old)
	}
	if man.Owns(foreign) {
		t.Fatal("foreign pointer must never enter the manifest")
	}
	des, _ := os.ReadDir(dir)
	if len(des) != 1 {
		t.Fatalf("injector added a duplicate row: %d files, want 1", len(des))
	}
}

func TestInjectMuosHistoryRekeysMarkerFlip(t *testing.T) {
	dir := t.TempDir()
	roms := "/mnt/mmc/ROMS"
	man := testManifest()
	t1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	// First sync: the game is a ✘ stub.
	if w, _, _ := injectMuosHistory(dir, roms,
		[]ContinueEntry{{Rel: "/Roms/Sega Game Gear/✘ Aladdin (USA).gg", T: t1}}, man); w != 1 {
		t.Fatalf("seed inject wrote %d, want 1", w)
	}
	oldName := filepath.Join(dir, "✘ Aladdin (USA)-"+upperHex8(t, cFNV1a32(roms+"/Sega Game Gear/✘ Aladdin (USA).gg"))+".cfg")
	if _, err := os.Stat(oldName); err != nil {
		t.Fatal("seed pointer missing")
	}
	// Download-on-launch flipped ✘→✓; the next feed resolves the ✓ name.
	t2 := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	wrote, rekeyed, _ := injectMuosHistory(dir, roms,
		[]ContinueEntry{{Rel: "/Roms/Sega Game Gear/✓ Aladdin (USA).gg", T: t2}}, man)
	if wrote != 1 || rekeyed != 1 {
		t.Fatalf("wrote/rekeyed = %d/%d, want 1/1", wrote, rekeyed)
	}
	if _, err := os.Stat(oldName); !os.IsNotExist(err) {
		t.Fatal("stale ✘ pointer not removed (it is DEAD — muxhistory cannot launch it)")
	}
	if man.Owns(oldName) {
		t.Fatal("manifest still owns the removed stale pointer")
	}
	newName := filepath.Join(dir, "✓ Aladdin (USA)-"+upperHex8(t, cFNV1a32(roms+"/Sega Game Gear/✓ Aladdin (USA).gg"))+".cfg")
	b, err := os.ReadFile(newName)
	if err != nil {
		t.Fatalf("re-keyed pointer missing: %v", err)
	}
	if want := roms + "/Sega Game Gear/✓ Aladdin (USA).gg\nSega Game Gear\n✓ Aladdin (USA)"; string(b) != want {
		t.Fatalf("re-keyed content = %q, want %q", b, want)
	}
	if !man.OwnsKind(newName, platform.ManifestHistory) {
		t.Fatal("re-keyed pointer not recorded")
	}
}

func TestInjectMuosHistoryAdoptsMarkedNativePointer(t *testing.T) {
	dir := t.TempDir()
	roms := "/mnt/mmc/ROMS"
	man := testManifest() // manifest lost/fresh: the pointer below is NOT owned
	// muOS itself wrote this pointer by launching a Lodor ✘ stub natively. A marker
	// name is never the user's own artifact (gamelist's rule) — and after the ✘→✓
	// flip it is unlaunchable, so the injector may re-key it.
	stale := filepath.Join(dir, "✘ Aladdin (USA)-"+upperHex8(t, cFNV1a32(roms+"/Sega Game Gear/✘ Aladdin (USA).gg"))+".cfg")
	body := roms + "/Sega Game Gear/✘ Aladdin (USA).gg\nSega Game Gear\n✘ Aladdin (USA)"
	if err := os.WriteFile(stale, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	wrote, rekeyed, _ := injectMuosHistory(dir, roms,
		[]ContinueEntry{{Rel: "/Roms/Sega Game Gear/✓ Aladdin (USA).gg", T: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)}}, man)
	if wrote != 1 || rekeyed != 1 {
		t.Fatalf("wrote/rekeyed = %d/%d, want 1/1", wrote, rekeyed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale marked pointer not adopted/removed")
	}
}

func TestInjectMuosHistoryNoChurnOnRepeat(t *testing.T) {
	dir := t.TempDir()
	roms := "/mnt/mmc/ROMS"
	man := testManifest()
	entries := []ContinueEntry{{Rel: "/Roms/Sega Game Gear/✘ Zilion (USA).gg", T: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)}}
	if w, _, _ := injectMuosHistory(dir, roms, entries, man); w != 1 {
		t.Fatal("first pass did not write")
	}
	p := filepath.Join(dir, "✘ Zilion (USA)-"+upperHex8(t, cFNV1a32(roms+"/Sega Game Gear/✘ Zilion (USA).gg"))+".cfg")
	fi1, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	wrote, rekeyed, skipped := injectMuosHistory(dir, roms, entries, man)
	if wrote != 0 || rekeyed != 0 || skipped != 1 {
		t.Fatalf("repeat wrote/rekeyed/skipped = %d/%d/%d, want 0/0/1 (no churn)", wrote, rekeyed, skipped)
	}
	fi2, _ := os.Stat(p)
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("repeat pass churned the mtime (false recency)")
	}
}

func TestInjectMuosHistoryEmptyFeedNoop(t *testing.T) {
	dir := t.TempDir()
	man := testManifest()
	if w, r, s := injectMuosHistory(dir, "/mnt/mmc/ROMS", nil, man); w != 0 || r != 0 || s != 0 {
		t.Fatalf("empty feed wrote/rekeyed/skipped = %d/%d/%d, want 0/0/0", w, r, s)
	}
}

// TestInjectMuosHistoryHealsDeadForeignPointer is the #187 heal: the USER'S own
// (unmarked, unowned) pointer whose file_path died in a Lodor rename (here: the
// mirror re-marked the rom ✘) is re-pointed to the live name with the user's
// mtime preserved, instead of stranding a "Could not launch" entry forever.
func TestInjectMuosHistoryHealsDeadForeignPointer(t *testing.T) {
	dir := t.TempDir()
	roms := t.TempDir() // REAL root — the heal stats paths on disk
	man := testManifest()
	sys := filepath.Join(roms, "Sega Game Gear")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	// Live rom on card under the CURRENT (marked) name.
	live := filepath.Join(sys, "✘ Zilion (USA).gg")
	if err := os.WriteFile(live, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The user's pointer from before the rename: clean name, not manifest-owned,
	// file_path no longer exists.
	deadPath := filepath.Join(sys, "Zilion (USA).gg")
	deadFile := filepath.Join(dir, "Zilion (USA)-"+upperHex8(t, cFNV1a32(deadPath))+".cfg")
	if err := os.WriteFile(deadFile, []byte(deadPath+"\nSega Game Gear\nZilion (USA)"), 0o644); err != nil {
		t.Fatal(err)
	}
	userMt := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(deadFile, userMt, userMt); err != nil {
		t.Fatal(err)
	}

	feedT := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	entries := []ContinueEntry{{Rel: "/Roms/Sega Game Gear/✘ Zilion (USA).gg", T: feedT}}
	wrote, rekeyed, skipped := injectMuosHistory(dir, roms, entries, man)
	if wrote != 0 || rekeyed != 1 || skipped != 0 {
		t.Fatalf("wrote/rekeyed/skipped = %d/%d/%d, want 0/1/0 (heal counts as rekeyed)", wrote, rekeyed, skipped)
	}
	if _, err := os.Stat(deadFile); !os.IsNotExist(err) {
		t.Fatalf("dead pointer still present: %v", err)
	}
	healed := filepath.Join(dir, "✘ Zilion (USA)-"+upperHex8(t, cFNV1a32(live))+".cfg")
	hb, err := os.ReadFile(healed)
	if err != nil {
		t.Fatalf("healed pointer missing: %v", err)
	}
	if want := live + "\nSega Game Gear\n✘ Zilion (USA)"; string(hb) != want {
		t.Fatalf("healed content = %q, want %q", hb, want)
	}
	fi, err := os.Stat(healed)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(userMt) {
		t.Fatalf("healed mtime = %v, want the USER'S %v (their recency, not the feed's)", fi.ModTime(), userMt)
	}
	// Ownership NOT taken: the healed file carries a marker, so the NEXT run treats
	// it like any Lodor-marked pointer and restamps it to the feed time — from here
	// on it lives under the normal marked-pointer rules.
	wrote, rekeyed, skipped = injectMuosHistory(dir, roms, entries, man)
	if wrote != 1 || rekeyed != 0 || skipped != 0 {
		t.Fatalf("second run wrote/rekeyed/skipped = %d/%d/%d, want 1/0/0", wrote, rekeyed, skipped)
	}
	if fi, err = os.Stat(healed); err != nil || !fi.ModTime().Equal(feedT) {
		t.Fatalf("second-run mtime = %v err=%v, want feed time %v", fi.ModTime(), err, feedT)
	}
}

// A dead UNMARKED heal target (extension change: stub .gg -> downloaded .zip with
// a clean name) stays the user's: unowned, unmarked, mtime preserved across runs.
func TestInjectMuosHistoryHealStaysForeignWhenUnmarked(t *testing.T) {
	dir := t.TempDir()
	roms := t.TempDir()
	man := testManifest()
	sys := filepath.Join(roms, "Sega Game Gear")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(sys, "Zilion (USA).zip")
	if err := os.WriteFile(live, []byte("rom"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadPath := filepath.Join(sys, "Zilion (USA).gg")
	deadFile := filepath.Join(dir, "Zilion (USA)-"+upperHex8(t, cFNV1a32(deadPath))+".cfg")
	if err := os.WriteFile(deadFile, []byte(deadPath+"\nSega Game Gear\nZilion (USA)"), 0o644); err != nil {
		t.Fatal(err)
	}
	userMt := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	_ = os.Chtimes(deadFile, userMt, userMt)

	entries := []ContinueEntry{{Rel: "/Roms/Sega Game Gear/Zilion (USA).zip", T: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)}}
	if w, r, s := injectMuosHistory(dir, roms, entries, man); w != 0 || r != 1 || s != 0 {
		t.Fatalf("first run = %d/%d/%d, want 0/1/0", w, r, s)
	}
	healed := filepath.Join(dir, "Zilion (USA)-"+upperHex8(t, cFNV1a32(live))+".cfg")
	// Second run: the healed pointer is unmarked + unowned -> foreign -> skipped,
	// mtime untouched (the user's recency survives indefinitely).
	if w, r, s := injectMuosHistory(dir, roms, entries, man); w != 0 || r != 0 || s != 1 {
		t.Fatalf("second run = %d/%d/%d, want 0/0/1", w, r, s)
	}
	if fi, err := os.Stat(healed); err != nil || !fi.ModTime().Equal(userMt) {
		t.Fatalf("mtime = %v err=%v, want preserved %v", fi.ModTime(), err, userMt)
	}
}
