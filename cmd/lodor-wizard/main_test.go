package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lodor/buildinfo"
	"lodor/covercancel"
	"lodor/syncstamp"
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

// TestMenuLastSyncedRow (#43): the "Last synced" row appears ONLY once a verified
// sync stamped the record, renders the relative age, and drills into Recent
// activity. Never synced = no row (no fake freshness).
func TestMenuLastSyncedRow(t *testing.T) {
	base := buildMenuRows(menuState{userLabel: "Default"})
	if findRow(base, "Last synced") != menuAct(-1) {
		t.Error("Last synced row must be absent before any verified sync")
	}
	rows := buildMenuRows(menuState{userLabel: "Default", lastSync: "2h ago"})
	if findRow(rows, "Last synced: 2h ago") != actRecent {
		t.Error("Last synced row missing or not mapped to Recent activity")
	}
	// It sits directly under Sync now — freshness beside the action that refreshes it.
	for i, r := range rows {
		if r.label == "Sync now" {
			if i+1 >= len(rows) || !strings.HasPrefix(rows[i+1].label, "Last synced:") {
				t.Error("Last synced row must directly follow Sync now")
			}
			break
		}
	}
}

// TestLastSyncAgeReadsStamp (#43): the wizard's menu-state reader renders the
// engine's stamp as a relative age, and "" when the stamp is missing.
func TestLastSyncAgeReadsStamp(t *testing.T) {
	dir := t.TempDir()
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir}
	if got := w.lastSyncAge(); got != "" {
		t.Fatalf("lastSyncAge with no stamp = %q, want \"\"", got)
	}
	if err := syncstamp.WriteAt(dir, time.Now().Unix()-7200, 3, 1); err != nil {
		t.Fatal(err)
	}
	if got := w.lastSyncAge(); got != "2h ago" {
		t.Fatalf("lastSyncAge = %q, want \"2h ago\"", got)
	}
}

// ── lodor#42: cancelable background ops ──────────────────────────────────────

