package update

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
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

// partialDirSuffix names the resume sidecar BESIDE the staging dir
// (".update.partial" in production). It deliberately lives OUTSIDE StageDirName:
// the staging dir's contract is any-failure-removes-staging (the shell applier
// and the exit-4 semantics depend on it), so the partial must not share its
// lifetime. The applier never looks at the sidecar.
const partialDirSuffix = ".partial"

// identityFileName is the asset-identity file inside the partial sidecar. A
// partial zip is only resumable against the EXACT asset it came from — when a
// release bumps mid-partial the identity no longer matches, and the stale bytes
// are discarded instead of misreporting a hash mismatch (lodor#46's trap).
const identityFileName = "identity"

// partialZipName is the partial artifact inside the sidecar. Never renamed into
// the staging dir until its bytes are complete AND sha256-verified.
const partialZipName = "download.zip.part"

// ErrHashMismatch distinguishes a corrupt/tampered download (exit 4) from an
// unreachable host (exit 3).
var ErrHashMismatch = fmt.Errorf("update: downloaded artifact does not match expected sha256")

// ErrCancelled is returned (wrapped) when the caller's cancel check reported a
// user cancel mid-transfer (the launcher's B-press sentinel, same contract as
// romm.ErrCancelled). The partial zip + identity are deliberately KEPT — the
// next attempt resumes via HTTP Range. Detect with errors.Is.
var ErrCancelled = errors.New("update: download cancelled by user")

// cancelPollInterval rate-limits the cancel check from the copy loop: one
// sentinel stat per interval, not per 32 KB chunk (mirrors romm's copyStream).
// A var so tests can shrink it; production never changes it.
var cancelPollInterval = 200 * time.Millisecond

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
// Resume (lodor#46): the zip streams into a sidecar (<dir>.partial) holding the
// partial bytes plus an asset-identity file. A failed/cancelled TRANSFER keeps
// the sidecar; the next Stage for the SAME asset re-hashes the on-disk prefix
// and continues with an HTTP Range request (206 append / 200 rewrite-from-zero /
// 416 one clean restart — the romm client's proven contract). A different asset
// (version bump mid-partial) discards the sidecar and starts clean, so stale
// bytes can never masquerade as corruption. A COMPLETE download that fails
// verification removes sidecar AND staging (a resume would only mismatch again)
// and returns ErrHashMismatch — the exit-4 semantics are unchanged. The
// extract-and-READY phase stays all-or-nothing exactly as before.
//
// progress (may be nil) is fed REAL byte-level download percent + the current
// phase so a launcher can render an honest progress screen — see Progress.
// cancel (may be nil) is polled between chunks; on true the transfer stops with
// ErrCancelled and the partial is kept (see ErrCancelled).
func Stage(asset Asset, dir, version string, timeout time.Duration, progress Progress, cancel func() bool) error {
	fail := func(err error) error {
		_ = os.RemoveAll(dir)
		return err
	}
	if asset.URL == "" || asset.SHA256 == "" {
		return fmt.Errorf("update: asset entry missing url or sha256")
	}
	_ = os.RemoveAll(dir) // a fresh stage always replaces any prior/partial final staging
	partialDir := dir + partialDirSuffix
	if err := preparePartial(partialDir, asset, version); err != nil {
		return fail(fmt.Errorf("update partial staging: %w", err))
	}
	zipPart := filepath.Join(partialDir, partialZipName)
	if err := download(asset, zipPart, timeout, progress, cancel); err != nil {
		if err == ErrHashMismatch {
			// Proven corrupt/complete-but-wrong: keeping the partial would resume
			// into the same mismatch forever. Discard everything (exit-4 contract).
			_ = os.RemoveAll(partialDir)
			return fail(err)
		}
		// Transfer failure or user cancel: the partial + identity stay for resume;
		// the final staging dir is (already) gone, so the applier sees nothing.
		if errors.Is(err, ErrCancelled) {
			return fail(err)
		}
		return fail(fmt.Errorf("update download: %w", err))
	}
	// The bytes are down + sha256-verified in download(); extraction is a local,
	// fast step. Report it as a distinct phase so a stalled unzip is never
	// mistaken for a stalled download.
	progress.report("Verifying update…", 100)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err) // partial is complete+verified — kept, next attempt skips the network
	}
	zipPath := filepath.Join(dir, "download.zip")
	if err := os.Rename(zipPart, zipPath); err != nil {
		_ = os.RemoveAll(partialDir)
		return fail(err)
	}
	_ = os.RemoveAll(partialDir) // identity consumed with its zip
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

