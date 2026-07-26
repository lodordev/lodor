package main

// launchcard_test.go — launch card v2 (task launch-card-v2). The content
// model is pure (fixtures in, model out); the loop tests drive the REAL
// card/sub-view code with a fake engine and a scripted button feed, the same
// scaffolding the rest of the wizard suite uses.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lodor/ui"
)

// Fixtures: the exact row shapes --list-saves / --list-states emit.
const (
	fxSaves = "12\t2026-07-06 21:04\tflip\t4KB\n" +
		"34\t2026-07-05 09:12\tbrick\t4KB\tCURRENT\n" +
		"LOCAL=older\n"
	fxStates = `LISTSTATE id=1 slot=0 compat=1 known=1 age=100 size=1 origin=lodor/knulli/gpsp@0.91/arm64 why="-" name="a"
LISTSTATE id=2 slot=auto compat=1 known=0 age=7200 size=1 origin=lodor/muos/gpsp@0.91/arm64 why="-" name="b"
LISTSTATE id=3 slot=1 compat=0 known=0 age=60 size=1 origin=lodor/knulli/gpsp@0.91/armhf why="different architecture (armhf vs arm64)" name="c"
LISTSTATE id=4 slot=2 compat=0 known=0 age=5 size=1 origin=foreign:builtin why="non-lodor state" name="d"
RESULT liststates=4 compatstates=2 reason=ok
`
)

// TestBuildCardModel: the fixture pair yields the right rows, bright/dim
// split (straight from the engine's compat= flag — the ONE verdict), counts
// line, saves list, and news signal.
func TestBuildCardModel(t *testing.T) {
	m := buildCardModel("/roms/PS/Legend of Dragoon.m3u", fxSaves, nil, fxStates, nil,
		true, 1, 3, "Played 4h 12m across 7 sessions")
	if m.title != "Legend of Dragoon" {
		t.Fatalf("title = %q", m.title)
	}
	if len(m.states) != 4 || len(m.saves) != 2 {
		t.Fatalf("rows: %d states %d saves", len(m.states), len(m.saves))
	}
	if m.loadableN != 2 || m.otherN != 2 {
		t.Fatalf("compat split: %d/%d", m.loadableN, m.otherN)
	}
	if !m.saveNewer {
		t.Fatal("LOCAL=older must set saveNewer")
	}
	if m.states[0].compat != true || m.states[2].compat != false {
		t.Fatal("compat must mirror the engine's compat= flag verbatim")
	}
	want := []string{"Played 4h 12m across 7 sessions", "Discs 1/3 on card", "4 states - 2 saves"}
	if len(m.hero) != 3 || m.hero[0] != want[0] || m.hero[1] != want[1] || m.hero[2] != want[2] {
		t.Fatalf("hero = %v", m.hero)
	}
	if got := m.compatSummary(); got != "* 2 loadable here   o 2 other device" {
		t.Fatalf("summary = %q", got)
	}
	if !m.saves[1].current || !strings.Contains(m.saves[1].label, "(on this device)") {
		t.Fatalf("CURRENT save row: %+v", m.saves[1])
	}
}

// TestBuildCardModelProbeFailure: a failed probe flags the section instead of
// rendering "0 states" as fact (feedback_no_fake_ui_state: an unreachable
// server is not an empty server).
func TestBuildCardModelProbeFailure(t *testing.T) {
	m := buildCardModel("/roms/GB/Tetris.gb", "", errors.New("rc=3"), "", errors.New("rc=3"),
		false, 0, 0, "")
	if !m.savesErr || !m.statesErr {
		t.Fatal("probe errors must be carried")
	}
	if len(m.states) != 0 || len(m.saves) != 0 || m.saveNewer {
		t.Fatal("failed probes must contribute no rows and no news")
	}
	if m.hero[0] != "In your cloud library" {
		t.Fatalf("stub status = %q", m.hero[0])
	}
	if m.compatSummary() != "" {
		t.Fatal("no states -> no summary line")
	}
}

