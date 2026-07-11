package main

import (
	"testing"

	"lodor/sync"
	"lodor/syncstamp"
)

// TestStampSyncWritesToPakDir: the cmd-layer helper writes the #43 record into the
// engine's pak dir (LODOR_PAK_DIR — the same resolution every queue file uses).
func TestStampSyncWritesToPakDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LODOR_PAK_DIR", dir)
	stampSync(2, 1)
	st, ok := syncstamp.Read(dir)
	if !ok {
		t.Fatal("stampSync wrote no readable stamp")
	}
	if st.Saves != 2 || st.States != 1 || st.Epoch <= 0 {
		t.Fatalf("stamp = %+v", st)
	}
}

// TestPullSawServer: only outcomes that REQUIRE a server answer count as contact —
// transport/resolve/local-staging failures must never let a run stamp "synced".
func TestPullSawServer(t *testing.T) {
	yes := []sync.PullOutcome{sync.PullWritten, sync.PullInSync, sync.PullLocalUnpushed, sync.PullTombstoned, sync.PullNoServerSave}
	no := []sync.PullOutcome{sync.PullError, sync.PullResolveFail, sync.PullSnapshotFail}
	for _, o := range yes {
		if !pullSawServer(sync.PullResult{Outcome: o}) {
			t.Fatalf("outcome %v should prove server contact", o)
		}
	}
	for _, o := range no {
		if pullSawServer(sync.PullResult{Outcome: o}) {
			t.Fatalf("outcome %v must NOT prove server contact", o)
		}
	}
}
