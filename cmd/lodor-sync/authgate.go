package main

// PAIRING_EXPIRED contract (task #123 deliverable 2). When the server rejects
// our client-token (401, or 403-blaming-the-token — romm.AuthError), the run's
// outcome is a DISTINCT, stable engine result so the launcher/pak can say
// "Pairing expired — run Setup / Re-pair" instead of a generic "sync failed":
//
//   - RESULT-printing modes: the mode's normal RESULT failure line is printed
//     UNCHANGED (existing parsers keep working), then ONE extra final stdout
//     line `PAIRING_EXPIRED`, and the process exits 6.
//   - Data-list modes (--list-saves --sync-feed --recent --list-users
//     --list-profiles), whose stdout is a byte-parsed list: NO extra stdout
//     line (it would corrupt the list), exit code 6 alone carries the signal.
//   - --validate additionally appends pairing_expired=<0|1> to its RESULT line.
//
// Exit 6 was chosen because 0/2/3/4 are the documented map and 5 is already
// used by the profile modes' write-failure path.
//
// HARD RULE: config.json is NEVER deleted or modified on an auth failure — a
// transient server misconfig must not wipe a valid pairing. This gate only
// REPORTS.

import (
	"fmt"
	"os"

	"lodor/romm"
	"lodor/sync"
)

// pairingExpiredLine is the machine-readable stdout token; pairingExpiredExit
// the matching exit code. Both are frozen contract for the pak/launcher wiring.
const (
	pairingExpiredLine = "PAIRING_EXPIRED"
	pairingExpiredExit = 6
)

// pairingExpired accumulates whether ANY API call this run failed with an
// invalid/expired/revoked token. The CLI is single-threaded per process, so a
// package var is safe.
var pairingExpired bool

// noteAuthErr flags the run when err is a romm.AuthError (nil-safe).
func noteAuthErr(err error) {
	if err != nil && romm.IsAuthError(err) {
		pairingExpired = true
	}
}

// notePushResults flags the run when any push result carried AuthExpired.
func notePushResults(results []sync.PushResult) {
	for _, r := range results {
		if r.AuthExpired {
			pairingExpired = true
			return
		}
	}
}

// notePullResult flags the run when a pull result carried AuthExpired.
func notePullResult(r sync.PullResult) {
	if r.AuthExpired {
		pairingExpired = true
	}
}

// exitMode ends a RESULT-printing mode: when a pairing expiry was noted it
// prints the PAIRING_EXPIRED line (AFTER the mode's own RESULT line, which the
// caller has already printed) and exits 6; otherwise it exits code unchanged.
func exitMode(code int) {
	if pairingExpired {
		fmt.Println(pairingExpiredLine)
		os.Exit(pairingExpiredExit)
	}
	os.Exit(code)
}

// exitModeQuiet ends a DATA-LIST mode (stdout is a byte-parsed list): exit 6
// with NO extra stdout when a pairing expiry was noted, else exit code.
func exitModeQuiet(code int) {
	if pairingExpired {
		os.Exit(pairingExpiredExit)
	}
	os.Exit(code)
}
