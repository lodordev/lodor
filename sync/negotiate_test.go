package sync

// Unit tests for the network-free halves of the managed-sync executor: op bucketing
// (bucketOps) and the per-ROM push verdict reduction (applyPush). The full
// ExecuteNegotiatedPlan / ReconcileLibrary round-trip is exercised against a live/mock
// RomM + card fixture at the e2e layer; these lock the decision logic.

import (
	"testing"

	"lodor/romm"
)

func TestBucketOpsGroupsAndCounts(t *testing.T) {
	ops := []romm.SyncOperation{
		{Action: "upload", RomID: 7},
		{Action: "upload", RomID: 7}, // coexist twin — same ROM, second upload op
		{Action: "download", RomID: 9},
		{Action: "conflict", RomID: 9},
		{Action: "no_op", RomID: 11},     // excluded from planned
		{Action: "resurrect", RomID: 13}, // unknown future action — ignored
	}
	order, byRom, planned := bucketOps(ops)

	if planned != 4 { // 2 upload + 1 download + 1 conflict; no_op & unknown excluded
		t.Errorf("planned = %d want 4", planned)
	}
	if len(order) != 2 || order[0] != 7 || order[1] != 9 {
		t.Fatalf("order = %v want [7 9] (first-sighting)", order)
	}
	if byRom[7].uploads != 2 || byRom[7].downloads != 0 || byRom[7].conflicts != 0 {
		t.Errorf("rom 7 = %+v want uploads:2", *byRom[7])
	}
	if byRom[9].downloads != 1 || byRom[9].conflicts != 1 {
		t.Errorf("rom 9 = %+v want downloads:1 conflicts:1", *byRom[9])
	}
	if _, ok := byRom[11]; ok {
		t.Errorf("no_op ROM 11 should not be bucketed")
	}
	if _, ok := byRom[13]; ok {
		t.Errorf("unknown-action ROM 13 should not be bucketed")
	}
}

func TestApplyPushVerdict(t *testing.T) {
	tests := []struct {
		name                       string
		in                         []PushResult
		wantOK, wantSkip, wantAuth bool
	}{
		{"pushed", []PushResult{{Outcome: OutcomePushed}}, true, false, false},
		{"already-on-server", []PushResult{{Outcome: OutcomeAlreadyOnServer}}, true, false, false},
		{"one-of-two-pushed", []PushResult{{Outcome: OutcomeUploadError}, {Outcome: OutcomePushed}}, true, false, false},
		{"no-local-save-is-skip", []PushResult{{Outcome: OutcomeNoLocalSave}}, false, true, false},
		{"empty-local-is-skip", []PushResult{{Outcome: OutcomeEmptyLocalSave}}, false, true, false},
		{"upload-error-is-fail", []PushResult{{Outcome: OutcomeUploadError}}, false, false, false},
		{"hash-mismatch-is-fail", []PushResult{{Outcome: OutcomeHashMismatch}}, false, false, false},
		{"resolve-fail-is-fail", []PushResult{{Outcome: OutcomeResolveFail}}, false, false, false},
		{"auth-expired-surfaces", []PushResult{{Outcome: OutcomeUploadError, AuthExpired: true}}, false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, skip, auth := applyPush(tc.in)
			if ok != tc.wantOK || skip != tc.wantSkip || auth != tc.wantAuth {
				t.Errorf("applyPush = (ok=%v skip=%v auth=%v) want (ok=%v skip=%v auth=%v)",
					ok, skip, auth, tc.wantOK, tc.wantSkip, tc.wantAuth)
			}
		})
	}
}
