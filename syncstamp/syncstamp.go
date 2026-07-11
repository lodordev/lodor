// Package syncstamp is the engine's "last successful sync" record (lodor#43): one
// tiny file, <pak dir>/last-sync.txt, stamped by every sync mode that VERIFIABLY
// moved or confirmed data against the server, and read by every lane's menu/home
// surface to answer "did my saves make it up?" without running a sync.
//
// Exactly one line, machine-first, additive-friendly:
//
//	last_sync_ok=<unix epoch> saves=<n> states=<m>
//
// where saves/states count the save/state files the STAMPING run moved (pushed +
// pulled). A run that moved nothing but positively verified everything in-sync
// stamps 0s — the epoch is the signal the surfaces render ("Last synced: 2h ago").
// Last writer wins; the counts describe the most recent stamping run only.
//
// HONESTY CONTRACT (feedback_no_fake_ui_state): callers must stamp ONLY after a
// verified server interaction — never on a mode that merely exited 0 while offline
// (several push modes exit 0 with their queue intact). A stamp is a claim the
// server was reached; the per-mode conditions live at the call sites.
//
// The writer is FAT32-atomic (fsutil temp+fsync+rename+dir-fsync) — a torn stamp
// on a power-yank would otherwise render as a garbage age forever. This file is
// engine-OWNED: lanes only read it, keeping the "engine never writes settings.conf"
// single-writer discipline intact (the stamp deliberately does NOT live in
// settings.conf, whose lane writers do read-modify-write of the whole file, and
// which LodorOS dot-sources as shell).
package syncstamp

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lodor/fsutil"
)

// FileName is the stamp's file name inside the pak/data dir. Every lane reads the
// same name: the Go wizard (muOS/Knulli) from its data dir, NextUI's launch.sh and
// the LodorOS data layer from the pak dir, Android's MainActivity from pakDir.
const FileName = "last-sync.txt"

// Stamp is one parsed record.
type Stamp struct {
	Epoch  int64 // unix seconds of the verified sync success
	Saves  int   // save files moved by that run (pushed + pulled)
	States int   // state files moved by that run (pushed + pulled)
}

// Path returns the stamp path for a pak/data dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Write stamps a verified sync success at time.Now().
func Write(dir string, saves, states int) error {
	return WriteAt(dir, time.Now().Unix(), saves, states)
}

// WriteAt is Write with an explicit epoch (tests; catch-up writers).
func WriteAt(dir string, epoch int64, saves, states int) error {
	line := "last_sync_ok=" + strconv.FormatInt(epoch, 10) +
		" saves=" + strconv.Itoa(saves) +
		" states=" + strconv.Itoa(states) + "\n"
	return fsutil.WriteFileAtomicString(Path(dir), line, 0o644)
}

// Read parses the stamp. ok=false when the file is missing, unreadable, or does
// not carry a positive last_sync_ok epoch — surfaces then say nothing rather than
// guessing (never a fake age). Unknown extra tokens are ignored (additive room).
func Read(dir string) (Stamp, bool) {
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		return Stamp{}, false
	}
	var st Stamp
	for _, f := range strings.Fields(strings.SplitN(string(b), "\n", 2)[0]) {
		k, v, found := strings.Cut(f, "=")
		if !found {
			continue
		}
		switch k {
		case "last_sync_ok":
			st.Epoch, _ = strconv.ParseInt(v, 10, 64)
		case "saves":
			st.Saves, _ = strconv.Atoi(v)
		case "states":
			st.States, _ = strconv.Atoi(v)
		}
	}
	if st.Epoch <= 0 {
		return Stamp{}, false
	}
	return st, true
}

// Age renders the stamp's age at now as the short relative form every lane's
// surface shares: "just now" / "Nm ago" / "Nh ago" / "Nd ago". A stamp from the
// future (clock skew on RTC-less handhelds) reads as "just now" — never negative.
func (s Stamp) Age(now int64) string {
	secs := now - s.Epoch
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < 90:
		return "just now"
	case secs < 3600:
		return strconv.FormatInt(secs/60, 10) + "m ago"
	case secs < 86400:
		return strconv.FormatInt(secs/3600, 10) + "h ago"
	default:
		return strconv.FormatInt(secs/86400, 10) + "d ago"
	}
}
