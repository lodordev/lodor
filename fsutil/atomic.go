// Package fsutil provides FAT32-safe atomic whole-file writes for the handhelds
// LodorOS runs on. On a FAT32 SD card a power-yank mid-write can zero the file
// (the directory entry is updated before the data blocks are flushed), so every
// whole-file replace must go: write a sibling temp, fsync the temp, close it,
// rename over the target, then fsync the parent directory so the rename itself is
// durable. This mirrors the gold-standard writer in platform/manifest.go and the
// config writer, consolidated here so there is ONE correct implementation.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic durably replaces path with data at the given permission mode.
//
// Sequence: MkdirAll(parent) → write path.tmp → f.Sync() → f.Close() →
// os.Rename(tmp, path) → fsync(parent dir). The temp file is removed on any
// error so a failed write never leaves a stray .tmp behind. The parent-dir fsync
// is best-effort: a read-only or fsync-less filesystem must not fail an otherwise
// good write, but on FAT32 it is what makes the rename itself power-safe.
//
// Callers that previously used os.WriteFile+os.Rename (no fsync) are unsafe on
// FAT32; route them through here.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, werr := f.Write(data); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return werr
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return serr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return rerr
	}
	SyncDir(filepath.Dir(path))
	return nil
}

// WriteFileAtomicString is the string convenience wrapper over WriteFileAtomic.
func WriteFileAtomicString(path, content string, perm os.FileMode) error {
	return WriteFileAtomic(path, []byte(content), perm)
}

// SyncDir best-effort fsyncs a directory so a rename into it is persisted. Errors
// are swallowed: some filesystems (and read-only mounts) don't support it, and a
// write that already landed must not be reported as failed. Exported for callers
// that stream large files (e.g. multi-disc .chd) directly and can't route the
// whole payload through WriteFileAtomic, but still need the rename made durable.
func SyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
