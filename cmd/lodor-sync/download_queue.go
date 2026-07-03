package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lodor/config"
	"lodor/fsutil"
	"lodor/platform"
	"lodor/romm"
)

// download-queue.txt is the offline-first DOWNLOAD queue the launcher appends to when
// the user picks "Add to queue" on a not-yet-downloaded (0-byte stub) game. Each line
// is one SDCARD-relative ROM path (the launcher strips SDCARD_PATH + leading slash when
// it enqueues), e.g. "Roms/Game Boy Advance (GBA)/[^] Game (USA).gba". It lives beside
// pending-saves.txt inside the host pak dir, and is mutated under the SAME .queue.lock
// the pending-saves path uses, so an append never races a rewrite into corruption.
func downloadQueuePath() string {
	return filepath.Join(pakDir(), "download-queue.txt")
}

// dlQueueRead returns the queued ROM paths in file order, skipping blank lines. A
// missing file is an empty queue (not an error) — mirrors pendingRead.
func dlQueueRead() []string {
	data, err := os.ReadFile(downloadQueuePath())
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

// dlQueueWrite atomically replaces download-queue.txt with the given paths (tmp file +
// rename), one per line, trailing newline; an empty slice truncates it to zero bytes so
// the launcher's "Download Queue (N)" count reads zero. Mirrors pendingWrite.
func dlQueueWrite(paths []string) error {
	p := downloadQueuePath()
	var body string
	if len(paths) > 0 {
		body = strings.Join(paths, "\n") + "\n"
	}
	// FAT32-atomic: temp + fsync + rename + dir fsync (fsutil).
	return fsutil.WriteFileAtomicString(p, body, 0o644)
}

// queueAbsPath turns a stored (SDCARD-relative) queue line into the absolute on-card
// path downloadRomCore writes to. Absolute lines (legacy/manual entries) pass through.
func queueAbsPath(line string) string {
	if filepath.IsAbs(line) {
		return line
	}
	sd := os.Getenv("SDCARD_PATH")
	if sd == "" {
		sd = "/mnt/SDCARD"
	}
	return filepath.Join(sd, line)
}

// runDownloadQueue downloads every ROM listed in download-queue.txt over Wi-Fi, reusing
// the SAME resolve→fetch→hash-verify path as --download (downloadRomCore), and rewrites
// the queue to keep only the entries that did NOT land (failures stay for retry; landed
// entries are dropped). Live per-file progress flows through the existing /tmp/dl-progress
// + /tmp/romm-phase side-channels the launcher already renders.
//
// Contract: RESULT downloaded=<N> failed=<M> remaining=<K> (K == M; the queue's new
// length). An empty queue prints downloaded=0 failed=0 remaining=0. BLUEPRINT §3/§8.
//
// Concurrency: the queue is READ under .queue.lock, downloaded WITHOUT the lock held (a
// batch can take minutes — never block the minarch shim's pending-saves append that long),
// then the survivors are written by recomputing "current file minus the entries that
// landed" under the lock, so an append that arrived mid-batch is preserved.
func runDownloadQueue(client *romm.Client, cfg *config.Config) {
	release := acquireQueueLock()
	queue := dlQueueRead()
	release()

	total := len(queue)
	if total == 0 {
		writeProgress(0)
		fmt.Println("RESULT downloaded=0 failed=0 remaining=0")
		os.Exit(0)
	}

	landed := make(map[string]bool, total)
	downloaded := 0
	failed := 0
	for i, line := range queue {
		name := strings.TrimSuffix(platform.StripLeadingMarker(filepath.Base(line)), filepath.Ext(line))
		writePhase(fmt.Sprintf("Downloading %d/%d — %s…", i+1, total, name))
		if downloadRomCore(client, cfg, queueAbsPath(line)) {
			landed[line] = true
			downloaded++
		} else {
			failed++
		}
	}

	// Rewrite the queue: recompute from the CURRENT file (an append may have arrived
	// during the batch) minus the lines that landed this run. Under the lock.
	release = acquireQueueLock()
	cur := dlQueueRead()
	var survivors []string
	for _, line := range cur {
		if landed[line] {
			continue
		}
		survivors = append(survivors, line)
	}
	if wErr := dlQueueWrite(survivors); wErr != nil {
		fmt.Fprintf(os.Stderr, "QUEUEWARN rewrite: %s\n", safeErr(wErr))
	}
	remaining := len(survivors)
	release()

	writeProgress(100)
	fmt.Printf("RESULT downloaded=%d failed=%d remaining=%d\n", downloaded, failed, remaining)
	if failed > 0 {
		exitMode(4) // ran but one or more items errored (matches the documented exit map)
	}
	exitMode(0)
}