// TestCardTitleStripsDecorations: marker, RomM coexist tag, and extension all
// drop from the hero title.
func TestCardTitleStripsDecorations(t *testing.T) {
	for in, want := range map[string]string{
		"/r/GB/✘ Tetris (USA).gb":     "Tetris (USA)",
		"/r/GB/✓ Tetris (RomM).gb":    "Tetris",
		"/r/PS/Legend of Dragoon.m3u": "Legend of Dragoon",
	} {
		if got := cardTitle(in); got != want {
			t.Fatalf("cardTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCardStatusLine: disc census wins; else downloaded/stub truth.
func TestCardStatusLine(t *testing.T) {
	if got := cardStatusLine(true, 1, 3); got != "Discs 1/3 on card" {
		t.Fatalf("multi-disc = %q", got)
	}
	if got := cardStatusLine(true, 0, 0); got != "On this card" {
		t.Fatalf("downloaded = %q", got)
	}
	if got := cardStatusLine(false, 0, 0); got != "In your cloud library" {
		t.Fatalf("stub = %q", got)
	}
}

// TestNeedsLabel: a dim row explains itself from its origin tuple; foreign
// rows fall back to the engine's why= text.
func TestNeedsLabel(t *testing.T) {
	kv := parseStateLine(`LISTSTATE id=3 compat=0 origin=lodor/knulli/gpsp@0.91/armhf why="different architecture"`)
	if got := needsLabel(kv); got != "needs gpsp on armhf" {
		t.Fatalf("tuple needs = %q", got)
	}
	kv = parseStateLine(`LISTSTATE id=4 compat=0 origin=foreign:builtin why="non-lodor state"`)
	if got := needsLabel(kv); got != "needs: non-lodor state" {
		t.Fatalf("foreign needs = %q", got)
	}
	kv = parseStateLine(`LISTSTATE id=5 compat=0 origin=garbage why="-"`)
	if got := needsLabel(kv); got != "can't load here" {
		t.Fatalf("fallback = %q", got)
	}
}

// TestNextLoadableSkipsDimmed: the States cursor lands only on compat rows —
// dimmed rows are visible but unselectable; a list with no loadable rows
// yields no cursor at all.
func TestNextLoadableSkipsDimmed(t *testing.T) {
	rows := parseCardStates(fxStates) // compat: [1]=t [2]=t [3]=f [4]=f (ids 1..4)
	if i := nextLoadable(rows, -1, +1); i != 0 {
		t.Fatalf("first loadable = %d", i)
	}
	if i := nextLoadable(rows, 0, +1); i != 1 {
		t.Fatalf("second loadable = %d", i)
	}
	if i := nextLoadable(rows, 1, +1); i != 1 {
		t.Fatal("down past the last loadable row must stay (rows 2,3 are dimmed)")
	}
	if i := nextLoadable(rows, 1, -1); i != 0 {
		t.Fatalf("back up = %d", i)
	}
	allDim := parseCardStates(strings.ReplaceAll(fxStates, "compat=1", "compat=0"))
	if i := nextLoadable(allDim, -1, +1); i != -1 {
		t.Fatalf("no loadable rows must mean no cursor, got %d", i)
	}
}

// TestMoveActionClampsAndDefaultsToPlay: Play is the default highlight; the
// cursor clamps at both row ends.
func TestMoveActionClampsAndDefaultsToPlay(t *testing.T) {
	m := &cardModel{}
	if m.sel != cardActPlay {
		t.Fatal("default selection must be Play")
	}
	m.moveAction(-1)
	if m.sel != cardActPlay {
		t.Fatal("left of Play must clamp")
	}
	for i := 0; i < 10; i++ {
		m.moveAction(+1)
	}
	if m.sel != cardActManage {
		t.Fatalf("right end must clamp at Manage, got %d", m.sel)
	}
}

// TestParseSaveRowsSharedShape: the extracted parser keeps gmServerSaves'
// exact labels and drops the LOCAL= trailer.
func TestParseSaveRowsSharedShape(t *testing.T) {
	saves := parseSaveRows(fxSaves)
	if len(saves) != 2 {
		t.Fatalf("rows = %d", len(saves))
	}
	if saves[0].label != "2026-07-06 21:04  -  flip  -  4KB" || saves[0].id != "12" {
		t.Fatalf("row 0 = %+v", saves[0])
	}
	if !saves[1].current {
		t.Fatal("CURRENT must be flagged")
	}
}

// TestCardDiscCounts: present = playlist lines with bytes, total = the
// manifest's canonical set; unmanifested falls back to present; non-m3u and
// stub playlists census as (0,0).
func TestCardDiscCounts(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("LODOR_PAK_DIR", base)
	dir := filepath.Join(base, "Roms", "PS")
	if err := os.MkdirAll(filepath.Join(dir, ".Dragoon"), 0o755); err != nil {
		t.Fatal(err)
	}
	m3u := filepath.Join(dir, "Dragoon.m3u")
	if err := os.WriteFile(m3u, []byte(".Dragoon/disc1.chd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".Dragoon", "disc1.chd"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"version":1,"entries":{"/Roms/PS/Dragoon.m3u":{"kind":"rom","discs":[".Dragoon/disc1.chd",".Dragoon/disc2.chd",".Dragoon/disc3.chd"]}}}`
	if err := os.WriteFile(filepath.Join(base, "mirror-manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, tot := cardDiscCounts(m3u); p != 1 || tot != 3 {
		t.Fatalf("census = %d/%d, want 1/3", p, tot)
	}
	// Unmanifested: total falls back to present.
	if err := os.Remove(filepath.Join(base, "mirror-manifest.json")); err != nil {
		t.Fatal(err)
	}
	if p, tot := cardDiscCounts(m3u); p != 1 || tot != 1 {
		t.Fatalf("unmanifested = %d/%d, want 1/1", p, tot)
	}
	// Non-m3u and 0-byte stub playlists: no census.
	if p, tot := cardDiscCounts(filepath.Join(dir, "One.chd")); p != 0 || tot != 0 {
		t.Fatalf("non-m3u = %d/%d", p, tot)
	}
	stub := filepath.Join(dir, "Stub.m3u")
	if err := os.WriteFile(stub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if p, tot := cardDiscCounts(stub); p != 0 || tot != 0 {
		t.Fatalf("stub = %d/%d", p, tot)
	}
}

// TestCardCoverPath: the .media cover resolves through the marker variants
// (the cover keeps the marker it was fetched under; the rom's marker flips on
// download/evict), and a missing cover returns "" — the placeholder path.
func TestCardCoverPath(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, ".media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	rom := filepath.Join(dir, "✓ Tetris (USA).gb")
	if got := cardCoverPath(rom); got != "" {
		t.Fatalf("missing cover = %q", got)
	}
	// Cover saved under the CLOUD marker while the rom is now on-device.
	cov := filepath.Join(media, "✘ Tetris (USA).png")
	if err := os.WriteFile(cov, []byte("png-ish"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cardCoverPath(rom); got != cov {
		t.Fatalf("marker-variant cover = %q, want %q", got, cov)
	}
	// Exact-name cover wins when present.
	exact := filepath.Join(media, "✓ Tetris (USA).png")
	if err := os.WriteFile(exact, []byte("png-ish"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cardCoverPath(rom); got != exact {
		t.Fatalf("exact cover = %q, want %q", got, exact)
	}
}

// ---- render + loop + trigger (launch card v2) ------------------------------

// TestCardHasNews: the smart trigger fires on a newer save OR an unseen
// compatible state; failed probes and no-news fixtures stay silent.
func TestCardHasNews(t *testing.T) {
	if !cardHasNews(fxSaves, nil, "", nil) {
		t.Fatal("LOCAL=older must be news")
	}
	if !cardHasNews("LOCAL=current\n", nil, fxStates, nil) {
		t.Fatal("compat=1 known=0 state must be news")
	}
	if cardHasNews("LOCAL=current\n", nil,
		`LISTSTATE id=1 slot=0 compat=1 known=1 age=9 size=1 origin=lodor/muos/gpsp@1/arm64 why="-" name="a"`, nil) {
		t.Fatal("a known state is not news")
	}
	if cardHasNews(fxSaves, errors.New("rc=3"), fxStates, errors.New("rc=3")) {
		t.Fatal("failed probes must never be news")
	}
}

// driveCard runs the REAL card loop against a scripted button feed and a
// no-op renderer; buttons past the script feed B so a loop that ignored B
// times out (honest failure) instead of hanging the suite.
func driveCard(m *cardModel, seq []ui.Button) bool {
	w := &wizard{t: ui.DefaultTheme()}
	return driveView(func(draw func(*ui.Canvas), btn func() ui.Button) {
		w.runCardLoop(m, nil, draw, btn)
	}, seq)
}

// driveView runs any card screen against a scripted feed + no-op renderer;
// exhausted scripts feed B so an unresponsive screen fails by timeout.
func driveView(f func(draw func(*ui.Canvas), btn func() ui.Button), seq []ui.Button) bool {
	ch := make(chan ui.Button, len(seq))
	for _, b := range seq {
		ch <- b
	}
	done := make(chan struct{})
	go func() {
		btn := func() ui.Button {
			select {
			case b := <-ch:
				return b
			default:
				return ui.BtnBack
			}
		}
		f(func(*ui.Canvas) {}, btn)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// TestCardLoopPlayDefault: A on the untouched card plays immediately (Play is
// the default highlight); B and Start play too. The card never traps.
func TestCardLoopPlayDefault(t *testing.T) {
	for _, seq := range [][]ui.Button{
		{ui.BtnConfirm},
		{ui.BtnBack},
		{ui.BtnStart},
	} {
		m := buildCardModel("/r/GB/Tetris.gb", fxSaves, nil, fxStates, nil, true, 0, 0, "")
		if !driveCard(m, seq) {
			t.Fatalf("card must exit to launch on %v", seq)
		}
		if m.sel != cardActPlay {
			t.Fatalf("selection moved unexpectedly: %d", m.sel)
		}
	}
}

// TestCardLoopActionRowMovement: Right/Left and L1/R1 move the action cursor
// (clamped) and the loop still exits via B.
func TestCardLoopActionRowMovement(t *testing.T) {
	m := buildCardModel("/r/GB/Tetris.gb", fxSaves, nil, fxStates, nil, true, 0, 0, "")
	if !driveCard(m, []ui.Button{ui.BtnRight, ui.BtnR1, ui.BtnLeft, ui.BtnBack}) {
		t.Fatal("loop must exit")
	}
	if m.sel != cardActStates {
		t.Fatalf("sel = %d, want States (right,right,left)", m.sel)
	}
	m2 := buildCardModel("/r/GB/Tetris.gb", fxSaves, nil, fxStates, nil, true, 0, 0, "")
	if !driveCard(m2, []ui.Button{ui.BtnL1, ui.BtnBack}) {
		t.Fatal("loop must exit")
	}
	if m2.sel != cardActPlay {
		t.Fatalf("L1 at Play must clamp, got %d", m2.sel)
	}
}

// TestRenderCardSmoke: the full-card render draws real content (not bare
// background), reacts to cover presence and selection, and the canvas is
// PNG-dumpable — the LODOR_FB_DUMP surface.
func TestRenderCardSmoke(t *testing.T) {
	th := ui.DefaultTheme()
	m := buildCardModel("/r/PS/Legend of Dragoon.m3u", fxSaves, nil, fxStates, nil,
		true, 1, 3, "Played 4h 12m across 7 sessions")
	plain := renderCard(th, m, nil)
	nonBg := 0
	for _, p := range plain.Pix {
		if p != th.Bg && p != th.Panel {
			nonBg++
		}
	}
	if nonBg < 500 {
		t.Fatalf("card render suspiciously empty: %d non-bg pixels", nonBg)
	}
	// Cover changes the hero box.
	cov := ui.NewCanvas(60, 80)
	cov.Clear(0x00ff00)
	withCov := renderCard(th, m, cov)
	if equalPix(plain, withCov) {
		t.Fatal("cover blit must change the render")
	}
	// Selection changes the action row.
	m.moveAction(+1)
	moved := renderCard(th, m, nil)
	if equalPix(plain, moved) {
		t.Fatal("action-row selection must change the render")
	}
	if err := plain.SavePNG(filepath.Join(t.TempDir(), "card.png")); err != nil {
		t.Fatalf("PNG dump: %v", err)
	}
}

func equalPix(a, b *ui.Canvas) bool {
	if a.W != b.W || a.H != b.H {
		return false
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			return false
		}
	}
	return true
}

// TestFitScale: long titles step down toward 1 instead of overflowing.
func TestFitScale(t *testing.T) {
	if fitScale("SHORT", 720, 3) != 3 {
		t.Fatal("short title keeps preferred scale")
	}
	long := strings.Repeat("VERY LONG TITLE ", 8)
	if got := fitScale(long, 300, 3); got != 1 {
		t.Fatalf("overlong = %d, want 1", got)
	}
}

// TestFitText: #73 per-line ellipsis. A fitting line is untouched; an overlong
// line is clipped with a trailing "..." and never exceeds maxW.
func TestFitText(t *testing.T) {
	if got := fitText("On this card", 720, 2); got != "On this card" {
		t.Fatalf("fitting line changed: %q", got)
	}
	long := "needs pcsx_rearmed on arm64 - Auto-resume - 3 days ago - from a Smart Pro"
	got := fitText(long, 300, 2)
	if got == long {
		t.Fatal("overlong line was not clipped")
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("clipped line missing ellipsis: %q", got)
	}
	if ui.TextWidth(got, 2) > 300 {
		t.Fatalf("clipped line still overflows: width %d > 300", ui.TextWidth(got, 2))
	}
	// Degenerate: maxW too small for even "..." -> longest bare prefix, still in bounds.
	tiny := fitText("ABCDEF", 5, 2)
	if ui.TextWidth(tiny, 2) > 5 {
		t.Fatalf("tiny clip overflowed: %q width %d", tiny, ui.TextWidth(tiny, 2))
	}
}

// ---- States sub-view -------------------------------------------------------

// cardEngineStub installs a fake lodor-sync that records argv and answers
// --pull-state / --evict / --restore-save / --sync-save / --download with the
// given RESULT line. Returns the wizard and the argv capture file.
func cardEngineStub(t *testing.T, result string) (*wizard, string) {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	bin := writeFakeEngine(t, `echo "$@" >> `+argsFile+`
echo "`+result+`"; exit 0`)
	return &wizard{t: ui.DefaultTheme(), dataDir: t.TempDir(), bin: bin}, argsFile
}

// TestCardStatesViewPlacesLoadable: Down skips nothing among loadable rows;
// A places the SECOND loadable state via --pull-state with its id, then the
// view returns to the card.
func TestCardStatesViewPlacesLoadable(t *testing.T) {
	w, argsFile := cardEngineStub(t, "RESULT placedstate=1")
	m := buildCardModel("/r/GG/Woody.gg", "", nil, fxStates, nil, true, 0, 0, "")
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardStatesView(m, d, b) },
		[]ui.Button{ui.BtnDown, ui.BtnConfirm}) {
		t.Fatal("states view must return after placement")
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--pull-state /r/GG/Woody.gg --state-id 2") {
		t.Fatalf("engine argv = %q, want pull-state of id 2", args)
	}
}

// TestCardStatesViewCursorSkipsDimmed: with the cursor on the last loadable
// row, Down (into dimmed territory) stays put; A still places THAT row —
// dimmed rows are unreachable, hence unselectable.
func TestCardStatesViewCursorSkipsDimmed(t *testing.T) {
	w, argsFile := cardEngineStub(t, "RESULT placedstate=1")
	m := buildCardModel("/r/GG/Woody.gg", "", nil, fxStates, nil, true, 0, 0, "")
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardStatesView(m, d, b) },
		[]ui.Button{ui.BtnDown, ui.BtnDown, ui.BtnDown, ui.BtnConfirm}) {
		t.Fatal("states view must return")
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--state-id 2") {
		t.Fatalf("cursor must clamp at the last LOADABLE row: %q", args)
	}
}

// TestCardStatesViewAllDimmedNoPlacement: a list with zero loadable rows has
// no cursor; A is a no-op and B leaves without any engine call.
func TestCardStatesViewAllDimmedNoPlacement(t *testing.T) {
	w, argsFile := cardEngineStub(t, "RESULT placedstate=1")
	allDim := strings.ReplaceAll(fxStates, "compat=1", "compat=0")
	m := buildCardModel("/r/GG/Woody.gg", "", nil, allDim, nil, true, 0, 0, "")
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardStatesView(m, d, b) },
		[]ui.Button{ui.BtnConfirm, ui.BtnDown, ui.BtnConfirm, ui.BtnBack}) {
		t.Fatal("states view must return via B")
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("no engine call may happen when nothing is loadable")
	}
}

// TestCardStatesViewOfflineHonest: a failed probe renders the honest
// unreachable message (dismissable), never an empty "no states" claim.
func TestCardStatesViewOfflineHonest(t *testing.T) {
	w, argsFile := cardEngineStub(t, "RESULT placedstate=1")
	m := buildCardModel("/r/GG/Woody.gg", "", nil, "", errors.New("rc=3"), true, 0, 0, "")
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardStatesView(m, d, b) },
		[]ui.Button{ui.BtnConfirm}) {
		t.Fatal("offline message must dismiss")
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("offline view must not call the engine")
	}
}

// ---- Saves + Manage sub-views ----------------------------------------------

// TestCardSavesViewRestores: Down then A restores the second server save via
// the existing --restore-save mode with that row's id.
func TestCardSavesViewRestores(t *testing.T) {
	w, argsFile := cardEngineStub(t, "RESULT restored=1 staged=1")
	m := buildCardModel("/r/GG/Woody.gg", fxSaves, nil, "", nil, true, 0, 0, "")
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardSavesView(m, d, b) },
		[]ui.Button{ui.BtnDown, ui.BtnConfirm}) {
		t.Fatal("saves view must return after restore")
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--restore-save /r/GG/Woody.gg 34") {
		t.Fatalf("engine argv = %q, want restore of id 34", args)
	}
}

// TestCardSavesViewHonestEmptyAndOffline: no saves and failed probe each show
// a dismissable honest message with zero engine calls.
func TestCardSavesViewHonestEmptyAndOffline(t *testing.T) {
	w, argsFile := cardEngineStub(t, "RESULT restored=1")
	empty := buildCardModel("/r/GG/Woody.gg", "LOCAL=none\n", nil, "", nil, true, 0, 0, "")
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardSavesView(empty, d, b) },
		[]ui.Button{ui.BtnConfirm}) {
		t.Fatal("empty-list message must dismiss")
	}
	off := buildCardModel("/r/GG/Woody.gg", "", errors.New("rc=3"), "", nil, true, 0, 0, "")
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardSavesView(off, d, b) },
		[]ui.Button{ui.BtnConfirm}) {
		t.Fatal("offline message must dismiss")
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("neither case may call the engine")
	}
}

