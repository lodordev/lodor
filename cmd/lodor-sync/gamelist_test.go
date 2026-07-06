// Gamelist emitter tests (task #186 — Lodor for Knulli, Phase A).
//
// Deliberately UNTAGGED: the merge logic is host-agnostic and must behave
// identically on every build (only the call sites gate on gamelistEnabled), so
// this suite runs under default, -tags onion, -tags muos AND -tags knulli. The
// env relocation (BASE_PATH → <base>/Roms etc.) is honored by every variant.
package main

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lodor/config"
	"lodor/platform"
)

// outGame / outDoc parse the emitter's output for assertions.
type outGame struct {
	ID     string `xml:"id,attr"`
	Source string `xml:"source,attr"`
	Path   string `xml:"path"`
	Name   string `xml:"name"`
	Image  string `xml:"image"`
	Desc   string `xml:"desc"`
	Rating string `xml:"rating"`
}

type outDoc struct {
	XMLName xml.Name  `xml:"gameList"`
	Games   []outGame `xml:"game"`
	Folders []struct {
		Path string `xml:"path"`
		Name string `xml:"name"`
	} `xml:"folder"`
}

func parseGamelist(t *testing.T, data []byte) outDoc {
	t.Helper()
	var doc outDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("emitted gamelist does not parse as XML: %v\n%s", err, data)
	}
	return doc
}

func findGame(doc outDoc, path string) (outGame, bool) {
	for _, g := range doc.Games {
		if g.Path == path {
			return g, true
		}
	}
	return outGame{}, false
}

// gamelistEnv relocates the whole tree to a temp dir and returns the gba rom dir.
func gamelistEnv(t *testing.T) (base, dir string) {
	t.Helper()
	base = t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("ROMS_DIR", "") // onion variant honors ROMS_DIR first; force BASE_PATH path
	os.Unsetenv("ROMS_DIR")
	t.Setenv("LODOR_PAK_DIR", filepath.Join(base, "pak"))
	dir = filepath.Join(base, "Roms", "gba")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return base, dir
}

