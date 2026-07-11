package main

// last-sync stamping (lodor#43). Every mode that VERIFIABLY moved or confirmed
// save/state data against the server records the moment at <pakDir>/last-sync.txt
// via the syncstamp package, so every lane's menu/home surface can answer "did my
// saves make it up?" with a cheap local read ("Last synced: 2h ago").
//
// The honesty conditions live at the call sites, one per mode, because "exit 0"
// does NOT mean "server reached" here: the offline-first push modes exit 0 with
// their queue intact. The shared rule: stamp on real verified transfers (pushed
// or pulled files), or on a run that positively verified everything in-sync
// against the server; never on an empty queue, an all-stuck (offline) drain, or
// a local-only sweep. Best-effort by contract — a failed stamp write warns on
// stderr and never changes a mode's RESULT line or exit code.

import (
	"fmt"
	"os"

	"lodor/sync"
	"lodor/syncstamp"
)

// stampSync records a verified sync success (saves/states = files moved by THIS
// run). Best-effort: a write failure must never turn a real sync into a reported
// failure (the writeLastSynced precedent).
func stampSync(saves, states int) {
	if err := syncstamp.Write(pakDir(), saves, states); err != nil {
		fmt.Fprintf(os.Stderr, "syncstamp: WARN couldn't write last-sync stamp: %s\n", safeErr(err))
	}
}

// pullSawServer reports whether a pull result PROVES the server answered — every
// outcome that requires a server save listing (or a served download) to be
// reached. PullError (offline/transport) and PullResolveFail (may be offline
// before any listing) prove nothing and return false, as does PullSnapshotFail
// (local staging failed; the run's verdict is a failure regardless).
func pullSawServer(r sync.PullResult) bool {
	switch r.Outcome {
	case sync.PullWritten, sync.PullInSync, sync.PullLocalUnpushed, sync.PullTombstoned, sync.PullNoServerSave:
		return true
	}
	return false
}
