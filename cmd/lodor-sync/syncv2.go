package main

// Sync V2 cmd glue (Argosy research #1/#2/#3): the pending-upload signal the per-game
// anchor reconciler needs, and the experimental whole-library negotiated-sync mode.

import (
	"fmt"
	"os"

	"lodor/config"
	"lodor/romm"
	"lodor/sync"
)

// isPendingUpload reports whether romPath is recorded in pending-saves.txt (a local
// change we have not yet pushed). The anchor reconciler treats a pending rom as
// KEEP_LOCAL — by our own record the local bytes are the ones to preserve.
func isPendingUpload(romPath string) bool {
	for _, line := range pendingRead() {
		rp, _, _, _ := parseQueueLine(line)
		if rp == romPath {
			return true
		}
	}
	return false
}

// runNegotiateSync runs the experimental SYNC V2 whole-library negotiated sync. It
// reads the server version via /api/heartbeat, derives capabilities, and:
//   - on a server that SupportsSyncNegotiate (>= 4.9.0): asks the server for a sync
//     plan, executes it (push/pull/conflict/noop), completes the session;
//   - otherwise: does nothing and reports negotiated=0 — the per-game anchor reconciler
//     (--sync-save) is the path on older servers.
//
// Contract: RESULT negotiated=<0|1> pulled=<N> pushed=<N> conflicts=<N> errors=<N>.
// Host-free per-op diagnostics go to STDERR only. EXPERIMENTAL: the negotiate wire
// shape is unverified against a live >= 4.9.0 server — see the buildout report.
func runNegotiateSync(client *romm.Client, cfg *config.Config) {
	writeProgress(0)
	writePhase("Checking server…")

	hb, err := client.GetHeartbeat()
	if err != nil {
		fmt.Fprintf(os.Stderr, "negotiate-sync: heartbeat failed: %s\n", safeErr(err))
		fmt.Println("RESULT negotiated=0 pulled=0 pushed=0 conflicts=0 errors=0")
		os.Exit(0)
	}
	strat := sync.SelectStrategy(hb.Capabilities)
	fmt.Fprintf(os.Stderr, "negotiate-sync: server=%s strategy=%s\n", hb.Version, strat)

	if strat != sync.StrategyNegotiate {
		fmt.Fprintln(os.Stderr, "negotiate-sync: server does not support negotiate; use per-game --sync-save")
		fmt.Println("RESULT negotiated=0 pulled=0 pushed=0 conflicts=0 errors=0")
		os.Exit(0)
	}

	writePhase("Negotiating with server…")
	sum, err := sync.NegotiateLibrarySync(client, cfg, cfg.ActiveHost().DeviceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "negotiate-sync: negotiate failed: %s\n", safeErr(err))
		fmt.Println("RESULT negotiated=0 pulled=0 pushed=0 conflicts=0 errors=0")
		os.Exit(0)
	}
	writeProgress(100)

	for _, op := range sum.Ops {
		fmt.Fprintf(os.Stderr, "negotiate-sync: rom=%d op=%s ok=%t %s\n", op.RomID, op.Op, op.OK, op.Reason)
	}
	fmt.Printf("RESULT negotiated=1 pulled=%d pushed=%d conflicts=%d errors=%d\n",
		sum.Pulled, sum.Pushed, sum.Conflicts, sum.Errors)
	os.Exit(0)
}
