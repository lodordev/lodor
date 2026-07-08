package main

// Handoff v1 cmd modes (design §6.3). RESULT tokens are NEW lines no existing
// shell parses (verified by grep at build time — the H1/H2 lesson): consumers
// added in the same commits as their parsers.

import (
	"fmt"

	"lodor/config"
	"lodor/romm"
	"lodor/sync"
)

// runPushStates uploads the ROM's local save states. Contract:
//
//	RESULT pushedstates=<N> skippedstates=<N> failedstates=<N> retiredstates=<N> queuedstate=<0|1> reason=<token>
//
// reason: ok | no-manifest (statecores.json absent/unusable — feature dark) |
// no-system (rom's platform not in the manifest / unsupported host) |
// no-states (nothing local to push) | resolve | offline. retiredstates counts
// this engine's OWN old uploads deleted by retention after a landed push (6.4).
// An offline attempt auto-queues the rom into pending-states.txt
// (queuedstate=1) so --push-pending-states retries it later. Exit 0 in all
// cases: state push is additive best-effort and must never fail a hook chain.
func runPushStates(client *romm.Client, cfg *config.Config, romPath string) {
	res := sync.PushStates(client, cfg, romPath)
	queued := 0
	if res.Reason == "offline" {
		if _, err := enqueuePendingState(romPath); err == nil {
			queued = 1
		}
	}
	fmt.Printf("RESULT pushedstates=%d skippedstates=%d failedstates=%d retiredstates=%d queuedstate=%d reason=%s\n",
		res.Pushed, res.Skipped, res.Failed, res.Retired, queued, res.Reason)
	exitMode(0)
}

// runListStates prints one machine line per server state plus a summary:
//
//	LISTSTATE id=<N> slot=<s> compat=<0|1> known=<0|1> age=<sec> size=<B> origin=<tuple> why=<->|<reason> name=<file>
//	RESULT liststates=<N> compatstates=<N> reason=<token>
//
// known=1: this device's ledger carries the record's id (originated here or
// already pulled here) — the launch card treats compat=1 known=0 as news.
func runListStates(client *romm.Client, cfg *config.Config, romPath string) {
	res := sync.ListStates(client, cfg, romPath)
	compat := 0
	for _, o := range res.Offers {
		why := o.Why
		if why == "" {
			why = "-"
		}
		if o.Compatible {
			compat++
		}
		fmt.Printf("LISTSTATE id=%d slot=%s compat=%d known=%d age=%d size=%d origin=%s why=%q name=%q\n",
			o.ID, o.Slot, b2i(o.Compatible), b2i(o.Known), o.AgeSeconds, o.Size, o.Origin, why, o.FileName)
	}
	fmt.Printf("RESULT liststates=%d compatstates=%d reason=%s\n", len(res.Offers), compat, res.Reason)
	exitMode(0)
}

// runPullState places one server state at the local slot. Contract:
//
//	RESULT placedstate=<0|1> reason=<token> path=<local path when placed>
func runPullState(client *romm.Client, cfg *config.Config, romPath string, stateID int, slot string) {
	if stateID == 0 {
		fmt.Println("RESULT placedstate=0 reason=bad-args")
		exitMode(2)
	}
	res := sync.PullState(client, cfg, romPath, stateID, slot)
	if res.Placed {
		fmt.Printf("RESULT placedstate=1 reason=ok path=%q\n", res.Path)
	} else {
		fmt.Printf("RESULT placedstate=0 reason=%s\n", res.Reason)
	}
	exitMode(0)
}
