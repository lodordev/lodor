package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lodor/ui"
)

// driveOnboarding runs the onboarding state machine with a scripted button sequence and a
// no-op renderer, returning true if it exited (returned) within the timeout. This is the
// off-hardware "sim" proof for blocker #170: the user can always bail to muOS. Any button
// requested past the script is fed as BtnBack, so a screen that ignored B would keep asking
// and the test would time out (fail) rather than falsely pass.
func driveOnboarding(seq []ui.Button) bool {
	w := &wizard{t: ui.DefaultTheme(), device: "RG34XX", server: "https://"}
	w.dataDir = "/nonexistent-lodor-wizard-test" // no config.json -> onboarding path
	ch := make(chan ui.Button, len(seq))
	for _, b := range seq {
		ch <- b
	}
	done := make(chan struct{})
	go func() {
		draw := func(*ui.Canvas) {}
		btn := func() ui.Button {
			select {
			case b := <-ch:
				return b
			default:
				return ui.BtnBack
			}
		}
		w.runOnboarding(draw, btn)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// TestOnboardingBackFromWelcomeExits: B on the very first screen must exit to muOS.
func TestOnboardingBackFromWelcomeExits(t *testing.T) {
	if !driveOnboarding([]ui.Button{ui.BtnBack}) {
		t.Fatal("B on the welcome screen must exit onboarding (blocker #170)")
	}
}

// TestOnboardingBackChainExits: A into Wi-Fi, B back to welcome, B exits. Proves the
// server->wifi->welcome->exit back-chain is wired regardless of Wi-Fi state (these steps
// make no engine calls).
func TestOnboardingBackChainExits(t *testing.T) {
	if !driveOnboarding([]ui.Button{ui.BtnConfirm, ui.BtnBack, ui.BtnBack}) {
		t.Fatal("A,B,B (welcome->wifi->welcome->exit) must return, never trap the user")
	}
}

// ---- startup phase instrumentation (BUG 2a) -------------------------------------------

// TestLogPhaseWritesLine: logPhase appends an immediately-flushed line to wizard.log.
func TestLogPhaseWritesLine(t *testing.T) {
	dir := t.TempDir()
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir}
	w.logPhase("menu: state ok (pendingN=%d tsAvail=%v)", 3, true)
	b, err := os.ReadFile(filepath.Join(dir, "wizard.log"))
	if err != nil {
		t.Fatalf("wizard.log not written: %v", err)
	}
	if !strings.Contains(string(b), "wizard-phase: menu: state ok (pendingN=3 tsAvail=true)") {
		t.Fatalf("phase line not in wizard.log: %q", b)
	}
}

// TestPhaseSelftestEmitsSequence: the built-binary selftest (what the integ harness runs)
// emits EVERY canonical startup phase line, in order — proving the instrumentation fires so
// the next on-hardware hang localizes to the exact last phase reached.
func TestPhaseSelftestEmitsSequence(t *testing.T) {
	dir := t.TempDir()
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir}
	w.phaseSelftest()
	b, err := os.ReadFile(filepath.Join(dir, "wizard.log"))
	if err != nil {
		t.Fatalf("wizard.log not written: %v", err)
	}
	log := string(b)
	want := []string{
		"wizard: start",
		"fb open ",
		"input open ",
		"configured=",
		"menu: build state",
		"menu: state ok (",
		"menu: first draw",
		"menu: awaiting input",
	}
	last := 0
	for _, ph := range want {
		i := strings.Index(log[last:], ph)
		if i < 0 {
			t.Fatalf("startup phase %q missing or out of order in:\n%s", ph, log)
		}
		last += i + len(ph)
	}
}

// ---- menu-row assertions (the management-menu spine is pure + table-driven) ------------