// TestCardManageDeleteStubsLaunchTarget: THE edge case — deleting the game
// the card is gating. The evict runs (confirmed), the model flips to
// not-downloaded + evicted, and the hero status line goes honest ("In your
// cloud library"); the card itself still exits 0 upstream (launch proceeds
// onto the stub, the shape every lane already handles).
func TestCardManageDeleteStubsLaunchTarget(t *testing.T) {
	w, argsFile := cardEngineStub(t, "RESULT evicted=1")
	rom := filepath.Join(t.TempDir(), "Woody.gg")
	if err := os.WriteFile(rom, []byte("real bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := buildCardModel(rom, fxSaves, nil, fxStates, nil, true, 0, 0, "")
	// A = pick "Delete from card", A = confirm yes, A = dismiss "Deleted".
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardManageView(m, d, b) },
		[]ui.Button{ui.BtnConfirm, ui.BtnConfirm, ui.BtnConfirm}) {
		t.Fatal("manage view must return after delete")
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--evict "+rom) {
		t.Fatalf("engine argv = %q, want evict", args)
	}
	if !m.evicted || m.downloaded {
		t.Fatalf("model after delete: evicted=%v downloaded=%v", m.evicted, m.downloaded)
	}
	if m.hero[len(m.hero)-2] != "In your cloud library" {
		t.Fatalf("status line after delete = %q", m.hero[len(m.hero)-2])
	}
}

