package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func stateLedgerEnv(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("LODOR_PAK_DIR", base)
	return base
}

func TestStateLedgerRoundTrip(t *testing.T) {
	stateLedgerEnv(t)
	if StateKnown(9752, "abc") {
		t.Fatal("empty ledger knows things")
	}
	if err := RecordState(9752, StateLedgerEntry{MD5: "ABC", Size: 7, ServerID: 42, Slot: "auto", Origin: "lodor/knulli/gambatte@r838/arm64"}); err != nil {
		t.Fatal(err)
	}
	if !StateKnown(9752, "abc") { // case-insensitive
		t.Fatal("recorded hash not known")
	}
	if StateKnown(9751, "abc") {
		t.Fatal("hash leaked across roms")
	}
	// A re-record of the SAME logical entry (same Slot AND ServerID AND MD5,
	// case-insensitive) refreshes in place, it does not append.
	if err := RecordState(9752, StateLedgerEntry{MD5: "abc", Size: 9, ServerID: 42, Slot: "auto"}); err != nil {
		t.Fatal(err)
	}
	ents := StateLedgerEntries(9752)
	if len(ents) != 1 || ents[0].Size != 9 {
		t.Fatalf("refresh wrong: %+v", ents)
	}
	if ents[0].RecordedAt.IsZero() {
		t.Fatal("RecordedAt not stamped")
	}
}

// TestStateLedgerDistinctSlotsSameBytes locks BUG 3: two DISTINCT server records
// that carry identical bytes in different slots (or under different server ids)
// must NOT collapse to one — otherwise one server id is orphaned from the ledger
// and retention never deletes it (a server-side leak). Both must stay recorded
// and both must be trackable for retention via their live ServerID.
func TestStateLedgerDistinctSlotsSameBytes(t *testing.T) {
	stateLedgerEnv(t)
	// Same MD5 "dup", different slots + server ids => two distinct artifacts.
	if err := RecordState(9752, StateLedgerEntry{MD5: "dup", Size: 5, ServerID: 100, Slot: "auto", Own: true}); err != nil {
		t.Fatal(err)
	}
	if err := RecordState(9752, StateLedgerEntry{MD5: "dup", Size: 5, ServerID: 200, Slot: "0", Own: true}); err != nil {
		t.Fatal(err)
	}
	ents := StateLedgerEntries(9752)
	if len(ents) != 2 {
		t.Fatalf("distinct slots/servers collapsed: %+v", ents)
	}
	// Both server ids must be present and independently forgettable (retention
	// tracks each). Forgetting one must leave the other live.
	byID := map[int]StateLedgerEntry{}
	for _, e := range ents {
		byID[e.ServerID] = e
	}
	if _, ok := byID[100]; !ok {
		t.Fatalf("server id 100 orphaned (retention leak): %+v", ents)
	}
	if _, ok := byID[200]; !ok {
		t.Fatalf("server id 200 orphaned (retention leak): %+v", ents)
	}
	if byID[100].Slot != "auto" || byID[200].Slot != "0" {
		t.Fatalf("slots crossed: %+v", ents)
	}
	// StateOnServer (the retention/placement gate) sees the bytes as live while
	// EITHER id survives; forgetting both takes it off-server.
	if err := ForgetStateServerID(9752, 100); err != nil {
		t.Fatal(err)
	}
	if !StateOnServer(9752, "dup") {
		t.Fatal("bytes must still be on-server while id 200 lives")
	}
	if err := ForgetStateServerID(9752, 200); err != nil {
		t.Fatal(err)
	}
	if StateOnServer(9752, "dup") {
		t.Fatal("bytes must be off-server once both ids forgotten")
	}
}

func TestStateLedgerForgetServerID(t *testing.T) {
	stateLedgerEnv(t)
	_ = RecordState(9752, StateLedgerEntry{MD5: "aa", ServerID: 7})
	_ = RecordState(9752, StateLedgerEntry{MD5: "bb", ServerID: 8})
	if err := ForgetStateServerID(9752, 7); err != nil {
		t.Fatal(err)
	}
	ents := StateLedgerEntries(9752)
	for _, e := range ents {
		if e.MD5 == "aa" && e.ServerID != 0 {
			t.Fatalf("server id not forgotten: %+v", e)
		}
		if e.MD5 == "bb" && e.ServerID != 8 {
			t.Fatalf("wrong entry touched: %+v", e)
		}
	}
	if !StateKnown(9752, "aa") {
		t.Fatal("entry itself must survive a server-id forget")
	}
}

func TestStateLedgerCorruptDegradesEmpty(t *testing.T) {
	base := stateLedgerEnv(t)
	if err := os.WriteFile(filepath.Join(base, "state-ledger.json"), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if StateKnown(9752, "abc") {
		t.Fatal("corrupt ledger should read as empty")
	}
	// and writing over a corrupt ledger works
	if err := RecordState(9752, StateLedgerEntry{MD5: "abc"}); err != nil {
		t.Fatal(err)
	}
	if !StateKnown(9752, "abc") {
		t.Fatal("recover-by-write failed")
	}
}
