// Package playtime is LodorOS's engine-owned playtime tracker (task #146).
//
// DESIGN PROVENANCE: the games/sessions split, per-session rows + rolled-up
// totals, and merge-on-conflict semantics follow Allium's playtime schema
// (allium games/game_sessions, MIT — see engine/CREDITS.md), reworked for
// LodorOS's constraints: JSONL + TSV on the SD card instead of SQLite (the
// engine is CGO-free and SQLite is banned), and cross-device merge via
// .lodortime meta-saves on the existing RomM saves transport — the twist no
// upstream has, because none of them have a server.
//
// CLOCK-JUMP IMMUNITY (feeds #147's honesty): a session's duration is NEVER
// wall-clock end minus wall-clock start — RTC-less handhelds boot at epoch and
// NTP may yank the clock either way mid-session. Duration is the /proc/uptime
// DELTA (monotonic since boot); the session's start_utc is back-computed as
// wall_end − duration, so even a session that started "in 1970" lands at a
// sane place once the clock is sane at exit.
//
// STORE (all under $SDCARD/.userdata/shared/.lodor/playtime/):
//
//	sessions.jsonl — one JSON object per finished LOCAL session (append-only)
//	peers/<id>.json — newest .lodortime record per (device,key) pulled from RomM
//	totals.json    — rebuilt roll-up, local + peers merged
//	totals.tsv     — the launcher-facing contract file, columns EXACTLY:
//	                 key \t rom_basename \t secs \t plays \t last_utc
//	                 (last_utc = integer unix seconds; rows newest-first)
//
// Whole-file writes are temp+rename+fsync (FAT32 — a yank must never zero a
// good file); sessions.jsonl is append-only and its reader skips torn lines.
//
// CGO-free, stdlib only. No network anywhere in this package.
package playtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// StagePath is where --session-start stages the in-flight session. /tmp is
// tmpfs on every target: a crash or power yank simply forgets the session —
// never a torn record on the card.
const StagePath = "/tmp/lodor-session.json"

// sessionsTailCap bounds how many recent sessions ride inside a pushed
// .lodortime record — enough for peers to audit recency, small enough to stay
// a compact meta-save.
const sessionsTailCap = 64

// Stage is the in-flight session marker --session-start writes.
type Stage struct {
	Rom         string  `json:"rom"`          // the launched ROM path, verbatim
	WallStart   int64   `json:"wall_start"`   // unix seconds at launch (display only, never duration)
	UptimeStart float64 `json:"uptime_start"` // /proc/uptime seconds at launch (the duration anchor)
}

// Session is one finished play session (a sessions.jsonl line).
type Session struct {
	Key      string `json:"key"` // md5:<hash> or tag:<TAG>/<canonical basename>
	Rom      string `json:"rom"` // canonical rom basename (display + fallback identity)
	Device   string `json:"device,omitempty"`
	StartUTC int64  `json:"start_utc"` // wall_end − secs (clock-jump-immune)
	Secs     int64  `json:"secs"`
}

// Record is the compact per-ROM .lodortime payload one device pushes: that
// device's AUTHORITATIVE roll-up for one game, plus a bounded session tail.
// Merge replaces older records from the same (device,key) — totals are never
// double-counted because each device only ever counts its own sessions.
type Record struct {
	V        int       `json:"v"`
	Key      string    `json:"key"`
	Rom      string    `json:"rom"`
	Device   string    `json:"device"`
	Secs     int64     `json:"secs"`
	Plays    int64     `json:"plays"`
	LastUTC  int64     `json:"last_utc"`
	Sessions []Session `json:"sessions,omitempty"`
}

// Total is one game's merged roll-up (a totals.tsv row).
type Total struct {
	Key     string `json:"key"`
	Rom     string `json:"rom"`
	Secs    int64  `json:"secs"`
	Plays   int64  `json:"plays"`
	LastUTC int64  `json:"last_utc"`
}

// Dir returns the playtime store directory under the shared userdata tree.
// Honors SDCARD_PATH like the rest of the engine.
func Dir() string {
	sd := os.Getenv("SDCARD_PATH")
	if sd == "" {
		sd = "/mnt/SDCARD"
	}
	return filepath.Join(sd, ".userdata", "shared", ".lodor", "playtime")
}