// TestCardManageDeleteDeclined: backing out of the confirm leaves the model
// and the card untouched (no engine call).
func TestCardManageDeleteDeclined(t *testing.T) {
	w, argsFile := cardEngineStub(t, "RESULT evicted=1")
	rom := filepath.Join(t.TempDir(), "Woody.gg")
	if err := os.WriteFile(rom, []byte("real bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := buildCardModel(rom, "", nil, "", nil, true, 0, 0, "")
	// A = pick Delete, Down+A = pick "Keep it".
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardManageView(m, d, b) },
		[]ui.Button{ui.BtnConfirm, ui.BtnDown, ui.BtnConfirm}) {
		t.Fatal("manage view must return")
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("declined confirm must not evict")
	}
	if m.evicted || !m.downloaded {
		t.Fatalf("model must be untouched: evicted=%v downloaded=%v", m.evicted, m.downloaded)
	}
}

// TestCardManageDetailsOffline: Details is pure-local and dismisses; no
// engine call. (A stub path renders the honest cloud state.)
func TestCardManageDetailsOffline(t *testing.T) {
	w, argsFile := cardEngineStub(t, "RESULT ok=1")
	m := buildCardModel("/nonexistent/Woody.gg", "", nil, "", nil, false, 0, 0, "")
	if !driveView(func(d func(*ui.Canvas), b func() ui.Button) { w.cardManageView(m, d, b) },
		[]ui.Button{ui.BtnDown, ui.BtnConfirm, ui.BtnConfirm}) {
		t.Fatal("details must dismiss and return")
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("details must not call the engine")
	}
}
