package playtime

// Playtime store tests (task #146): uptime-delta duration math under simulated
// clock jumps, session-identity dedup, peer-record merge dedup, and the exact
// totals.tsv contract columns.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("SDCARD_PATH", base)
	return base
}

// TestComputeSessionClockJumps: duration comes from the uptime DELTA only —
// wall-clock jumps in either direction must not distort it, and start_utc is
// back-computed from the (sane) end clock.
func TestComputeSessionClockJumps(t *testing.T) {
	cases := []struct {
		name      string
		st        Stage
		uptimeNow float64
		wallEnd   int64
		wantSecs  int64
		wantStart int64
		wantOK    bool
	}{
		{
			name:      "steady clock",
			st:        Stage{WallStart: 1_000_000, UptimeStart: 100},
			uptimeNow: 400, wallEnd: 1_000_300,
			wantSecs: 300, wantStart: 1_000_000, wantOK: true,
		},
		{
			name: "device booted at epoch, NTP jumped clock FORWARD mid-session",
			// wall start says 1970; wall end is real time. Wall delta would be
			// ~56 years — uptime delta says 300s.
			st:        Stage{WallStart: 3600, UptimeStart: 50},
			uptimeNow: 350, wallEnd: 1_782_000_000,
			wantSecs: 300, wantStart: 1_782_000_000 - 300, wantOK: true,
		},
		{
			name: "NTP corrected clock BACKWARD mid-session",
			// wall delta is negative — uptime delta stays 120s.
			st:        Stage{WallStart: 2_000_000_000, UptimeStart: 1000},
			uptimeNow: 1120, wallEnd: 1_782_000_000,
			wantSecs: 120, wantStart: 1_782_000_000 - 120, wantOK: true,
		},
		{
			name:      "uptime went backwards (stage predates a reboot) — refuse",
			st:        Stage{WallStart: 1_000_000, UptimeStart: 5000},
			uptimeNow: 60, wallEnd: 1_000_300,
			wantOK: false,
		},
		{
			name:      "sub-second bounce — refuse",
			st:        Stage{WallStart: 1_000_000, UptimeStart: 100},
			uptimeNow: 100.4, wallEnd: 1_000_001,
			wantOK: false,
		},
		{
			name:      "rounding: 299.6s delta records 300",
			st:        Stage{WallStart: 1_000_000, UptimeStart: 0.2},
			uptimeNow: 299.8, wallEnd: 1_000_300,
			wantSecs: 300, wantStart: 1_000_000, wantOK: true,
		},
	}
	for _, c := range cases {
		start, secs, ok := ComputeSession(c.st, c.uptimeNow, c.wallEnd)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if secs != c.wantSecs || start != c.wantStart {
			t.Errorf("%s: (start,secs) = (%d,%d), want (%d,%d)", c.name, start, secs, c.wantStart, c.wantSecs)
		}
	}
}