func sessionsPath() string   { return filepath.Join(Dir(), "sessions.jsonl") }
func peersDir() string       { return filepath.Join(Dir(), "peers") }
func totalsJSONPath() string { return filepath.Join(Dir(), "totals.json") }

// TotalsTSVPath is the launcher-facing contract file.
func TotalsTSVPath() string { return filepath.Join(Dir(), "totals.tsv") }

// ─── stage (session start) ────────────────────────────────────────────────────

// WriteStage stages an in-flight session. wallNow/uptimeNow are injected for
// testability; production callers pass time.Now().Unix() and ReadUptime().
func WriteStage(rom string, wallNow int64, uptimeNow float64) error {
	data, err := json.Marshal(Stage{Rom: rom, WallStart: wallNow, UptimeStart: uptimeNow})
	if err != nil {
		return err
	}
	return os.WriteFile(StagePath, data, 0o644)
}

// TakeStage reads AND CONSUMES the staged session (idempotency: the exit hook
// can fire twice — trap + explicit call — and only the first can record).
// ok=false when no session is staged or the staged ROM isn't the one ending.
func TakeStage(rom string) (Stage, bool) {
	data, err := os.ReadFile(StagePath)
	if err != nil {
		return Stage{}, false
	}
	_ = os.Remove(StagePath)
	var st Stage
	if json.Unmarshal(data, &st) != nil {
		return Stage{}, false
	}
	if rom != "" && st.Rom != rom {
		return Stage{}, false // a different game's stale stage — not ours to bill
	}
	return st, true
}

// ReadUptime returns /proc/uptime's first field (seconds since boot,
// monotonic). ok=false on any parse problem — the caller records nothing
// rather than guessing.
func ReadUptime() (float64, bool) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	var up float64
	if _, err := fmt.Sscanf(fields[0], "%f", &up); err != nil {
		return 0, false
	}
	return up, true
}

// ComputeSession is the pure duration/start math (unit-tested with simulated
// clock jumps): duration = uptime delta, start_utc = wall_end − duration.
// ok=false for a non-positive or absurd delta (uptime went backwards = the
// stage predates a reboot; nothing trustworthy to record).
func ComputeSession(st Stage, uptimeNow float64, wallEnd int64) (startUTC, secs int64, ok bool) {
	delta := uptimeNow - st.UptimeStart
	if delta < 1 { // sub-second sessions are launcher bounce noise; <0 is a reboot
		return 0, 0, false
	}
	secs = int64(delta + 0.5)
	return wallEnd - secs, secs, true
}

// ─── local session log ────────────────────────────────────────────────────────

// AppendSession appends one finished session to sessions.jsonl (append-only;
// a torn tail line from a yank is skipped by the reader).
func AppendSession(s Session) error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(sessionsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(line, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// LocalSessions reads sessions.jsonl, skipping torn/garbage lines and
// DEDUPLICATING by session identity (key, start_utc, secs) — a re-appended
// line (crash between append and stage-consume, or a re-run hook) counts once.
func LocalSessions() []Session {
	f, err := os.Open(sessionsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Session
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var s Session
		if json.Unmarshal(sc.Bytes(), &s) != nil || s.Key == "" || s.Secs <= 0 {
			continue
		}
		id := fmt.Sprintf("%s\x00%d\x00%d", s.Key, s.StartUTC, s.Secs)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, s)
	}
	return out
}

// ─── peers (cross-device records) ─────────────────────────────────────────────

// peerFileName gives a (device,key)-stable, FAT32-safe filename.
func peerFileName(device, key string) string {
	san := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				b.WriteRune(r)
			} else {
				b.WriteRune('_')
			}
		}
		return b.String()
	}
	// key can collide post-sanitize in theory; length-prefix keeps it stable enough
	// for a cache whose worst failure is one extra fetch.
	return fmt.Sprintf("%s--%02d%s.json", san(device), len(key)%100, san(key))
}

