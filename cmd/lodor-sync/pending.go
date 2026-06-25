package main

import (
	"os"
	"path/filepath"
	"strings"
)

// pending-saves.txt is the offline-first upload queue the minarch shim appends to
// after a game whose save changed (BLUEPRINT §8): one ABSOLUTE on-card ROM path per
// line, deduplicated. It lives at:
//
//	<SDCARD>/Tools/<PLAT>/RomM Sync.pak/pending-saves.txt
//
// where SDCARD comes from SDCARD_PATH (default /mnt/SDCARD) and PLAT from PLATFORM
// (default miyoomini) — the SAME env the launcher scripts and catalog.IndexPath use,
// so every component agrees on the path.

func sdcardPath() string {
	if sd := os.Getenv("SDCARD_PATH"); sd != "" {
		return sd
	}
	return "/mnt/SDCARD"
}

func platformName() string {
	if p := os.Getenv("PLATFORM"); p != "" {
		return p
	}
	return "miyoomini"
}

// pakDir returns <SDCARD>/Tools/<PLAT>/RomM Sync.pak — the pak's working directory.
func pakDir() string {
	return filepath.Join(sdcardPath(), "Tools", platformName(), "RomM Sync.pak")
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
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	var body string
	if len(paths) > 0 {
		body = strings.Join(paths, "\n") + "\n"
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