// TestGamelistEndToEnd is the whole Phase-A contract in one pass: foreign
// entries (attributes + <desc>/<rating> children) and non-game elements are
// preserved; owned entries are added with marker-stripped names and a cover
// <image> only when the cover file exists; a stale owned entry under the OLD
// marker (✘) is re-pointed at the new on-disk name (✓) IN PLACE with its
// scraper children intact; the output parses as XML; the write is atomic (no
// temp residue) and idempotent (an unchanged merge writes nothing).
func TestGamelistEndToEnd(t *testing.T) {
	_, dir := gamelistEnv(t)
	cfg := &config.Config{MirrorMode: config.MirrorModeOwn}

	// On-card state: one cloud stub, one downloaded game (with cover), plus the
	// user's own Mario.gba the manifest does NOT own.
	stub := filepath.Join(dir, platform.MarkerCloud+"Game & Co (USA).gba")
	if err := os.WriteFile(stub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dl := filepath.Join(dir, platform.MarkerOnDevice+"Zelda (USA).gba")
	if err := os.WriteFile(dl, []byte("ROM BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(dir, ".media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, platform.MarkerOnDevice+"Zelda (USA).png"), []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Mario.gba"), []byte("USER ROM"), 0o644); err != nil {
		t.Fatal(err)
	}

	man := platform.LoadManifest()
	man.Record(stub, platform.ManifestStub, 11)
	man.Record(dl, platform.ManifestDownload, 22)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	// Existing gamelist: the user's scraped Mario entry (attrs + desc + rating),
	// a <folder> element, and a STALE owned entry still pointing at the pre-flip
	// "✘ Zelda" name that a scraper decorated with a <desc>.
	existing := `<?xml version="1.0"?>
<gameList>
  <game id="42" source="ScreenScraper">
    <path>./Mario.gba</path>
    <name>Mario</name>
    <desc>The user's own plumber.</desc>
    <rating>0.9</rating>
  </game>
  <folder>
    <path>./hacks</path>
    <name>Hacks</name>
  </folder>
  <game>
    <path>./` + platform.MarkerCloud + `Zelda (USA).gba</path>
    <name>Zelda (USA)</name>
    <desc>Scraped while it was still a stub.</desc>
  </game>
</gameList>
`
	glPath := filepath.Join(dir, "gamelist.xml")
	if err := os.WriteFile(glPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	files, entries, err := emitGamelists(cfg, nil)
	if err != nil {
		t.Fatalf("emitGamelists: %v", err)
	}
	if files != 1 || entries != 2 {
		t.Errorf("emitGamelists = (files=%d, entries=%d) want (1, 2)", files, entries)
	}

	data, rerr := os.ReadFile(glPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	doc := parseGamelist(t, data) // spec: output parses as XML

	// Foreign entry: byte-for-byte data survival (attrs + children), still first.
	mario, ok := findGame(doc, "./Mario.gba")
	if !ok {
		t.Fatalf("foreign Mario entry dropped:\n%s", data)
	}
	if mario.ID != "42" || mario.Source != "ScreenScraper" ||
		mario.Desc != "The user's own plumber." || mario.Rating != "0.9" || mario.Name != "Mario" {
		t.Errorf("foreign entry mutated: %+v", mario)
	}
	if len(doc.Folders) != 1 || doc.Folders[0].Path != "./hacks" || doc.Folders[0].Name != "Hacks" {
		t.Errorf("non-game <folder> element not preserved: %+v", doc.Folders)
	}

	// Marker flip: the stale ✘ entry is re-pointed at the ✓ name in place, its
	// scraper <desc> intact, <image> now set (the cover exists), and NO ✘
	// duplicate remains.
	zelda, ok := findGame(doc, "./"+platform.MarkerOnDevice+"Zelda (USA).gba")
	if !ok {
		t.Fatalf("owned Zelda entry not re-pointed at the ✓ name:\n%s", data)
	}
	if zelda.Name != "Zelda (USA)" {
		t.Errorf("owned name = %q want marker-stripped %q", zelda.Name, "Zelda (USA)")
	}
	if zelda.Desc != "Scraped while it was still a stub." {
		t.Errorf("scraper <desc> on OUR entry lost across the update: %+v", zelda)
	}
	if want := "./.media/" + platform.MarkerOnDevice + "Zelda (USA).png"; zelda.Image != want {
		t.Errorf("owned image = %q want %q", zelda.Image, want)
	}
	if _, stale := findGame(doc, "./"+platform.MarkerCloud+"Zelda (USA).gba"); stale {
		t.Error("stale ✘ entry left beside the updated ✓ entry (duplicate row)")
	}

	// New stub entry: added, marker-stripped display name, NO image (no cover),
	// special characters escaped-then-round-tripped.
	game, ok := findGame(doc, "./"+platform.MarkerCloud+"Game & Co (USA).gba")
	if !ok {
		t.Fatalf("owned stub entry not added:\n%s", data)
	}
	if game.Name != "Game & Co (USA)" {
		t.Errorf("stub name = %q want %q", game.Name, "Game & Co (USA)")
	}
	if game.Image != "" {
		t.Errorf("stub image = %q want none (no cover on card)", game.Image)
	}

	// Atomic: no temp residue beside the written file.
	if _, err := os.Stat(glPath + ".tmp"); !os.IsNotExist(err) {
		t.Error("gamelist.xml.tmp left behind — write not atomic")
	}

	// Idempotent: a second pass changes nothing and writes no file.
	files2, _, err2 := emitGamelists(cfg, nil)
	if err2 != nil {
		t.Fatalf("second emitGamelists: %v", err2)
	}
	if files2 != 0 {
		t.Errorf("second pass wrote %d file(s), want 0 (unchanged merge must not write)", files2)
	}
	data2, _ := os.ReadFile(glPath)
	if string(data2) != string(data) {
		t.Error("second pass changed bytes — merge not stable")
	}
}

// TestGamelistUnparseableExistingUntouched: a corrupt gamelist.xml is NEVER
// clobbered — the user's bytes stay identical and no temp file appears.
func TestGamelistUnparseableExistingUntouched(t *testing.T) {
	_, dir := gamelistEnv(t)
	cfg := &config.Config{MirrorMode: config.MirrorModeOwn}

	stub := filepath.Join(dir, platform.MarkerCloud+"Game.gba")
	if err := os.WriteFile(stub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	man := platform.LoadManifest()
	man.Record(stub, platform.ManifestStub, 1)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	garbage := "<gameList><game><path>./x.gba</path>" // truncated: unparseable
	glPath := filepath.Join(dir, "gamelist.xml")
	if err := os.WriteFile(glPath, []byte(garbage), 0o644); err != nil {
		t.Fatal(err)
	}

	files, _, err := emitGamelists(cfg, nil)
	if err != nil {
		t.Fatalf("emitGamelists: %v", err)
	}
	if files != 0 {
		t.Errorf("wrote %d file(s) over an unparseable gamelist, want 0", files)
	}
	data, _ := os.ReadFile(glPath)
	if string(data) != garbage {
		t.Errorf("corrupt user gamelist was modified:\n%s", data)
	}
	if _, err := os.Stat(glPath + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp residue left beside the refused file")
	}
}

// TestGamelistFreshFileAndDirScoping: with no existing gamelist a fresh one is
// created; the onlyDirs scope restricts the walk to the named directory.
func TestGamelistFreshFileAndDirScoping(t *testing.T) {
	base, dir := gamelistEnv(t)
	cfg := &config.Config{MirrorMode: config.MirrorModeOwn}

	otherDir := filepath.Join(base, "Roms", "snes")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gba := filepath.Join(dir, platform.MarkerCloud+"Game.gba")
	snes := filepath.Join(otherDir, platform.MarkerCloud+"Other.sfc")
	for _, p := range []string{gba, snes} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	man := platform.LoadManifest()
	man.Record(gba, platform.ManifestStub, 1)
	man.Record(snes, platform.ManifestStub, 2)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	// Scoped to the gba dir only.
	maybe := map[string]bool{dir: true}
	files, entries, err := emitGamelists(cfg, maybe)
	if err != nil {
		t.Fatalf("emitGamelists(scoped): %v", err)
	}
	if files != 1 || entries != 1 {
		t.Errorf("scoped emit = (files=%d, entries=%d) want (1, 1)", files, entries)
	}
	if _, err := os.Stat(filepath.Join(otherDir, "gamelist.xml")); !os.IsNotExist(err) {
		t.Error("out-of-scope snes gamelist was written")
	}
	data, rerr := os.ReadFile(filepath.Join(dir, "gamelist.xml"))
	if rerr != nil {
		t.Fatalf("fresh gamelist not created: %v", rerr)
	}
	doc := parseGamelist(t, data)
	if g, ok := findGame(doc, "./"+platform.MarkerCloud+"Game.gba"); !ok || g.Name != "Game" {
		t.Errorf("fresh entry wrong: %+v (ok=%v)", g, ok)
	}

	// The manifest records the written gamelist (kind "gamelist").
	man2 := platform.LoadManifest()
	if !man2.OwnsKind(filepath.Join(dir, "gamelist.xml"), platform.ManifestGamelist) {
		t.Error("written gamelist.xml not recorded in the mirror manifest")
	}
}

// TestMergeGamelistPure: unit coverage of the merge itself — a marker-less user
// file that shares the CANONICAL name with an owned MARKED rom is foreign and
// must be preserved (only marker-carrying paths may be re-keyed).
func TestMergeGamelistPure(t *testing.T) {
	existing := `<gameList>
  <game>
    <path>./Zelda (USA).gba</path>
    <name>Zelda the user's copy</name>
    <desc>Not Lodor's file.</desc>
  </game>
</gameList>`
	ours := []glEntry{{
		fsname: platform.MarkerCloud + "Zelda (USA).gba",
		name:   "Zelda (USA)",
	}}
	out, err := mergeGamelist([]byte(existing), ours)
	if err != nil {
		t.Fatal(err)
	}
	doc := parseGamelist(t, out)
	user, ok := findGame(doc, "./Zelda (USA).gba")
	if !ok || user.Name != "Zelda the user's copy" || user.Desc != "Not Lodor's file." {
		t.Errorf("marker-less user entry with a shared canonical name was captured: %+v (ok=%v)", user, ok)
	}
	if _, ok := findGame(doc, "./"+platform.MarkerCloud+"Zelda (USA).gba"); !ok {
		t.Error("owned marked entry not appended beside the user's")
	}
	if len(doc.Games) != 2 {
		t.Errorf("games = %d want 2 (user's + ours)", len(doc.Games))
	}
	if !strings.Contains(string(out), "Not Lodor's file.") {
		t.Error("foreign child text lost")
	}
}