// writeFakeEngine writes a shell script standing in for lodor-sync.
func writeFakeEngine(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-engine.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunEngineCancellableBCancels: B during a long op writes the cancel sentinel
// (the engine's --cancellable contract), the run reports cancelled, the engine's
// argv carried --cancellable, and the sentinel never leaks past the run.
func TestRunEngineCancellableBCancels(t *testing.T) {
	covercancel.Clear()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	// Fake engine: record argv, then behave like a real armed mode — poll the
	// sentinel between "items" and exit 0 the moment it appears; 7 = never saw it.
	bin := writeFakeEngine(t, `echo "$@" > `+argsFile+`
i=0
while [ $i -lt 100 ]; do
	[ -f /tmp/lodor-cover-cancel ] && { echo "RESULT pushed=1 total=3 stuck=2 cancelled=1"; exit 0; }
	i=$((i+1)); sleep 0.1
done
exit 7`)
	w := &wizard{t: ui.DefaultTheme(), dataDir: t.TempDir(), bin: bin}
	w.in = ui.NewScriptedSource([]ui.Button{ui.BtnBack})
	out, rc, cancelled := w.runEngineCancellable("Sync now", "phase", func(*ui.Canvas) {}, "--push-pending")
	if !cancelled || rc != 0 {
		t.Fatalf("cancelled run: rc=%d cancelled=%v out=%q", rc, cancelled, out)
	}
	if !strings.Contains(out, "cancelled=1") {
		t.Fatalf("engine output not captured: %q", out)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--cancellable") {
		t.Fatalf("engine must be invoked with --cancellable, got argv: %q", args)
	}
	if covercancel.Requested() {
		t.Fatal("sentinel must be cleared after a cancelled run (must not leak into the next op)")
	}
}

// TestRunEngineCancellableCompletes: no B — output captured, rc honest, not cancelled.
// Also covers the nil-input path (off-hardware callers without an input source).
func TestRunEngineCancellableCompletes(t *testing.T) {
	covercancel.Clear()
	bin := writeFakeEngine(t, `echo "RESULT pushed=2 total=2 stuck=0"; exit 0`)
	w := &wizard{t: ui.DefaultTheme(), dataDir: t.TempDir(), bin: bin}
	out, rc, cancelled := w.runEngineCancellable("Sync now", "phase", func(*ui.Canvas) {}, "--push-pending")
	if cancelled || rc != 0 || !strings.Contains(out, "RESULT pushed=2") {
		t.Fatalf("clean run: rc=%d cancelled=%v out=%q", rc, cancelled, out)
	}
	bin = writeFakeEngine(t, `exit 4`)
	w = &wizard{t: ui.DefaultTheme(), dataDir: t.TempDir(), bin: bin}
	_, rc, cancelled = w.runEngineCancellable("Sync now", "phase", func(*ui.Canvas) {}, "--pull-saves")
	if cancelled || rc != 4 {
		t.Fatalf("failing run: rc=%d cancelled=%v, want rc=4 uncancelled", rc, cancelled)
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

// ---- lodor#31: honest pairing-failure mapping -------------------------------------------

func TestPairFailureMapping(t *testing.T) {
	msg, retry := pairFailure(3, "PAIRFAIL exchange: network error\nRESULT paired=0 scopes_ok=0\n")
	if !strings.Contains(msg, "Couldn't reach your server") {
		t.Fatalf("rc=3 must blame reachability: %q", msg)
	}
	if strings.Contains(msg, "fresh code") {
		t.Fatalf("rc=3 must never tell the user to generate a fresh code: %q", msg)
	}
	if retry != stepServer {
		t.Fatalf("rc=3 retry = %d, want stepServer", retry)
	}

	msg, retry = pairFailure(6, "RESULT paired=0 scopes_ok=0\n")
	if !strings.Contains(msg, "Pairing expired") || retry != stepCode {
		t.Fatalf("rc=6 = %q retry=%d, want expired copy + stepCode", msg, retry)
	}

	msg, retry = pairFailure(4, "PAIRFAIL exchange: invalid or expired code\nRESULT paired=0 scopes_ok=0\n")
	if !strings.Contains(msg, "invalid or expired code") || !strings.Contains(msg, "fresh code") || retry != stepCode {
		t.Fatalf("rc=4 must carry the PAIRFAIL reason + fresh-code advice: %q retry=%d", msg, retry)
	}

	// flag-missing: rc=0 but no paired=1 in the output — the existing fresh-code copy.
	msg, retry = pairFailure(0, "garbage\n")
	if !strings.Contains(msg, "Generate a fresh code") || retry != stepCode {
		t.Fatalf("flag-missing = %q retry=%d", msg, retry)
	}
}

// ---- lodor#35: certificate trust for self-signed servers --------------------------------

// TestCertFailureDetection: the trust offer keys ONLY on the engine's reason=tls machine
// token with a non-zero rc — never on human text, never on a successful run.
func TestCertFailureDetection(t *testing.T) {
	certOut := "PAIRFAIL exchange: server certificate could not be verified\nRESULT paired=0 scopes_ok=0 reason=tls\n"
	if !certFailure(3, certOut) {
		t.Fatal("rc=3 + reason=tls must classify as a certificate failure")
	}
	if certFailure(0, certOut) {
		t.Fatal("rc=0 must never classify as a certificate failure")
	}
	if certFailure(3, "PAIRFAIL exchange: network error\nRESULT paired=0 scopes_ok=0\n") {
		t.Fatal("plain unreachable (no reason=tls) must not classify")
	}
	if certFailure(4, "RESULT paired=0 scopes_ok=0 reason=other\n") {
		t.Fatal("a different reason token must not classify")
	}
}

// TestCertTrustCopyRules: plain language — "certificate", never a bare TLS/SSL, and the
// honest one-line tradeoff ("only do this for your own server") is present.
func TestCertTrustCopyRules(t *testing.T) {
	for _, s := range []string{certTrustTitle, certTrustBody, certTrustOption} {
		if strings.Contains(s, "TLS") || strings.Contains(s, "SSL") {
			t.Fatalf("copy must not say TLS/SSL: %q", s)
		}
	}
	if !strings.Contains(certTrustBody, "certificate") || !strings.Contains(certTrustOption, "certificate") {
		t.Fatal("copy must name the certificate")
	}
	if !strings.Contains(certTrustBody, "self-signed home servers") {
		t.Fatalf("body must normalize the self-signed case: %q", certTrustBody)
	}
	if !strings.Contains(certTrustBody, "only do this for your own server") {
		t.Fatalf("body must carry the honest tradeoff line: %q", certTrustBody)
	}
}

// writeFakeCertEngine installs a fake lodor-sync that answers --pair with the engine's
// REAL certificate-failure contract (reason=tls, rc=3) until --set-server has been
// re-run with --insecure (recorded via a marker file), after which pairing succeeds —
// the exact engine behavior the trust-retry flow rides.
func writeFakeCertEngine(t *testing.T, dir string) (bin, marker string) {
	t.Helper()
	marker = filepath.Join(dir, "insecure-set")
	script := `#!/bin/sh
case "$1" in
--set-server)
	for a in "$@"; do [ "$a" = "--insecure" ] && : > "` + marker + `"; done
	echo "RESULT server_set=1"; exit 0 ;;
--pair)
	if [ -f "` + marker + `" ]; then
		echo "RESULT paired=1 scopes_ok=1"; exit 0
	fi
	echo "PAIRFAIL exchange: server certificate could not be verified" >&2
	echo "RESULT paired=0 scopes_ok=0 reason=tls"; exit 3 ;;
esac
exit 0
`
	bin = filepath.Join(dir, "fake-lodor-sync")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, marker
}

// scriptedBtns feeds a fixed button sequence, then BtnBack forever (same convention as
// driveOnboarding: a screen that ignores the script backs out instead of hanging).
func scriptedBtns(seq ...ui.Button) func() ui.Button {
	ch := make(chan ui.Button, len(seq))
	for _, b := range seq {
		ch <- b
	}
	return func() ui.Button {
		select {
		case b := <-ch:
			return b
		default:
			return ui.BtnBack
		}
	}
}

// TestPairServerCertTrustFlow: cert failure -> trust choice (A on the top row) ->
// --set-server --insecure persisted -> automatic re-pair with the same code -> stepDevice.
func TestPairServerCertTrustFlow(t *testing.T) {
	dir := t.TempDir()
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir}
	bin, marker := writeFakeCertEngine(t, dir)
	w.bin = bin
	next := w.pairServer("https://romm.home", "X7K2", func(*ui.Canvas) {}, scriptedBtns(ui.BtnConfirm))
	if next != stepDevice {
		t.Fatalf("trusted pair must land on stepDevice, got %d (errMsg=%q)", next, w.errMsg)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("trusting must re-run --set-server with --insecure (marker missing)")
	}
}

// TestPairServerCertGoBack: choosing "Go back" (second row) returns to the server step
// and never writes the insecure setting.
func TestPairServerCertGoBack(t *testing.T) {
	dir := t.TempDir()
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir}
	bin, marker := writeFakeCertEngine(t, dir)
	w.bin = bin
	next := w.pairServer("https://romm.home", "X7K2", func(*ui.Canvas) {}, scriptedBtns(ui.BtnDown, ui.BtnConfirm))
	if next != stepServer {
		t.Fatalf("Go back must return to stepServer, got %d", next)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Go back must NOT write the insecure setting")
	}
	// B on the choice screen is the same walk-back.
	if next := w.pairServer("https://romm.home", "X7K2", func(*ui.Canvas) {}, scriptedBtns(ui.BtnBack)); next != stepServer {
		t.Fatalf("B on the trust screen must return to stepServer, got %d", next)
	}
}

// TestPairServerCertHTTPNeverOffers: reason=tls on a plain-http server (defensive — the
// engine shouldn't produce it) maps to the normal lodor#31 error screen, no trust offer.
func TestPairServerCertHTTPNeverOffers(t *testing.T) {
	dir := t.TempDir()
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir}
	bin, marker := writeFakeCertEngine(t, dir)
	w.bin = bin
	next := w.pairServer("http://romm.home", "X7K2", func(*ui.Canvas) {}, scriptedBtns())
	if next != stepError {
		t.Fatalf("http must take the normal error path, got %d", next)
	}
	if w.retry != stepServer || !strings.Contains(w.errMsg, "Couldn't reach your server") {
		t.Fatalf("http cert-ish failure must keep the rc=3 mapping: retry=%d msg=%q", w.retry, w.errMsg)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("http path must never write the insecure setting")
	}
}

