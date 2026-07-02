package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lodor/romm"
)

func TestDisplayNameFor(t *testing.T) {
	cases := []struct{ in, want string }{
		// marker chrome stripped, extension stripped, region parens stripped
		{"/Roms/Game Boy Advance (GBA)/✘ Metroid Fusion (USA).gba", "Metroid Fusion"},
		{"/Roms/Game Boy Advance (GBA)/✓ 007 - Nightfire (E) [!].gba", "007 - Nightfire"},
		{"/Roms/Super Nintendo (SFC)/[v] Chrono Trigger.sfc", "Chrono Trigger"},
		// double extension (NextUI rule: strip while trailing ".xx".."."+4)
		{"/Roms/Pico-8 (P8)/celeste.p8.png", "celeste"},
		// multi-disc folder-as-rom .m3u
		{"/Roms/Sony PlayStation (PS)/Final Fantasy VII (USA).m3u", "Final Fantasy VII"},
		// a name that would nuke to nothing keeps its pre-paren-strip form
		{"/Roms/Arcade (FBN)/(unnamed).zip", "(unnamed)"},
	}
	for _, c := range cases {
		if got := DisplayNameFor(c.in); got != c.want {
			t.Errorf("DisplayNameFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWriteContinueHead pins the dispatcher file contract: "<rel>\t<display>\n"
// per entry, newest first, capped at continueHeadMax (task #135 — multiple lines
// so the dispatcher can fall through past a drifted path), written only when the
// shared Lodor dir exists, and a transient empty feed never erases an existing
// head.
func TestWriteContinueHead(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SDCARD_PATH", base)

	rel := "/Roms/Game Boy Advance (GBA)/✘ Metroid Fusion (USA).gba"

	// no shared Lodor dir -> capability-gated, no write anywhere
	if WriteContinueHead([]string{rel}) {
		t.Fatal("wrote a head file without the shared Lodor dir")
	}

	dir := filepath.Join(base, ".userdata", "shared", "Lodor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !WriteContinueHead([]string{rel, "/Roms/other.gba"}) {
		t.Fatal("head write failed")
	}
	data, err := os.ReadFile(filepath.Join(dir, "continue-head.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), rel+"\tMetroid Fusion\n/Roms/other.gba\tother\n"; got != want {
		t.Fatalf("head = %q, want %q", got, want)
	}

	// the cap: a feed longer than continueHeadMax writes exactly continueHeadMax lines
	long := make([]string, continueHeadMax+3)
	for i := range long {
		long[i] = "/Roms/Game Boy (GB)/game" + string(rune('a'+i)) + ".gb"
	}
	if !WriteContinueHead(long) {
		t.Fatal("long head write failed")
	}
	capped, _ := os.ReadFile(filepath.Join(dir, "continue-head.txt"))
	if got := strings.Count(string(capped), "\n"); got != continueHeadMax {
		t.Fatalf("capped head has %d lines, want %d", got, continueHeadMax)
	}
	if !WriteContinueHead([]string{rel, "/Roms/other.gba"}) {
		t.Fatal("re-write failed")
	}
	data, _ = os.ReadFile(filepath.Join(dir, "continue-head.txt"))

	// empty feed leaves the existing head alone
	if WriteContinueHead(nil) {
		t.Fatal("empty feed claimed a write")
	}
	if data2, _ := os.ReadFile(filepath.Join(dir, "continue-head.txt")); string(data2) != string(data) {
		t.Fatal("empty feed clobbered the head file")
	}
}

// TestUpdateContinueRootLabel pins the map.txt merge contract: gated on the
// LODORCT root folder, foreign lines (including the pak-owned NBSP Game Manager
// alias) preserved byte-verbatim, our line replaced not duplicated, no rewrite
// when already exact.
func TestUpdateContinueRootLabel(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SDCARD_PATH", base)
	roms := filepath.Join(base, "Roms")
	mapPath := filepath.Join(roms, "map.txt")

	// no LODORCT folder -> no-op, no map.txt materializes
	if err := os.MkdirAll(roms, 0o755); err != nil {
		t.Fatal(err)
	}
	if UpdateContinueRootLabel("Metroid Fusion") {
		t.Fatal("labeled without the Continue root folder")
	}
	if _, err := os.Stat(mapPath); err == nil {
		t.Fatal("map.txt appeared without the Continue root folder")
	}

	if err := os.MkdirAll(filepath.Join(roms, continueRootDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	// pre-existing user + pak lines that MUST survive verbatim (GM alias carries
	// a leading NBSP — the bottom-sort trick — treat it as opaque bytes).
	gmLine := "Game Manager (LODORGM)\t Game Manager"
	userLine := "Game Boy Advance (GBA)\tGBA Games"
	if err := os.WriteFile(mapPath, []byte(userLine+"\n"+gmLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !UpdateContinueRootLabel("Metroid Fusion") {
		t.Fatal("label write failed")
	}
	data, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatal(err)
	}
	want := userLine + "\n" + gmLine + "\n" + continueRootDirName + "\t0) Continue: Metroid Fusion\n"
	if string(data) != want {
		t.Fatalf("map.txt = %q, want %q", string(data), want)
	}

	// idempotent: exact content -> no rewrite reported
	if UpdateContinueRootLabel("Metroid Fusion") {
		t.Fatal("rewrote an already-exact map.txt")
	}
	// new game replaces OUR line only, once
	if !UpdateContinueRootLabel("Chrono Trigger") {
		t.Fatal("relabel failed")
	}
	data, _ = os.ReadFile(mapPath)
	if strings.Count(string(data), continueRootDirName) != 1 {
		t.Fatalf("duplicated LODORCT lines: %q", string(data))
	}
	if !strings.Contains(string(data), userLine) || !strings.Contains(string(data), gmLine) {
		t.Fatalf("foreign lines lost: %q", string(data))
	}
	if !strings.Contains(string(data), "0) Continue: Chrono Trigger") {
		t.Fatalf("label not updated: %q", string(data))
	}

	// over-long names truncate with an ellipsis and never break the line shape
	long := strings.Repeat("A", 80)
	if !UpdateContinueRootLabel(long) {
		t.Fatal("long-name label failed")
	}
	data, _ = os.ReadFile(mapPath)
	line := ""
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, continueRootDirName+"\t") {
			line = l
		}
	}
	if line == "" || len(line) > 200 || !strings.HasSuffix(line, "…") {
		t.Fatalf("long label line wrong: %q", line)
	}
}

// TestSyncContinueWritesHead rides the existing continueTestEnv fake end-to-end:
// the light sync must land the newest entry in continue-head.txt and, with the
// LODORCT folder present, refresh the dynamic label in Roms/map.txt.
func TestSyncContinueWritesHead(t *testing.T) {
	cfg, _, rel := continueTestEnv(t, 2)
	base := os.Getenv("SDCARD_PATH")
	if err := os.MkdirAll(filepath.Join(base, ".userdata", "shared", "Lodor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "Roms", continueRootDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &continueFake{
		platforms: []romm.Platform{{ID: 10, FsSlug: "gba"}},
		saves: map[int][]romm.Save{10: {
			{ID: 1, RomID: 1, FileSizeBytes: 10, UpdatedAt: at(9)},
			{ID: 2, RomID: 2, FileSizeBytes: 10, UpdatedAt: at(11)}, // newest -> head
		}},
	}
	entries, _, _ := SyncContinue(fake, cfg)
	if entries != 2 {
		t.Fatalf("entries = %d, want 2", entries)
	}
	head, err := os.ReadFile(filepath.Join(base, ".userdata", "shared", "Lodor", "continue-head.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Multi-entry head (task #135): every Continue entry lands, newest first, so the
	// dispatcher can fall through past a drifted path.
	if got, want := string(head), rel(2)+"\tGame 2\n"+rel(1)+"\tGame 1\n"; got != want {
		t.Fatalf("head = %q, want %q", got, want)
	}
	mapData, err := os.ReadFile(filepath.Join(base, "Roms", "map.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mapData), continueRootDirName+"\t0) Continue: Game 2") {
		t.Fatalf("map.txt lacks the dynamic label: %q", string(mapData))
	}
}