// identityString is the exact byte content of the sidecar's identity file. Any
// difference — version, url, sha, size — means "not the same asset": resume is
// only ever attempted against identical identity bytes.
func identityString(asset Asset, version string) string {
	return fmt.Sprintf("version=%s\nurl=%s\nsha256=%s\nsize=%d\n",
		version, asset.URL, strings.ToLower(asset.SHA256), asset.Size)
}

// preparePartial makes the sidecar dir usable for THIS asset: an existing
// sidecar whose identity matches byte-for-byte is kept (its partial zip will be
// prefix-rehashed and resumed); anything else — no identity, mismatched
// identity (a version bump mid-partial) — is discarded and recreated clean.
// The identity is written atomically BEFORE any bytes stream, so a partial can
// never exist without the identity that validates it.
func preparePartial(partialDir string, asset Asset, version string) error {
	want := identityString(asset, version)
	if got, err := os.ReadFile(filepath.Join(partialDir, identityFileName)); err == nil && string(got) == want {
		return nil
	}
	_ = os.RemoveAll(partialDir)
	if err := os.MkdirAll(partialDir, 0o755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomicString(filepath.Join(partialDir, identityFileName), want, 0o644)
}

// countingWriter tallies bytes as they stream and emits a REAL percent to a
// Progress callback, throttled to whole-percent changes so a slow radio doesn't
// thrash the side-channel file. total<=0 means the size is unknown, so it
// reports pct=-1 (the launcher shows the phase label, no bar) — never a guess.
// written may start >0 on a resume so the bar continues where it left off.
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
//
// Resume: when path already holds bytes (a kept partial), the prefix is
// re-hashed FROM DISK — the final sha256 always covers the real on-disk bytes,
// never an assumed prefix — and the transfer continues with an HTTP Range
// request. Any problem with the prefix silently restarts from zero; resume is
// an optimization, correctness comes from the final hash. cancel (may be nil)
// stops the copy between chunks with ErrCancelled, keeping the partial.
func download(asset Asset, path string, timeout time.Duration, progress Progress, cancel func() bool) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	h := sha256.New()
	offset := rehashPrefix(f, h, asset, progress)

	total := offset
	if asset.Size <= 0 || offset < asset.Size {
		// Bytes still missing (or size unknown): fetch, resuming from offset.
		n, fErr := fetchInto(f, h, offset, asset, timeout, progress, cancel, true)
		if sErr := f.Sync(); fErr == nil {
			fErr = sErr
		}
		if fErr != nil {
			_ = f.Close()
			closed = true
			return fErr
		}
		total = n
	}
	// else: the partial already holds exactly asset.Size bytes — nothing to
	// fetch; the hash verdict below decides (complete partial vs corrupt one).

	if err := f.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	if asset.Size > 0 && total != asset.Size {
		return ErrHashMismatch
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), asset.SHA256) {
		return ErrHashMismatch
	}
	return nil
}

// rehashPrefix feeds the existing on-disk partial through h and returns its
// length (the resume offset). Any inconsistency — unreadable file, short read,
// a partial LONGER than the declared asset — resets both file and hash to zero:
// a broken prefix costs a clean restart, never a wrong verdict.
func rehashPrefix(f *os.File, h hash.Hash, asset Asset, progress Progress) int64 {
	restart := func() int64 {
		h.Reset()
		_ = f.Truncate(0)
		_, _ = f.Seek(0, io.SeekStart)
		return 0
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return restart()
	}
	offset := fi.Size()
	if asset.Size > 0 && offset > asset.Size {
		return restart() // longer than the whole asset: garbage, not a prefix
	}
	progress.report("Checking partial download…", -1)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return restart()
	}
	n, err := io.Copy(h, io.LimitReader(f, offset))
	if err != nil || n != offset {
		return restart()
	}
	return offset
}

