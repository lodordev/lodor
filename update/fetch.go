package update

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lodor/fsutil"
)

// ReadyFileName marks a fully verified staging: the shell applier refuses to
// touch anything without it, and its presence at boot means "resume the apply".
const ReadyFileName = "READY"

// StageDirName is the staging root, created beside config.json (the engine's
// CWD): <cfg-dir>/.update/{download.zip, tree/, READY}.
const StageDirName = ".update"

// ErrHashMismatch distinguishes a corrupt/tampered download (exit 4) from an
// unreachable host (exit 3).
var ErrHashMismatch = fmt.Errorf("update: downloaded artifact does not match expected sha256")

// Progress reports HONEST staging progress to the caller so the launcher can
// draw a real bar (never a fabricated one). phase is a short human label
// ("Downloading update…", "Verifying…", "Extracting…"); pct is 0..100, or -1
// when the byte total is unknown so no bar can be drawn truthfully. It is
// best-effort UI only: a Progress callback never changes Stage's error or the
// mode's exit code. A nil callback disables reporting (every test call site).
type Progress func(phase string, pct int)

func (p Progress) report(phase string, pct int) {
	if p != nil {
		p(phase, pct)
	}
}

// Stage downloads an update asset, verifies its sha256, extracts it into
// <dir>/tree, and writes the READY marker — in that order, so READY implies a
// complete verified tree. ANY failure removes the whole staging dir: rollback
// is "the update never started", and the running binary is never touched here
// (the shell applier owns the swap; a running binary can't replace itself on
// FAT32 anyway).
//
// progress (may be nil) is fed REAL byte-level download percent + the current
// phase so a launcher can render an honest progress screen — see Progress.
func Stage(asset Asset, dir, version string, timeout time.Duration, progress Progress) error {
	fail := func(err error) error {
		_ = os.RemoveAll(dir)
		return err
	}
	if asset.URL == "" || asset.SHA256 == "" {
		return fmt.Errorf("update: asset entry missing url or sha256")
	}
	_ = os.RemoveAll(dir) // a fresh stage always replaces any prior/partial one
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	zipPath := filepath.Join(dir, "download.zip")
	if err := download(asset, zipPath, timeout, progress); err != nil {
		if err == ErrHashMismatch {
			return fail(err)
		}
		return fail(fmt.Errorf("update download: %w", err))
	}
	// The bytes are down + sha256-verified in download(); extraction is a local,
	// fast step. Report it as a distinct phase so a stalled unzip is never
	// mistaken for a stalled download.
	progress.report("Verifying update…", 100)
	treeDir := filepath.Join(dir, "tree")
	if err := extractZip(zipPath, treeDir); err != nil {
		return fail(fmt.Errorf("update extract: %w", err))
	}
	_ = os.Remove(zipPath) // tree is verified-by-hash transitively; the zip is dead weight on a small card
	ready := fmt.Sprintf("version=%s\nsha256=%s\nurl=%s\n", version, asset.SHA256, asset.URL)
	if err := fsutil.WriteFileAtomicString(filepath.Join(dir, ReadyFileName), ready, 0o644); err != nil {
		return fail(err)
	}
	return nil
}

// countingWriter tallies bytes as they stream and emits a REAL percent to a
// Progress callback, throttled to whole-percent changes so a slow radio doesn't
// thrash the side-channel file. total<=0 means the size is unknown, so it
// reports pct=-1 (the launcher shows the phase label, no bar) — never a guess.
type countingWriter struct {
	total    int64
	written  int64
	lastPct  int
	progress Progress
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.written += int64(n)
	if w.progress == nil {
		return n, nil
	}
	if w.total <= 0 {
		w.progress.report("Downloading update…", -1)
		return n, nil
	}
	pct := int(w.written * 100 / w.total)
	if pct > 100 {
		pct = 100
	}
	if pct != w.lastPct {
		w.lastPct = pct
		w.progress.report("Downloading update…", pct)
	}
	return n, nil
}

// download streams the asset to path while hashing, then verifies both the
// sha256 and (when the manifest carries one) the byte size. Content-Length is
// also enforced mid-stream so a truncated body can never verify. progress (may
// be nil) receives the live byte percent as the body streams.
func download(asset Asset, path string, timeout time.Duration, progress Progress) error {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "lodor-sync-updater")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// Prefer the manifest's Size for the denominator (it's what the hash covers);
	// fall back to Content-Length. Emit an initial 0% so the bar appears at the
	// instant the transfer starts, not only after the first chunk lands.
	total := asset.Size
	if total <= 0 {
		total = resp.ContentLength
	}
	progress.report("Downloading update…", 0)
	cw := &countingWriter{total: total, progress: progress}
	h := sha256.New()
	n, cErr := io.Copy(io.MultiWriter(f, h, cw), resp.Body)
	if sErr := f.Sync(); cErr == nil {
		cErr = sErr
	}
	if clErr := f.Close(); cErr == nil {
		cErr = clErr
	}
	if cErr != nil {
		return cErr
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return fmt.Errorf("truncated: got %d of %d bytes", n, resp.ContentLength)
	}
	if asset.Size > 0 && n != asset.Size {
		return ErrHashMismatch
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), asset.SHA256) {
		return ErrHashMismatch
	}
	return nil
}

// extractZip unpacks src into destDir with zip-slip protection. Plain
// sequential writes + one dir fsync at the end: the READY marker (written
// atomically AFTER extraction) is the durability point, not each member file.
func extractZip(src, destDir string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	for _, f := range r.File {
		target := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(target, cleanDest) {
			return fmt.Errorf("illegal path in zip: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode() & 0o777
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, cErr := io.Copy(out, rc)
		rc.Close()
		if clErr := out.Close(); cErr == nil {
			cErr = clErr
		}
		if cErr != nil {
			return cErr
		}
	}
	fsutil.SyncDir(destDir)
	return nil
}