// TestPairServerPlainFailureUnchanged: a non-TLS pairing failure keeps the exact
// lodor#31 mapping (no trust screen consumed a button, errMsg/retry as before).
func TestPairServerPlainFailureUnchanged(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in --pair) echo \"PAIRFAIL exchange: invalid or expired code\" >&2; echo \"RESULT paired=0 scopes_ok=0\"; exit 4;; esac\nexit 0\n"
	bin := filepath.Join(dir, "fake-lodor-sync")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	w := &wizard{t: ui.DefaultTheme(), dataDir: dir, bin: bin}
	next := w.pairServer("https://romm.home", "BAD1", func(*ui.Canvas) {}, scriptedBtns())
	if next != stepError || w.retry != stepCode || !strings.Contains(w.errMsg, "fresh code") {
		t.Fatalf("rc=4 mapping changed: next=%d retry=%d msg=%q", next, w.retry, w.errMsg)
	}
}

// ---- lodor#38: the scope warning keys off the engine's scopes_ok flag -------------------

func TestScopesFlagParses(t *testing.T) {
	if resultFlag("RESULT paired=1 scopes_ok=0", "scopes_ok") {
		t.Fatal("scopes_ok=0 must not read as ok")
	}
	if !resultFlag("RESULT paired=1 scopes_ok=1", "scopes_ok") {
		t.Fatal("scopes_ok=1 must read as ok")
	}
}

