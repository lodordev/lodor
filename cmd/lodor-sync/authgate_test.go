package main

import (
	"testing"

	"lodor/sync"
)

// The four state run* wrappers gate on pairingExpired (set via noteStateAuth)
// exactly as the battery-save modes gate on it via notePushResults/notePullResult
// — and exitMode reads that same var to print the frozen PAIRING_EXPIRED line and
// exit 6. This locks BUG 4's wiring: a state result carrying AuthExpired flips the
// SAME accumulator the save path uses, so it rides the identical contract.
func TestNoteStateAuthFlipsSameGateAsSavePath(t *testing.T) {
	reset := func() { pairingExpired = false }

	reset()
	noteStateAuth(sync.PushStatesResult{AuthExpired: true}.AuthExpired)
	if !pairingExpired {
		t.Fatal("state AuthExpired did not flip pairingExpired (BUG 4)")
	}

	reset()
	noteStateAuth(sync.ListStatesResult{AuthExpired: true}.AuthExpired)
	if !pairingExpired {
		t.Fatal("list AuthExpired did not flip pairingExpired")
	}

	reset()
	noteStateAuth(sync.PullStateResult{AuthExpired: true}.AuthExpired)
	if !pairingExpired {
		t.Fatal("pull AuthExpired did not flip pairingExpired")
	}

	// A clean state result must NOT flip it (no false PAIRING_EXPIRED).
	reset()
	noteStateAuth(sync.PushStatesResult{AuthExpired: false}.AuthExpired)
	if pairingExpired {
		t.Fatal("clean state result wrongly flagged pairing expired")
	}

	// Parity: the battery-SAVE path flips the very same var — same contract.
	reset()
	notePushResults([]sync.PushResult{{AuthExpired: true}})
	saveFlipped := pairingExpired
	reset()
	noteStateAuth(true)
	stateFlipped := pairingExpired
	if saveFlipped != stateFlipped || !stateFlipped {
		t.Fatalf("state path (%v) does not share the save path gate (%v)", stateFlipped, saveFlipped)
	}
	reset()
}