// findRow returns the action for a label substring, or -1 if the row is absent.
func findRow(rows []menuRow, sub string) menuAct {
	for _, r := range rows {
		if contains(r.label, sub) {
			return r.act
		}
	}
	return menuAct(-1)
}
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestMenuRowsMapToModes: every row maps to the right engine action, and each row's label
// is present exactly where expected. This is the off-hardware proof that the menu spine
// dispatches Sync/Refresh/BIOS/etc to their confirmed modes.
func TestMenuRowsMapToModes(t *testing.T) {
	rows := buildMenuRows(menuState{userLabel: "Default"})
	cases := []struct {
		sub string
		act menuAct
	}{
		{"Sync now", actSyncNow},
		{"Refresh library (update)", actRefreshUpdate},
		{"Refresh library (full)", actRefreshFull},
		{"Game Manager", actGameManager},
		{"Search library", actSearch},
		{"Download BIOS", actDownloadBios},
		{"Recent activity", actRecent},
		{"Switch user", actSwitchUser},
		{"Add profile", actAddProfile},
		{"Box art", actBoxArt},
		{"RetroAchievements", actRetroAch},
		{"Setup / Re-pair", actReSetup},
		{"Remove Lodor", actRemoveLodor},
		{"Exit", actExit},
	}
	for _, c := range cases {
		if got := findRow(rows, c.sub); got != c.act {
			t.Errorf("row %q -> act %d, want %d", c.sub, got, c.act)
		}
	}
}

// TestMenuConditionalRows: pending/queue rows appear ONLY with a non-zero count; the
// pairing-expired banner only when flagged; Tailscale rows only when available.
func TestMenuConditionalRows(t *testing.T) {
	base := buildMenuRows(menuState{userLabel: "Default"})
	if findRow(base, "Pending saves") != menuAct(-1) {
		t.Error("Pending saves row must be absent with pendingN=0")
	}
	if findRow(base, "Download queue") != menuAct(-1) {
		t.Error("Download queue row must be absent with queueN=0")
	}
	if findRow(base, "Pairing expired") != menuAct(-1) {
		t.Error("Pairing-expired banner must be absent when not flagged")
	}
	if findRow(base, "Tailscale status") != menuAct(-1) {
		t.Error("Tailscale rows must be absent when unavailable")
	}
	full := buildMenuRows(menuState{userLabel: "Neo", pendingN: 2, queueN: 5, pairingExpired: true, tsAvail: true})
	if findRow(full, "Pending saves (2)") != actPushPending {
		t.Error("Pending saves (2) row missing/mismapped")
	}
	if findRow(full, "Download queue (5)") != actDownloadQueue {
		t.Error("Download queue (5) row missing/mismapped")
	}
	if findRow(full, "Pairing expired") != actRepair {
		t.Error("Pairing-expired banner missing/mismapped")
	}
	if findRow(full, "Tailscale: Reconnect") != actTsReconnect {
		t.Error("Tailscale reconnect row missing/mismapped")
	}
	// pairing-expired banner must be the FIRST row when present.
	if full[0].act != actRepair {
		t.Error("pairing-expired banner must be the top row")
	}
	// dynamic labels reflect state.
	if findRow(full, "Switch user (Neo)") != actSwitchUser {
		t.Error("Switch user label must carry the active profile")
	}
}

// TestBoxArtLabelReflectsState: the toggle label flips with fetch_covers state.
func TestBoxArtLabelReflectsState(t *testing.T) {
	off := buildMenuRows(menuState{userLabel: "d", coversOn: false})
	if findRow(off, "Box art: Downloaded games only") != actBoxArt {
		t.Error("covers-off label wrong")
	}
	on := buildMenuRows(menuState{userLabel: "d", coversOn: true})
	if findRow(on, "Box art: All covers") != actBoxArt {
		t.Error("covers-on label wrong")
	}
}

// TestParseButtonScript covers the LODOR_INPUT_SCRIPT decoder that feeds the off-hardware
// real-loop harness: token aliases map to the right logical buttons, whitespace/commas are
// both accepted, and an unknown token is a hard error (no silent drops that would desync a
// scripted run from the menu it drives).
func TestParseButtonScript(t *testing.T) {
	got, err := parseButtonScript("down, Down\td  a,b  START select up left right")
	if err != nil {
		t.Fatalf("parseButtonScript: %v", err)
	}
	want := []ui.Button{
		ui.BtnDown, ui.BtnDown, ui.BtnDown, ui.BtnConfirm, ui.BtnBack,
		ui.BtnStart, ui.BtnSelect, ui.BtnUp, ui.BtnLeft, ui.BtnRight,
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token %d = %v, want %v", i, got[i], want[i])
		}
	}
	if _, err := parseButtonScript("down,frobnicate,a"); err == nil {
		t.Fatalf("expected error on unknown token, got nil")
	}
}

// ---- #180A: post-mirror in-session re-seed --------------------------------------------