// MergePeer stores a peer device's .lodortime record, keeping only the NEWEST
// record per (device,key) — the dedup that makes re-pulling the whole feed
// idempotent. Returns true when the store changed. selfDevice guards against
// merging our own pushed records back in (they're already in sessions.jsonl).
func MergePeer(rec Record, selfDevice string) (bool, error) {
	if rec.Key == "" || rec.Device == "" || rec.Plays <= 0 {
		return false, nil
	}
	if selfDevice != "" && strings.EqualFold(rec.Device, selfDevice) {
		return false, nil
	}
	if err := os.MkdirAll(peersDir(), 0o755); err != nil {
		return false, err
	}
	p := filepath.Join(peersDir(), peerFileName(rec.Device, rec.Key))
	if old, err := os.ReadFile(p); err == nil {
		var prev Record
		if json.Unmarshal(old, &prev) == nil && prev.LastUTC >= rec.LastUTC && prev.Plays >= rec.Plays {
			return false, nil // already have this (or a newer) record
		}
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(p, data); err != nil {
		return false, err
	}
	return true, nil
}

// peerRecords loads every stored peer record.
func peerRecords() []Record {
	entries, err := os.ReadDir(peersDir())
	if err != nil {
		return nil
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(peersDir(), e.Name()))
		if rerr != nil {
			continue
		}
		var r Record
		if json.Unmarshal(data, &r) == nil && r.Key != "" {
			out = append(out, r)
		}
	}
	return out
}

// ─── roll-up ──────────────────────────────────────────────────────────────────

// BuildRecord rolls up THIS device's local sessions for one key into the
// compact .lodortime payload (bounded session tail, newest last). Plays==0
// means "nothing local to push".
func BuildRecord(key, device string) Record {
	rec := Record{V: 1, Key: key, Device: device}
	var tail []Session
	for _, s := range LocalSessions() {
		if s.Key != key {
			continue
		}
		rec.Secs += s.Secs
		rec.Plays++
		if end := s.StartUTC + s.Secs; end > rec.LastUTC {
			rec.LastUTC = end
		}
		if rec.Rom == "" || s.StartUTC+s.Secs >= rec.LastUTC {
			rec.Rom = s.Rom
		}
		tail = append(tail, s)
	}
	if len(tail) > sessionsTailCap {
		tail = tail[len(tail)-sessionsTailCap:]
	}
	rec.Sessions = tail
	return rec
}

// MergedTotals folds local sessions + stored peer records into per-key totals,
// newest-first. Each device contributes only its own authoritative counts, so
// nothing can double-count.
func MergedTotals() []Total {
	agg := map[string]*Total{}
	bump := func(key, rom string, secs, plays, last int64) {
		t := agg[key]
		if t == nil {
			t = &Total{Key: key}
			agg[key] = t
		}
		t.Secs += secs
		t.Plays += plays
		if last >= t.LastUTC {
			t.LastUTC = last
			if rom != "" {
				t.Rom = rom
			}
		}
		if t.Rom == "" {
			t.Rom = rom
		}
	}
	for _, s := range LocalSessions() {
		bump(s.Key, s.Rom, s.Secs, 1, s.StartUTC+s.Secs)
	}
	for _, r := range peerRecords() {
		bump(r.Key, r.Rom, r.Secs, r.Plays, r.LastUTC)
	}
	out := make([]Total, 0, len(agg))
	for _, t := range agg {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastUTC != out[j].LastUTC {
			return out[i].LastUTC > out[j].LastUTC
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// WriteTotals rebuilds totals.json + totals.tsv from the merged view. The TSV
// is the frozen launcher contract: key \t rom_basename \t secs \t plays \t
// last_utc (unix seconds), newest-first, no header.
func WriteTotals() error {
	totals := MergedTotals()
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	jd, err := json.Marshal(struct {
		V      int     `json:"v"`
		Totals []Total `json:"totals"`
	}{1, totals})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(totalsJSONPath(), jd); err != nil {
		return err
	}
	var b strings.Builder
	for _, t := range totals {
		fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%d\n", t.Key, t.Rom, t.Secs, t.Plays, t.LastUTC)
	}
	return writeFileAtomic(TotalsTSVPath(), []byte(b.String()))
}

// writeFileAtomic is the FAT32-safe whole-file write: temp + fsync + rename
// (feedback_lodor_fat32_atomic_writes — a yank must never zero a good file).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
