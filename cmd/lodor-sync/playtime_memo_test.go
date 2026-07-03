package main

import (
	"encoding/json"
	"errors"
	"testing"

	"lodor/playtime"
)

// TestMergePeerRecordRetriesAfterTransientError is the bug #166 proof: when MergePeer
// returns a transient error, the record's save_id must NOT be memoized, so a SECOND
// --sync-playtime run re-attempts it and, on success, merges the peer's playtime that
// would otherwise have been dropped forever.
func TestMergePeerRecordRetriesAfterTransientError(t *testing.T) {
	memo := map[int]int64{}
	body, _ := json.Marshal(playtime.Record{}) // any decodable record

	// Run 1: merge fails transiently.
	failing := func(playtime.Record, string) (bool, error) {
		return false, errors.New("transient: totals locked")
	}
	if merged := mergePeerRecord(memo, 42, 1000, body, "selfdev", failing); merged {
		t.Fatal("run1: reported merged on a merge error")
	}
	if _, seen := memo[42]; seen {
		t.Fatal("run1: save 42 was memoized despite a merge error — it would never re-merge (bug #166)")
	}

	// Run 2: same record, merge now succeeds and changes totals.
	succeeding := func(playtime.Record, string) (bool, error) {
		return true, nil
	}
	if merged := mergePeerRecord(memo, 42, 1000, body, "selfdev", succeeding); !merged {
		t.Fatal("run2: expected the previously-failed record to re-merge successfully")
	}
	if memo[42] != 1000 {
		t.Fatalf("run2: save 42 memo = %d, want 1000 (memoized after durable merge)", memo[42])
	}
}

// TestMergePeerRecordMemoizesOnSuccess proves a clean successful merge memoizes so it
// is not re-fetched next run.
func TestMergePeerRecordMemoizesOnSuccess(t *testing.T) {
	memo := map[int]int64{}
	body, _ := json.Marshal(playtime.Record{})
	noop := func(playtime.Record, string) (bool, error) { return false, nil }
	if merged := mergePeerRecord(memo, 7, 555, body, "d", noop); merged {
		t.Fatal("clean no-op reported as merged")
	}
	if memo[7] != 555 {
		t.Fatalf("save 7 memo = %d, want 555 (memoized on clean success)", memo[7])
	}
}

// TestMergePeerRecordMemoizesUndecodable proves a corrupt body is memoized (pointless
// to re-fetch) and never counts as a merge.
func TestMergePeerRecordMemoizesUndecodable(t *testing.T) {
	memo := map[int]int64{}
	called := false
	mergeFn := func(playtime.Record, string) (bool, error) { called = true; return true, nil }
	if merged := mergePeerRecord(memo, 9, 111, []byte("{not json"), "d", mergeFn); merged {
		t.Fatal("undecodable body reported as merged")
	}
	if called {
		t.Fatal("MergePeer was called on an undecodable body")
	}
	if memo[9] != 111 {
		t.Fatalf("save 9 memo = %d, want 111 (memoized to skip a dead body)", memo[9])
	}
}