// fetchInto GETs the asset (Range-resuming from offset when >0) and appends the
// body to f while feeding h. It returns the TOTAL bytes now on disk. The three
// server behaviors are handled exactly like the romm client's resume contract,
// so a resume can never corrupt the file:
//
//   - 206 Partial Content: honor the range — f is truncated to offset (dropping
//     anything past it) and the remainder appended; the prefix hash continues.
//   - 200 OK: the server ignored the Range header (proxy/CDN that strips it) and
//     is sending the WHOLE file — f and h reset to zero, rewritten clean.
//   - 416 Range Not Satisfiable: our offset is at/beyond the server's EOF (stale
//     partial) — reset to zero and re-fetch once (retry416 guards the loop).
func fetchInto(f *os.File, h hash.Hash, offset int64, asset Asset, timeout time.Duration, progress Progress, cancel func() bool, retry416 bool) (int64, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, asset.URL, nil)
	if err != nil {
		return offset, err
	}
	req.Header.Set("User-Agent", "lodor-sync-updater")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return offset, err
	}
	defer resp.Body.Close()

	restartZero := func() error {
		h.Reset()
		if err := f.Truncate(0); err != nil {
			return err
		}
		_, err := f.Seek(0, io.SeekStart)
		return err
	}

	switch resp.StatusCode {
	case http.StatusPartialContent: // 206 — resume: append from offset
		if offset == 0 {
			return 0, fmt.Errorf("HTTP 206 to a full-file request")
		}
		if err := f.Truncate(offset); err != nil {
			return offset, err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return offset, err
		}
	case http.StatusRequestedRangeNotSatisfiable: // 416 — stale partial: one clean restart
		if offset == 0 || !retry416 {
			return offset, fmt.Errorf("HTTP 416")
		}
		if err := restartZero(); err != nil {
			return 0, err
		}
		return fetchInto(f, h, 0, asset, timeout, progress, cancel, false)
	case http.StatusOK: // 200 — server ignored Range (or fresh fetch): rewrite from zero
		if err := restartZero(); err != nil {
			return 0, err
		}
		offset = 0
	default:
		return offset, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Prefer the manifest's Size for the denominator (it's what the hash covers);
	// fall back to offset+Content-Length. Emit an initial percent so the bar
	// appears at the instant the transfer starts — at the RESUME point, never 0
	// after bytes are already down.
	total := asset.Size
	if total <= 0 && resp.ContentLength > 0 {
		total = offset + resp.ContentLength
	}
	startPct := -1
	if total > 0 {
		startPct = int(offset * 100 / total)
	}
	progress.report("Downloading update…", startPct)
	cw := &countingWriter{total: total, written: offset, lastPct: startPct, progress: progress}
	n, cErr := copyCancel(io.MultiWriter(f, h, cw), resp.Body, cancel)
	if cErr != nil {
		return offset + n, cErr
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		// A truncated body is a TRANSFER failure, not corruption: the bytes on
		// disk are a good prefix, kept for the next resume.
		return offset + n, fmt.Errorf("truncated: got %d of %d bytes", n, resp.ContentLength)
	}
	return offset + n, nil
}

// copyCancel is io.Copy with a between-chunks cancel poll (time-gated, mirrors
// romm's copyStream). nil cancel = plain io.Copy, byte-identical behavior. On
// cancel the bytes already written stay where they are — the caller keeps the
// partial for resume — and ErrCancelled is returned.
func copyCancel(dst io.Writer, src io.Reader, cancel func() bool) (int64, error) {
	if cancel == nil {
		return io.Copy(dst, src)
	}
	if cancel() {
		return 0, ErrCancelled
	}
	var written int64
	buf := make([]byte, 32*1024)
	next := time.Now().Add(cancelPollInterval)
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			written += int64(nw)
			if werr != nil {
				return written, werr
			}
			if nw < nr {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return written, nil
			}
			return written, rerr
		}
		if now := time.Now(); !now.Before(next) {
			if cancel() {
				return written, ErrCancelled
			}
			next = now.Add(cancelPollInterval)
		}
	}
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
