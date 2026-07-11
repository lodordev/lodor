package main

// pending-states.txt — the offline queue for save-STATE pushes (Handoff v1).
// States are immutable files that stay on the card and the engine's ledger
// dedups uploads, so unlike pending-saves this queue needs no staging and no
// per-file entries: ONE bare absolute ROM path per line, deduplicated, meaning
// "this rom may have unpushed states — re-run --push-states when online".
//
// Enqueue paths:
//   - --queue-state <rom>: the hooks' offline branch (no network, instant —
//     their reachability gate already said the server is away).
//   - --push-states auto-queues when its own attempt lands reason=offline
//     (covers lanes that call the engine unconditionally, e.g. NextUI's
//     post-launch hook behind the Wi-Fi funnel).
//
// Drain (--push-pending-states) re-runs the push per queued rom; a rom stays
// queued ONLY while still offline. Every other reason drops the line: ok /
// no-states / no-manifest / no-system leave nothing to retry, and resolve
// (unmatchable rom) would wedge the queue forever — dropped LOUDLY per line.

import (
	"fmt"
	"path/filepath"

	"lodor/config"
	"lodor/romm"
	"lodor/sync"
)

func pendingStatesPath() string {
	return filepath.Join(pakDir(), "pending-states.txt")
}

// enqueuePendingState adds one bare ROM path, deduplicated, under the queue
// lock (shared with pending-saves — same rare, short critical section).
// added=false with nil err means it was already queued — still a success.
func enqueuePendingState(romPath string) (added bool, err error) {
	release := acquireQueueLock()
	defer release()
	cur := pendingReadFile(pendingStatesPath())
	for _, line := range cur {
		if line == romPath {
			return false, nil
		}
	}
	cur = append(cur, romPath)
	return true, pendingWriteFile(pendingStatesPath(), cur)
}

// runQueueState records "this rom may have unpushed states" without touching
// the network. Contract: RESULT queuedstate=<0|1> reason=<ok|write>
// (queuedstate=1 covers already-queued too — the rom IS in the queue).
func runQueueState(romPath string) {
	if _, err := enqueuePendingState(romPath); err != nil {
		fmt.Println("RESULT queuedstate=0 reason=write")
		exitMode(1)
	}
	fmt.Println("RESULT queuedstate=1 reason=ok")
	exitMode(0)
}

// drainPendingStates re-runs the state push for every queued rom, returning
// the processed lines and which of them stay queued (still offline). The queue
// file is rewritten under the lock, preserving any lines enqueued mid-drain.
// cancelled (lodor#42): the B-press sentinel stopped the drain between entries —
// every not-yet-attempted entry stays queued (never silently dropped).
func drainPendingStates(client *romm.Client, cfg *config.Config,
	report func(romPath string, res sync.PushStatesResult, kept bool)) (total, drained int, stuck []string, cancelled bool) {
	queued := pendingReadFile(pendingStatesPath())
	if len(queued) == 0 {
		return 0, 0, nil, false
	}
	for i, romPath := range queued {
		// REAL CANCEL, checked BETWEEN entries: what drained is drained; the rest
		// rides the rewritten queue to the next drain. Never mid-upload.
		if client != nil && client.CancelCheck != nil && client.CancelCheck() {
			cancelled = true
			stuck = append(stuck, queued[i:]...) // kept in the queue rewrite below
			break
		}
		res := sync.PushStates(client, cfg, romPath)
		kept := res.Reason == "offline"
		if kept {
			stuck = append(stuck, romPath)
		} else {
			drained++
		}
		if report != nil {
			report(romPath, res, kept)
		}
	}
	release := acquireQueueLock()
	processed := map[string]bool{}
	for _, l := range queued {
		processed[l] = true
	}
	final := append([]string{}, stuck...)
	inFinal := map[string]bool{}
	for _, l := range stuck {
		inFinal[l] = true
	}
	for _, l := range pendingReadFile(pendingStatesPath()) {
		if !processed[l] && !inFinal[l] {
			final = append(final, l) // enqueued while we were draining — keep
		}
	}
	// A rewrite failure is non-fatal: worst case the whole queue retries next
	// drain and the ledger dedups every already-landed upload to a skip.
	_ = pendingWriteFile(pendingStatesPath(), final)
	release()
	return len(queued), drained, stuck, cancelled
}

// runPushPendingStates drains the states queue. Contract:
//
//	PENDINGSTATE rom=<path> pushedstates=<N> skippedstates=<N> failedstates=<N> retiredstates=<N> reason=<tok> kept=<0|1>
//	RESULT pendingstates=<total> drained=<N> stuck=<K>
//
// Exit 0 always — draining is best-effort background work.
func runPushPendingStates(client *romm.Client, cfg *config.Config) {
	pushedN := 0 // #43: real landed state uploads across the drain
	total, drained, stuck, cancelled := drainPendingStates(client, cfg,
		func(romPath string, res sync.PushStatesResult, kept bool) {
			pushedN += res.Pushed
			fmt.Printf("PENDINGSTATE rom=%q pushedstates=%d skippedstates=%d failedstates=%d retiredstates=%d reason=%s kept=%d\n",
				romPath, res.Pushed, res.Skipped, res.Failed, res.Retired, res.Reason, b2i(kept))
		})
	// #43 (states leg): stamp only on REAL landed uploads — a drain that merely
	// retired local-only entries (no-states/resolve) or stayed offline never
	// stamps, nor does a CANCELLED drain (states deliberately left behind).
	if pushedN > 0 && !cancelled {
		stampSync(0, pushedN)
	}
	// cancelled=1 is ADDITIVE (lodor#42) — parsers key on pendingstates=/drained=.
	cancelSuffix := ""
	if cancelled {
		cancelSuffix = " cancelled=1"
	}
	fmt.Printf("RESULT pendingstates=%d drained=%d stuck=%d%s\n", total, drained, len(stuck), cancelSuffix)
	exitMode(0)
}