// writeFakeSeeder installs a fake bin/lodor-seed.sh under a fake LODOR_APPDIR that
// records its invocation (sentinel file) and emits the real seeder's summary line.
func writeFakeSeeder(t *testing.T, exitCode int) (appdir, sentinel string) {
	t.Helper()
	appdir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(appdir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel = filepath.Join(appdir, "seeded.sentinel")
	script := "#!/bin/sh\n: > \"" + sentinel + "\"\necho \"SEED overrides=3 skipped=1\"\nexit " +
		string(rune('0'+exitCode)) + "\n"
	if err := os.WriteFile(filepath.Join(appdir, "bin", "lodor-seed.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LODOR_APPDIR", appdir)
	return appdir, sentinel
}

// TestReseedOverridesRunsSeeder: reseedOverrides shells the seeder and logs the seeder's
// OWN overrides count (honest line, no fake numbers).
func TestReseedOverridesRunsSeeder(t *testing.T) {
	dir := t.TempDir()
	_, sentinel := writeFakeSeeder(t, 0)
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir}
	w.reseedOverrides()
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("seeder was not invoked")
	}
	b, err := os.ReadFile(filepath.Join(dir, "wizard.log"))
	if err != nil {
		t.Fatalf("wizard.log not written: %v", err)
	}
	if !strings.Contains(string(b), "seed: post-mirror re-seed, overrides=3") {
		t.Fatalf("honest overrides count missing from log: %q", b)
	}
}

// TestReseedOverridesSeederFailureIsLoud: a failing seeder logs FAILED, never a success line.
func TestReseedOverridesSeederFailureIsLoud(t *testing.T) {
	dir := t.TempDir()
	writeFakeSeeder(t, 1)
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir}
	w.reseedOverrides()
	b, _ := os.ReadFile(filepath.Join(dir, "wizard.log"))
	if !strings.Contains(string(b), "post-mirror re-seed FAILED") {
		t.Fatalf("failure not logged loudly: %q", b)
	}
	if strings.Contains(string(b), "re-seed, overrides=") {
		t.Fatalf("fake success line logged on failure: %q", b)
	}
}

// TestReseedOverridesMissingSeederNoop: no seeder on the host (off-hardware / non-muOS
// packaging) is a logged no-op, not a crash and not a silent pass.
func TestReseedOverridesMissingSeederNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LODOR_APPDIR", t.TempDir()) // exists, but has no bin/lodor-seed.sh
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir}
	w.reseedOverrides()
	b, _ := os.ReadFile(filepath.Join(dir, "wizard.log"))
	if !strings.Contains(string(b), "re-seed skipped - no seeder") {
		t.Fatalf("missing-seeder no-op not logged: %q", b)
	}
}

// TestScreenMirrorArgsReseedsOnSuccessOnly: the #180A seam itself — a mirror that exits 0
// re-seeds in-session; a failed mirror does NOT (overrides state untouched on failure).
func TestScreenMirrorArgsReseedsOnSuccessOnly(t *testing.T) {
	dir := t.TempDir()
	_, sentinel := writeFakeSeeder(t, 0)
	draw := func(*ui.Canvas) {}

	w := &wizard{t: ui.DefaultTheme(), dataDir: dir, bin: "/bin/true"}
	if rc := w.screenMirrorArgs(draw); rc != 0 {
		t.Fatalf("mirror rc = %d, want 0", rc)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("successful mirror must re-seed in-session (#180A)")
	}

	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	w = &wizard{t: ui.DefaultTheme(), dataDir: dir, bin: "/bin/false"}
	if rc := w.screenMirrorArgs(draw); rc == 0 {
		t.Fatal("mirror rc = 0 from /bin/false?")
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("failed mirror must NOT re-seed")
	}
}

// ── Handoff v1: LISTSTATE parsing + Continue-row helpers ─────────────────

func TestParseStateLine(t *testing.T) {
	line := `LISTSTATE id=618 slot=auto compat=0 age=7200 size=40960 origin=lodor/knulli/gpsp@0.91/arm64 why="different architecture (arm64 vs armhf)" name="Woody Pop [2026-07-07] (lodor sauto abcdef12).state"`
	kv := parseStateLine(line)
	if kv["id"] != "618" || kv["slot"] != "auto" || kv["compat"] != "0" {
		t.Fatalf("scalars: %v", kv)
	}
	if kv["origin"] != "lodor/knulli/gpsp@0.91/arm64" {
		t.Fatalf("origin: %q", kv["origin"])
	}
	if kv["why"] != "different architecture (arm64 vs armhf)" {
		t.Fatalf("quoted why: %q", kv["why"])
	}
	if kv["name"] != "Woody Pop [2026-07-07] (lodor sauto abcdef12).state" {
		t.Fatalf("quoted name: %q", kv["name"])
	}
}

