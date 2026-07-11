package update

// Resumable update downloads (lodor#46). The contract under test:
//
//   - an interrupted transfer keeps a partial zip in the <dir>.partial sidecar
//     WITH an asset-identity file, and the next Stage for the SAME asset
//     Range-resumes it (prefix re-hashed FROM DISK, so the final sha256 always
//     covers the real bytes);
//   - a DIFFERENT asset (version bump mid-partial) discards the sidecar and
//     starts clean — never a misreported hash mismatch;
//   - a user cancel keeps the partial (resume contract) and stages nothing;
//   - a complete-but-corrupt artifact still removes staging AND sidecar with
//     ErrHashMismatch (the exit-4 semantics are unchanged);
//   - servers that ignore Range (200) or reject the offset (416) degrade to a
//     clean full fetch — resume is an optimization, never a correctness input.

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rangeServer serves b honoring HTTP Range (via http.ServeContent) and records
// every request's Range header ("" when absent).
func rangeServer(t *testing.T, b []byte) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		http.ServeContent(w, r, "upd.zip", time.Unix(0, 0), bytes.NewReader(b))
	}))
	t.Cleanup(srv.Close)
	return srv, &ranges
}

func partialPaths(dir string) (pd, zipPart, identity string) {
	pd = dir + partialDirSuffix
	return pd, filepath.Join(pd, partialZipName), filepath.Join(pd, identityFileName)
}

// seedPartial plants a sidecar as a previous interrupted run would have left it.
func seedPartial(t *testing.T, dir string, prefix []byte, identity string) {
	t.Helper()
	pd, zipPart, idPath := partialPaths(dir)
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPart, prefix, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idPath, []byte(identity), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertStaged(t *testing.T, dir string) {
	t.Helper()
	for _, p := range []string{
		filepath.Join(dir, ReadyFileName),
		filepath.Join(dir, "tree", "lodor-sync"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s after Stage: %v", p, err)
		}
	}
}

// TestStageResumesInterruptedDownload is the end-to-end resume proof: attempt 1
// is cut off mid-body (server closes early), the partial + identity survive;
// attempt 2 sends Range from the exact partial size, gets a 206, and the FINAL
// sha256 verifies — which can only happen if the on-disk prefix was re-hashed
// and the remainder appended, never re-sent bytes double-hashed.
func TestStageResumesInterruptedDownload(t *testing.T) {
	zipBytes := mkZip(t, map[string]string{
		"lodor-sync":      strings.Repeat("fake-binary ", 4096), // big enough to cut in half
		"lib/some-lib.sh": "echo lib",
	})
	cut := len(zipBytes) / 2
	var first atomic.Bool
	first.Store(true)
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if first.CompareAndSwap(true, false) {
			// declare the full length, deliver half, die — a dropped radio
			w.Header().Set("Content-Length", strconv.Itoa(len(zipBytes)))
			_, _ = w.Write(zipBytes[:cut])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "upd.zip", time.Unix(0, 0), bytes.NewReader(zipBytes))
	}))
	t.Cleanup(srv.Close)

	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)), SHA256: sum(zipBytes)}

	err := Stage(asset, dir, "0.9.9", time.Minute, nil, nil)
	if err == nil || err == ErrHashMismatch {
		t.Fatalf("interrupted Stage = %v, want a transfer error (not nil, not mismatch)", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("staging dir must be removed on a failed attempt")
	}
	_, zipPart, idPath := partialPaths(dir)
	fi, err := os.Stat(zipPart)
	if err != nil {
		t.Fatalf("partial zip must survive an interrupted transfer: %v", err)
	}
	if fi.Size() != int64(cut) {
		t.Fatalf("partial holds %d bytes, want %d", fi.Size(), cut)
	}
	id, err := os.ReadFile(idPath)
	if err != nil || string(id) != identityString(asset, "0.9.9") {
		t.Fatalf("identity file wrong/missing: %q err=%v", id, err)
	}

	if err := Stage(asset, dir, "0.9.9", time.Minute, nil, nil); err != nil {
		t.Fatalf("resumed Stage: %v", err)
	}
	assertStaged(t, dir)
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 2 || ranges[0] != "" {
		t.Fatalf("requests seen: %q (want 2, first without Range)", ranges)
	}
	if want := "bytes=" + strconv.Itoa(cut) + "-"; ranges[1] != want {
		t.Errorf("resume sent Range %q, want %q", ranges[1], want)
	}
	if pd, _, _ := partialPaths(dir); dirExists(pd) {
		t.Errorf("sidecar must be consumed by a successful stage")
	}
}

// TestStageIdentityMismatchDiscardsPartial: a partial from version A must NOT
// be resumed into version B's fetch — the stale bytes are discarded (no Range
// request) and the stage succeeds cleanly instead of misreporting corruption.
func TestStageIdentityMismatchDiscardsPartial(t *testing.T) {
	zipBytes := mkZip(t, map[string]string{"lodor-sync": "fake-binary"})
	srv, ranges := rangeServer(t, zipBytes)
	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)), SHA256: sum(zipBytes)}

	// a previous run's partial for an OLDER release: same URL shape, other identity
	old := asset
	old.SHA256 = sum([]byte("older artifact"))
	seedPartial(t, dir, []byte("stale half-downloaded garbage"), identityString(old, "0.9.8"))

	if err := Stage(asset, dir, "0.9.9", time.Minute, nil, nil); err != nil {
		t.Fatalf("Stage after identity bump = %v, want success (never a misreported mismatch)", err)
	}
	assertStaged(t, dir)
	for _, r := range *ranges {
		if r != "" {
			t.Errorf("a mismatched-identity partial must not be resumed (saw Range %q)", r)
		}
	}
}