// ---- lodor#32: host-OS copy table --------------------------------------------------------

func TestHostCopyTable(t *testing.T) {
	mu, kn := hostCopyFor("muos"), hostCopyFor("knulli")
	if hostCopyFor("") != mu || hostCopyFor("unknown") != mu {
		t.Fatal("unset/unknown host must degrade to the muOS copy")
	}
	if !strings.Contains(mu.wifiOpen, "muOS Settings") || mu.updateVenue != "App Downloader" {
		t.Fatalf("muos copy wrong: %+v", mu)
	}
	if strings.Contains(kn.wifiOpen, "muOS") || strings.Contains(kn.noWifi, "muOS") || kn.osName != "Knulli" {
		t.Fatalf("knulli copy still names muOS: %+v", kn)
	}
	if kn.updateVenue != "GitHub zip" {
		t.Fatalf("knulli venue = %q", kn.updateVenue)
	}
}

// knulli#2: the uninstall finish copy must name Knulli's four real install paths — there
// is no "Lodor app" to delete on that host; muOS keeps the app-delete instruction.
func TestRemoveDoneCopyPerHost(t *testing.T) {
	mu, kn := hostCopyFor("muos"), hostCopyFor("knulli")
	if !strings.Contains(mu.removeDone, "Delete the Lodor app") {
		t.Fatalf("muos removeDone lost the app-delete instruction: %q", mu.removeDone)
	}
	for _, want := range []string{
		"system/lodor",
		"system/scripts/lodor-hook.sh",
		"system/services/lodor",
		"roms/ports/Lodor.sh",
		"/userdata",
	} {
		if !strings.Contains(kn.removeDone, want) {
			t.Fatalf("knulli removeDone missing %q: %q", want, kn.removeDone)
		}
	}
	if strings.Contains(kn.removeDone, "Lodor app") {
		t.Fatalf("knulli removeDone still tells the user to delete an app: %q", kn.removeDone)
	}
}

