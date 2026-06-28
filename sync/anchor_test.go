package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// anchorEnv points PakDir at a temp dir so the anchor store reads/writes there.
func anchorEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LODOR_PAK_DIR", dir)
	return dir
}

func TestAnchorRoundTrip(t *testing.T) {
	dir := anchorEnv(t)

	if _, ok := LoadAnchor(1234); ok {
		t.Fatal("expected no anchor before any save")
	}

	a := Anchor{Hash: "deadbeef", ServerSaveID: 9, ServerUpdatedAt: time.Unix(1700000000, 0).UTC()}
	if err := SaveAnchor(1234, a); err != nil {
		t.Fatalf("SaveAnchor: %v", err)
	}
	// The store file must exist under the pak dir.
	if _, err := os.Stat(filepath.Join(dir, "sync-anchors.json")); err != nil {
		t.Fatalf("anchor store not written: %v", err)
	}

	got, ok := LoadAnchor(1234)
	if !ok {
		t.Fatal("expected anchor after save")
	}
	if got.Hash != "deadbeef" || got.ServerSaveID != 9 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.SyncedAt.IsZero() {
		t.Error("SyncedAt should be stamped on save")
	}
}

func TestAnchorMultipleRomsAndOverwrite(t *testing.T) {
	anchorEnv(t)
	if err := SaveAnchor(1, Anchor{Hash: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveAnchor(2, Anchor{Hash: "two"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveAnchor(1, Anchor{Hash: "one-v2"}); err != nil {
		t.Fatal(err)
	}
	a1, _ := LoadAnchor(1)
	a2, _ := LoadAnchor(2)
	if a1.Hash != "one-v2" {
		t.Errorf("rom 1 = %q, want one-v2", a1.Hash)
	}
	if a2.Hash != "two" {
		t.Errorf("rom 2 = %q, want two (overwrite of 1 must not touch 2)", a2.Hash)
	}
}

func TestAnchorDelete(t *testing.T) {
	anchorEnv(t)
	_ = SaveAnchor(5, Anchor{Hash: "x"})
	if err := DeleteAnchor(5); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadAnchor(5); ok {
		t.Error("anchor should be gone after delete")
	}
	// Deleting a missing anchor is a no-op.
	if err := DeleteAnchor(999); err != nil {
		t.Errorf("delete missing: %v", err)
	}
}

// TestAnchorCorruptStoreDegradesToEmpty: a garbage store file must read as "no anchors"
// (reconcile-as-first-sync), never error out the sync.
func TestAnchorCorruptStoreDegradesToEmpty(t *testing.T) {
	dir := anchorEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "sync-anchors.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadAnchor(1); ok {
		t.Error("corrupt store should yield no anchor")
	}
	// And a save over a corrupt file repairs it.
	if err := SaveAnchor(1, Anchor{Hash: "fixed"}); err != nil {
		t.Fatalf("save over corrupt store: %v", err)
	}
	if a, ok := LoadAnchor(1); !ok || a.Hash != "fixed" {
		t.Error("save did not repair corrupt store")
	}
}
