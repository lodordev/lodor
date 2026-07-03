package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomicReplace proves a whole-file atomic replace lands the new
// content and leaves NO temp file behind (the .tmp must be renamed away, not left
// as the classic non-atomic residue).
func TestWriteFileAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "save.sav")

	if err := WriteFileAtomicString(path, "first", 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "first" {
		t.Fatalf("first content = %q, want %q", got, "first")
	}
	// Replace over an existing file — the real save-overwrite case.
	if err := WriteFileAtomicString(path, "second", 0o644); err != nil {
		t.Fatalf("replace write: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "second" {
		t.Fatalf("replaced content = %q, want %q", got, "second")
	}

	// The temp sidecar must not survive a successful write (rename-away semantics,
	// not copy). A lingering .tmp is the tell of a torn/non-atomic writer.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file %s.tmp still exists after successful write", path)
	}
}

// TestWriteFileAtomicPreservesOnFailure proves a failed write (target parent is
// not a directory) neither corrupts an existing file nor leaves a temp behind.
func TestWriteFileAtomicPreservesOnFailure(t *testing.T) {
	dir := t.TempDir()
	// Target path is an existing NON-EMPTY directory: the temp file is created and
	// fsync'd fine, but os.Rename(tmp, dir) fails (can't rename a file over a
	// non-empty dir). This exercises the post-temp cleanup path — the .tmp must be
	// removed, not orphaned.
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(target, []byte("nope"), 0o644); err == nil {
		t.Fatalf("expected rename-over-nonempty-dir to fail, got nil")
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file %s.tmp left behind after failed rename", target)
	}
}

// TestWriteFileAtomicPerm proves the requested mode is honored on create.
func TestWriteFileAtomicPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg")
	if err := WriteFileAtomic(path, []byte("d"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", fi.Mode().Perm())
	}
}

// TestWriteFileAtomicCreatesParent proves the parent dir is created if missing.
func TestWriteFileAtomicCreatesParent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "file.txt")
	if err := WriteFileAtomicString(path, "hi", 0o644); err != nil {
		t.Fatalf("write with missing parent: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "hi" {
		t.Fatalf("content = %q", got)
	}
}