func TestStageRoundTripAndConsume(t *testing.T) {
	// StagePath is a package constant under /tmp; give this test its own copy of
	// the flow by staging + taking through the API.
	if err := WriteStage("/Roms/GBA/Game.gba", 123, 45.5); err != nil {
		t.Fatal(err)
	}
	st, ok := TakeStage("/Roms/GBA/Game.gba")
	if !ok || st.WallStart != 123 || st.UptimeStart != 45.5 {
		t.Fatalf("TakeStage = %+v, %v", st, ok)
	}
	// consumed: a second take finds nothing (idempotent double exit-hook)
	if _, ok := TakeStage("/Roms/GBA/Game.gba"); ok {
		t.Error("stage not consumed on first take")
	}
	// a different game's stage is not ours to bill
	if err := WriteStage("/Roms/GBA/Other.gba", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := TakeStage("/Roms/GBA/Game.gba"); ok {
		t.Error("foreign stage billed to the wrong game")
	}
	_ = os.Remove(StagePath)
}

// TestSessionDedup: an identical session line appended twice (re-run hook /
// crash replay) counts ONCE in sessions, totals, and the built record.
func TestSessionDedup(t *testing.T) {
	env(t)
	s := Session{Key: "md5:abc", Rom: "Game (USA).gba", StartUTC: 1000, Secs: 300}
	for i := 0; i < 2; i++ {
		if err := AppendSession(s); err != nil {
			t.Fatal(err)
		}
	}
	if got := LocalSessions(); len(got) != 1 {
		t.Fatalf("LocalSessions = %d entries, want 1 (dedup by key+start+secs)", len(got))
	}
	rec := BuildRecord("md5:abc", "dev-a")
	if rec.Plays != 1 || rec.Secs != 300 || rec.LastUTC != 1300 {
		t.Errorf("BuildRecord = %+v, want plays=1 secs=300 last=1300", rec)
	}
}

// TestTornTailLineSkipped: a torn (power-yank) tail line is skipped, healthy
// lines survive.
func TestTornTailLineSkipped(t *testing.T) {
	base := env(t)
	if err := AppendSession(Session{Key: "k", Rom: "G.gba", StartUTC: 10, Secs: 60}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(base, ".userdata", "shared", ".lodor", "playtime", "sessions.jsonl"),
		os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"key":"k","rom":"G.gba","start_`) // torn mid-write
	_ = f.Close()
	if got := LocalSessions(); len(got) != 1 {
		t.Fatalf("LocalSessions with torn tail = %d, want 1", len(got))
	}
}

// TestMergePeerDedup: newest record per (device,key) wins; replaying the same
// or an older record changes nothing; our own device's record is never merged.
func TestMergePeerDedup(t *testing.T) {
	env(t)
	rec := Record{V: 1, Key: "md5:abc", Rom: "Game (USA).gba", Device: "flip", Secs: 600, Plays: 2, LastUTC: 2000}

	changed, err := MergePeer(rec, "brick")
	if err != nil || !changed {
		t.Fatalf("first merge: changed=%v err=%v", changed, err)
	}
	// identical replay: no change
	if changed, _ = MergePeer(rec, "brick"); changed {
		t.Error("identical record re-merged")
	}
	// older record: no change
	old := rec
	old.LastUTC = 1500
	old.Plays = 1
	if changed, _ = MergePeer(old, "brick"); changed {
		t.Error("older record replaced a newer one")
	}
	// newer record: replaces
	newer := rec
	newer.LastUTC = 3000
	newer.Plays = 3
	newer.Secs = 900
	if changed, _ = MergePeer(newer, "brick"); !changed {
		t.Error("newer record did not replace")
	}
	// our own device's pushed record pulled back: never merged
	self := rec
	self.Device = "brick"
	if changed, _ = MergePeer(self, "brick"); changed {
		t.Error("own device's record merged as a peer")
	}

	totals := MergedTotals()
	if len(totals) != 1 {
		t.Fatalf("totals = %d rows, want 1", len(totals))
	}
	if totals[0].Secs != 900 || totals[0].Plays != 3 || totals[0].LastUTC != 3000 {
		t.Errorf("merged total = %+v, want secs=900 plays=3 last=3000", totals[0])
	}
}

// TestMergedTotalsLocalPlusPeers: local sessions and two peers sum per key;
// rows come out newest-first.
func TestMergedTotalsLocalPlusPeers(t *testing.T) {
	env(t)
	// local: 2 sessions of GameA, 1 of GameB
	_ = AppendSession(Session{Key: "md5:a", Rom: "A.gba", StartUTC: 1000, Secs: 100})
	_ = AppendSession(Session{Key: "md5:a", Rom: "A.gba", StartUTC: 2000, Secs: 200})
	_ = AppendSession(Session{Key: "tag:GBA/B.gba", Rom: "B.gba", StartUTC: 9000, Secs: 50})
	// peers: flip played GameA too
	_, _ = MergePeer(Record{V: 1, Key: "md5:a", Rom: "A.gba", Device: "flip", Secs: 400, Plays: 1, LastUTC: 5000}, "brick")

	totals := MergedTotals()
	if len(totals) != 2 {
		t.Fatalf("totals = %d rows, want 2", len(totals))
	}
	if totals[0].Key != "tag:GBA/B.gba" {
		t.Errorf("row 0 = %s, want the newest (B at 9050)", totals[0].Key)
	}
	a := totals[1]
	if a.Secs != 700 || a.Plays != 3 || a.LastUTC != 5000 {
		t.Errorf("GameA merged = %+v, want secs=700 plays=3 last=5000", a)
	}
}

// TestTotalsTSVContract: columns EXACTLY key\trom_basename\tsecs\tplays\tlast_utc.
func TestTotalsTSVContract(t *testing.T) {
	env(t)
	_ = AppendSession(Session{Key: "md5:a", Rom: "Game (USA).gba", StartUTC: 1000, Secs: 300})
	if err := WriteTotals(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(TotalsTSVPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("tsv = %d lines, want 1", len(lines))
	}
	cols := strings.Split(lines[0], "\t")
	if len(cols) != 5 {
		t.Fatalf("tsv cols = %d, want 5 (%q)", len(cols), lines[0])
	}
	want := []string{"md5:a", "Game (USA).gba", "300", "1", "1300"}
	for i := range want {
		if cols[i] != want[i] {
			t.Errorf("col %d = %q, want %q", i, cols[i], want[i])
		}
	}
}

// TestBuildRecordTailCap: the pushed record's session tail stays bounded.
func TestBuildRecordTailCap(t *testing.T) {
	env(t)
	for i := 0; i < sessionsTailCap+10; i++ {
		_ = AppendSession(Session{Key: "k", Rom: "G.gba", StartUTC: int64(1000 + i*100), Secs: 60})
	}
	rec := BuildRecord("k", "dev")
	if len(rec.Sessions) != sessionsTailCap {
		t.Errorf("tail = %d, want %d", len(rec.Sessions), sessionsTailCap)
	}
	if rec.Plays != int64(sessionsTailCap+10) {
		t.Errorf("plays = %d, want %d (totals never truncated by the tail cap)", rec.Plays, sessionsTailCap+10)
	}
}