func TestParseStateLineDegradesGracefully(t *testing.T) {
	// truncated quote — parser stops, never panics, keeps what it had
	kv := parseStateLine(`LISTSTATE id=5 why="unterminated`)
	if kv["id"] != "5" {
		t.Fatalf("id lost on malformed tail: %v", kv)
	}
}

func TestHumanAge(t *testing.T) {
	for _, c := range []struct {
		s    int64
		want string
	}{{30, "just now"}, {600, "10m ago"}, {7200, "2h ago"}, {200000, "2d ago"}} {
		if got := humanAge(c.s); got != c.want {
			t.Fatalf("humanAge(%d) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestOriginLabel(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"lodor/knulli/gpsp@0.91/arm64", "a Knulli device"},
		{"lodor/lodoros/gambatte/armhf", "a LodorOS device"},
		{"foreign:builtin", "another app"},
		{"garbage", "unknown device"},
	} {
		if got := originLabel(c.in); got != c.want {
			t.Fatalf("originLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── Launch card decision helpers (launchcard.go) ─────────────────────────

func TestListSavesLocal(t *testing.T) {
	out := "12\t2026-07-06\tflip\t4KB\n34\t2026-07-05\tbrick\t4KB\tCURRENT\nLOCAL=older\n"
	if got := listSavesLocal(out); got != "older" {
		t.Fatalf("LOCAL = %q", got)
	}
	if got := listSavesLocal("12\t2026-07-06\n"); got != "" {
		t.Fatalf("missing trailer = %q", got)
	}
}

func TestNewestUnknownCompatState(t *testing.T) {
	out := strings.Join([]string{
		`LISTSTATE id=1 slot=0 compat=1 known=1 age=100 size=1 origin=lodor/knulli/gpsp/arm64 why="-" name="a"`,   // known -> not news
		`LISTSTATE id=2 slot=auto compat=1 known=0 age=7200 size=1 origin=lodor/muos/gpsp/arm64 why="-" name="b"`, // news, older
		`LISTSTATE id=3 slot=1 compat=1 known=0 age=60 size=1 origin=lodor/knulli/gpsp/arm64 why="-" name="c"`,    // news, newest
		`LISTSTATE id=4 slot=2 compat=0 known=0 age=5 size=1 origin=foreign:builtin why="non-lodor" name="d"`,     // incompatible
		"RESULT liststates=4 compatstates=3 reason=ok",
	}, "\n")
	best, ok := newestUnknownCompatState(out)
	if !ok || best.id != "3" {
		t.Fatalf("best = %+v ok=%v, want id 3", best, ok)
	}
	if !strings.Contains(best.label, "Slot 1") || !strings.Contains(best.label, "a Knulli device") {
		t.Fatalf("label = %q", best.label)
	}
	if _, ok := newestUnknownCompatState("RESULT liststates=0 compatstates=0 reason=none"); ok {
		t.Fatal("no rows must mean no news")
	}
}

func TestHumanDur(t *testing.T) {
	for _, c := range []struct {
		s    int64
		want string
	}{{30, "under a minute"}, {600, "10m"}, {15120, "4h 12m"}} {
		if got := humanDur(c.s); got != c.want {
			t.Fatalf("humanDur(%d) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestPlaytimeLineFor(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SDCARD_PATH", base)
	dir := filepath.Join(base, ".userdata", "shared", ".lodor", "playtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tsv := "k1\tWoody Pop.gg\t15120\t7\t1751800000\nk2\tAlex Kidd.gg\t59\t1\t1751800000\n"
	if err := os.WriteFile(filepath.Join(dir, "totals.tsv"), []byte(tsv), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := playtimeLineFor("Woody Pop.gg"); got != "Played 4h 12m across 7 sessions" {
		t.Fatalf("line = %q", got)
	}
	if got := playtimeLineFor("Nope.gg"); got != "" {
		t.Fatalf("missing rom line = %q", got)
	}
}
