package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lodor/fsutil"
	"lodor/platform"
)

// stagingDir holds pre-flashback save snapshots until --push-pending uploads them to the
// timeline. Under the pak dir so it travels with the queue (offline-first Flashback).
func stagingDir() string { return filepath.Join(pakDir(), ".flashback-staging") }

// stageSaveFile copies srcSavePath to a unique file under stagingDir and returns its
// absolute path, capturing the CURRENT save bytes before a flashback overwrites the live
// file so they can still be uploaded later. Empty path + error on failure.
func stageSaveFile(srcSavePath string) (string, error) {
	if err := os.MkdirAll(stagingDir(), 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(srcSavePath)
	if err != nil {
		return "", err
	}
	base := filepath.Base(srcSavePath)
	var lastErr error
	for i := 0; i < 10000; i++ {
		cand := filepath.Join(stagingDir(), fmt.Sprintf("%s.%d.staged", base, i))
		f, err := os.OpenFile(cand, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil { // name taken — try the next index (no time/rand needed for uniqueness)
			lastErr = err
			continue
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil {
			_ = os.Remove(cand)
			return "", werr
		}
		if cerr != nil {
			return "", cerr
		}
		return cand, nil
	}
	return "", lastErr
}

// enqueueStaged appends a staged-save entry to pending-saves.txt under the queue lock.
// Format: "<rompath>\t<stagedAbsPath>\t<emulator>" — three TAB fields mark a staged entry
// (vs the bare rompath line the minarch shim writes); the banner still counts it as one
// pending save, and --push-pending uploads the staged FILE (not the now-overwritten live
// save) then deletes it.
func enqueueStaged(romPath, stagedPath, emulator string) error {
	release := acquireQueueLock()
	defer release()
	cur := pendingRead()
	cur = append(cur, romPath+"\t"+stagedPath+"\t"+emulator)
	return pendingWrite(cur)
}

// enqueueBare appends a BARE ROM-path entry to pending-saves.txt under the queue lock,
// deduplicated (never adds a line that already exists). This is the offline-first
// fallback the HYBRID post-game push uses when it could not push AND could not stage a
// snapshot (e.g. the server was unreachable so the ROM's save files couldn't be
// resolved): the bare line guarantees the changed save is still recorded for later, and
// --push-pending resolves+uploads the live save when WiFi returns (parseQueueLine treats
// a one-field line as a bare ROM path).
func enqueueBare(romPath string) error {
	release := acquireQueueLock()
	defer release()
	cur := pendingRead()
	for _, line := range cur {
		if line == romPath {
			return nil // already queued — dedup
		}
	}
	cur = append(cur, romPath)
	return pendingWrite(cur)
}

// parseQueueLine classifies a pending-saves.txt line. A bare line is a ROM path (push the
// current live save); a TAB-split line is a staged pre-flashback snapshot (push the named
// staged file for that ROM). emu is "" when absent.
func parseQueueLine(line string) (romPath, stagedPath, emu string, staged bool) {
	parts := strings.Split(line, "\t")
	if len(parts) < 2 {
		return line, "", "", false
	}
	if len(parts) >= 3 {
		emu = parts[2]
	}
	return parts[0], parts[1], emu, true
}

// pending-saves.txt is the offline-first upload queue the minarch shim appends to
// after a game whose save changed (BLUEPRINT §8): one ABSOLUTE on-card ROM path per
// line, deduplicated. It lives inside the host pak's working directory:
//
//	<LODOR_PAK_DIR>/pending-saves.txt
//
// where LODOR_PAK_DIR is the absolute pak path the launch scripts export (falling back
// to the script CWD) — the SAME dir catalog.IndexPath resolves via platform.PakDir(), so
// every component agrees on the path without the engine knowing the host's pak name.

// pakDir returns the host pak's working directory (platform.PakDir(), resolved from
// LODOR_PAK_DIR / the script CWD). The engine owns no host pak name.
func pakDir() string {
	return platform.PakDir()
}

// pendingPath returns the absolute path of pending-saves.txt.
func pendingPath() string {
	return filepath.Join(pakDir(), "pending-saves.txt")
}

// lockPath returns the advisory lock directory used to serialize queue mutations.
func lockPath() string {
	return filepath.Join(pakDir(), ".queue.lock")
}

// acquireQueueLock takes the queue lock by creating a .queue.lock DIRECTORY: mkdir
// is atomic (fails if it already exists), so two concurrent processes can't both own
// it. Returns a release func. The minarch shim appends to pending-saves.txt while
// --push-pending rewrites it; the lock keeps an append from racing a rewrite. We try
// a few times in case a previous run died holding the lock, but never block forever —
// a stuck lock must not wedge the user's sync. On giving up we proceed anyway (the
// append path uses grep -qxF + >> which is line-atomic enough that a lost race only
// risks a duplicate entry, never corruption).
func acquireQueueLock() (release func()) {
	noop := func() {}
	for i := 0; i < 50; i++ {
		if err := os.Mkdir(lockPath(), 0o755); err == nil {
			return func() { _ = os.Remove(lockPath()) }
		}
		// Couldn't take it — someone else holds it (or a dead run left it). Don't
		// sleep on the handheld's slow FS in a tight loop; a few quick retries then
		// proceed unlocked rather than wedge.
		if i >= 5 {
			break
		}
	}
	return noop
}

// pendingRead returns the queued ROM paths in file order, skipping blank lines. A
// missing file is an empty queue (not an error).
func pendingRead() []string {
	data, err := os.ReadFile(pendingPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// pendingWrite atomically replaces pending-saves.txt with the given paths (tmp file
// + rename), one per line, trailing newline. An empty slice truncates the file to a
// single trailing newline's worth of nothing (zero bytes) so the launcher's
// "X Saves Pending" banner reads zero.
func pendingWrite(paths []string) error {
	p := pendingPath()
	var body string
	if len(paths) > 0 {
		body = strings.Join(paths, "\n") + "\n"
	}
	// FAT32-atomic: temp + fsync + rename + dir fsync (fsutil).
	return fsutil.WriteFileAtomicString(p, body, 0o644)
}