func TestUpdateInstructionsPerHost(t *testing.T) {
	kb := updateInstructions("knulli", "0.9.8", "0.9.7")
	for _, want := range []string{"Lodor-Knulli-0.9.8.zip", "/userdata", "pairing is kept", "0.9.7"} {
		if !strings.Contains(kb, want) {
			t.Fatalf("knulli update body missing %q: %q", want, kb)
		}
	}
	if strings.Contains(kb, "muOS") {
		t.Fatalf("knulli update body names muOS: %q", kb)
	}
	mb := updateInstructions("muos", "0.9.8", "0.9.7")
	if !strings.Contains(mb, "App Downloader") || !strings.Contains(mb, "0.9.8") {
		t.Fatalf("muos update body wrong: %q", mb)
	}
}

// ---- lodor#36: device-name preset ---------------------------------------------------------

func TestDefaultDeviceNameFrom(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "model")
	if err := os.WriteFile(p, []byte("Anbernic RG34XX SP\x00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := defaultDeviceNameFrom(p); got != "Anbernic RG34XX SP" {
		t.Fatalf("model preset = %q", got)
	}
	if got := defaultDeviceNameFrom(filepath.Join(dir, "missing")); got == "" {
		t.Fatal("missing model must fall back (hostname/RG34XX), never empty")
	}
	if err := os.WriteFile(p, []byte("\x00\x00 \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := defaultDeviceNameFrom(p); got == "" {
		t.Fatal("NUL-only model must fall back, never empty")
	}
}

// ---- lodor#39: validate screen carries the register outcome ------------------------------

func TestValidateBodyRegisteredRow(t *testing.T) {
	b := validateBody(true, true, true)
	if !strings.Contains(b, "Device registered: yes") {
		t.Fatalf("registered row missing: %q", b)
	}
	if strings.Contains(b, "retried automatically") {
		t.Fatalf("registered=yes must not carry the retry line: %q", b)
	}
	b = validateBody(true, true, false)
	if !strings.Contains(b, "Device registered: no") || !strings.Contains(b, "Saves start syncing once this device can register - retried automatically.") {
		t.Fatalf("unregistered body wrong: %q", b)
	}
}

// ---- lodor#45: version visibility ---------------------------------------------------------

func TestMenuTitleCarriesVersion(t *testing.T) {
	if !strings.Contains(menuTitle(), buildinfo.Version) {
		t.Fatalf("menu title %q must carry buildinfo version %q", menuTitle(), buildinfo.Version)
	}
}

func TestMenuUpdateBadgeVenue(t *testing.T) {
	rows := buildMenuRows(menuState{userLabel: "d", updateAvail: "0.9.8", updateVenue: "App Downloader"})
	if findRow(rows, "Update available (0.9.8) - App Downloader") != actCheckUpdates {
		t.Fatal("muos venue suffix missing from update badge")
	}
	rows = buildMenuRows(menuState{userLabel: "d", updateAvail: "0.9.8", updateVenue: "GitHub zip"})
	if findRow(rows, "Update available (0.9.8) - GitHub zip") != actCheckUpdates {
		t.Fatal("knulli venue suffix missing from update badge")
	}
	rows = buildMenuRows(menuState{userLabel: "d", updateAvail: "0.9.8"})
	if findRow(rows, "Update available (0.9.8)") != actCheckUpdates {
		t.Fatal("badge must still appear without a venue")
	}
}

func TestVersionFlashOnce(t *testing.T) {
	w := &wizard{t: ui.DefaultTheme(), dataDir: t.TempDir()}
	if got := w.versionFlash("dev"); got != "" {
		t.Fatalf("dev build must never flash: %q", got)
	}
	if got := w.versionFlash("0.9.7"); got != "" {
		t.Fatalf("first-seen version must stamp quietly: %q", got)
	}
	if got := w.versionFlash("0.9.7"); got != "" {
		t.Fatalf("unchanged version must not flash: %q", got)
	}
	if got := w.versionFlash("0.9.8"); got != "Updated to 0.9.8." {
		t.Fatalf("changed version flash = %q", got)
	}
	if got := w.versionFlash("0.9.8"); got != "" {
		t.Fatalf("flash must be one-shot: %q", got)
	}
}
