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
	// refresh by MD5, not append
	if err := RecordState(9752, StateLedgerEntry{MD5: "abc", Size: 7, ServerID: 43}); err != nil {
		t.Fatal(err)
	}
	ents := StateLedgerEntries(9752)
	if len(ents) != 1 || ents[0].ServerID != 43 {
		t.Fatalf("refresh wrong: %+v", ents)
	}
	if ents[0].RecordedAt.IsZero() {
		t.Fatal("RecordedAt not stamped")
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