// TestStagePrefixRehashCatchesCorruptPartial: identity matches but the on-disk
// prefix is corrupt. The resume appends the genuine remainder, and the final
// hash — which covers the REAL disk bytes because the prefix is re-hashed —
// must fail, removing staging AND the poisoned sidecar (no mismatch loop).
func TestStagePrefixRehashCatchesCorruptPartial(t *testing.T) {
	zipBytes := mkZip(t, map[string]string{"lodor-sync": strings.Repeat("fake-binary ", 4096)})
	cut := len(zipBytes) / 2
	srv, _ := rangeServer(t, zipBytes)
	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)), SHA256: sum(zipBytes)}

	corrupt := bytes.Repeat([]byte{0xAB}, cut) // right length, wrong bytes
	seedPartial(t, dir, corrupt, identityString(asset, "0.9.9"))

	if err := Stage(asset, dir, "0.9.9", time.Minute, nil, nil); err != ErrHashMismatch {
		t.Fatalf("Stage over a corrupt prefix = %v, want ErrHashMismatch", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("staging dir must be removed on hash mismatch")
	}
	if pd, _, _ := partialPaths(dir); dirExists(pd) {
		t.Errorf("a proven-corrupt partial must be discarded, or every retry mismatches forever")
	}
}

// TestStageCancelKeepsPartial: a B-press style cancel mid-transfer returns
// ErrCancelled, stages NOTHING, and keeps partial + identity for the resume.
func TestStageCancelKeepsPartial(t *testing.T) {
	old := cancelPollInterval
	cancelPollInterval = 0 // poll every chunk — the test must not wait out 200ms gates
	t.Cleanup(func() { cancelPollInterval = old })

	zipBytes := mkZip(t, map[string]string{"lodor-sync": strings.Repeat("fake-binary ", 8192)})
	halfway := len(zipBytes) / 2
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(zipBytes)))
		_, _ = w.Write(zipBytes[:halfway])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // hold the rest until the client cancels
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)), SHA256: sum(zipBytes)}
	var sawBytes atomic.Bool
	prog := func(phase string, pct int) {
		if phase == "Downloading update…" && pct > 0 {
			sawBytes.Store(true)
		}
	}
	cancel := func() bool { return sawBytes.Load() } // cancel as soon as real bytes landed

	err := Stage(asset, dir, "0.9.9", time.Minute, prog, cancel)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Stage = %v, want ErrCancelled", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a cancelled stage must stage nothing")
	}
	_, zipPart, idPath := partialPaths(dir)
	if fi, err := os.Stat(zipPart); err != nil || fi.Size() == 0 {
		t.Errorf("cancel must keep the partial for resume (err=%v)", err)
	}
	if _, err := os.Stat(idPath); err != nil {
		t.Errorf("cancel must keep the identity beside the partial: %v", err)
	}
}

// TestStageMismatchRemovesPartialToo extends the existing exit-4 contract to
// the sidecar: a COMPLETE download that fails verification discards the partial
// as well — resume must never re-serve proven-corrupt bytes.
func TestStageMismatchRemovesPartialToo(t *testing.T) {
	zipBytes := mkZip(t, map[string]string{"lodor-sync": "fake"})
	srv, _ := rangeServer(t, zipBytes)
	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)), SHA256: sum([]byte("something else"))}
	if err := Stage(asset, dir, "0.9.9", time.Minute, nil, nil); err != ErrHashMismatch {
		t.Fatalf("Stage = %v, want ErrHashMismatch", err)
	}
	if pd, _, _ := partialPaths(dir); dirExists(pd) {
		t.Errorf("partial sidecar must be removed on hash mismatch")
	}
}

// TestStageResumeServerIgnoresRange: a proxy/CDN that answers a ranged request
// with 200 sends the whole file — the partial is dropped, the hash reset, and
// the stage still verifies. Resume degrades, correctness holds.
func TestStageResumeServerIgnoresRange(t *testing.T) {
	zipBytes := mkZip(t, map[string]string{"lodor-sync": strings.Repeat("fake-binary ", 1024)})
	srv := serveBytes(t, zipBytes) // plain handler: ignores Range, always 200 full body
	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)), SHA256: sum(zipBytes)}
	seedPartial(t, dir, zipBytes[:len(zipBytes)/2], identityString(asset, "0.9.9"))

	if err := Stage(asset, dir, "0.9.9", time.Minute, nil, nil); err != nil {
		t.Fatalf("Stage against a Range-blind server: %v", err)
	}
	assertStaged(t, dir)
}

// TestStageResumeStalePartial416: a partial at/beyond the server's EOF draws a
// 416 — one clean restart from zero, then success (the romm 416 contract).
func TestStageResumeStalePartial416(t *testing.T) {
	zipBytes := mkZip(t, map[string]string{"lodor-sync": "fake-binary"})
	srv, ranges := rangeServer(t, zipBytes)
	dir := filepath.Join(t.TempDir(), StageDirName)
	// Size deliberately unknown (0): the too-long partial can't be pre-trimmed
	// locally, so the server's 416 is the only signal — the path under test.
	asset := Asset{URL: srv.URL, Size: 0, SHA256: sum(zipBytes)}
	stale := append(append([]byte{}, zipBytes...), []byte("overhang-garbage")...)
	seedPartial(t, dir, stale, identityString(asset, "0.9.9"))

	if err := Stage(asset, dir, "0.9.9", time.Minute, nil, nil); err != nil {
		t.Fatalf("Stage after 416 restart: %v", err)
	}
	assertStaged(t, dir)
	saw416Retry := false
	for i, r := range *ranges {
		if r == "" && i > 0 {
			saw416Retry = true // the clean re-fetch after the ranged 416
		}
	}
	if !saw416Retry {
		t.Errorf("expected a ranged request then a clean restart, saw %q", *ranges)
	}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
