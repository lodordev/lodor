package cover

import (
	"context"
	"errors"
	"testing"
	"time"
)

// blockingCtxDownloader blocks until the context is cancelled, then returns the ctx
// error — a stand-in for a slow-radio cover download that only returns when cancelled.
type blockingCtxDownloader struct{ called chan struct{} }

func (d *blockingCtxDownloader) DownloadCoverCtx(ctx context.Context, _ string) ([]byte, error) {
	close(d.called)
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestFetchAndSaveCtxCancelAborts proves an in-flight cover fetch returns promptly with
// OutcomeError once its context is cancelled, instead of blocking on the network — the
// core of the #26 fix. romPath points at a temp dir so the skip-existing gate is false.
func TestFetchAndSaveCtxCancelAborts(t *testing.T) {
	romPath := t.TempDir() + "/Game (USA).gb"

	dl := &blockingCtxDownloader{called: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())

	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := FetchAndSaveCtx(ctx, dl, "/assets/cover/small.png", romPath, false)
		done <- result{out, err}
	}()

	// Wait until the download is actually in flight, then cancel.
	select {
	case <-dl.called:
	case <-time.After(2 * time.Second):
		t.Fatal("download never started")
	}
	cancel()

	select {
	case r := <-done:
		if r.out != OutcomeError {
			t.Fatalf("cancelled fetch: want OutcomeError, got %v", r.out)
		}
		if r.err == nil {
			t.Fatal("cancelled fetch: want a non-nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FetchAndSaveCtx did not return promptly after cancel (the hang)")
	}
}

// TestFetchAndSaveCtxNoCoverShortCircuits confirms the grace contract is preserved: an
// empty coverPath returns OutcomeNoCover with no network call and no error.
func TestFetchAndSaveCtxNoCoverShortCircuits(t *testing.T) {
	dl := &blockingCtxDownloader{called: make(chan struct{})}
	out, err := FetchAndSaveCtx(context.Background(), dl, "", t.TempDir()+"/g.gb", false)
	if err != nil {
		t.Fatalf("no-cover: unexpected error %v", err)
	}
	if out != OutcomeNoCover {
		t.Fatalf("no-cover: want OutcomeNoCover, got %v", out)
	}
	select {
	case <-dl.called:
		t.Fatal("no-cover path must not hit the network")
	default:
	}
}

// TestFetchAndSaveCtxPreCancelled confirms a context already cancelled before the call
// aborts the download without hanging (errors.Is context.Canceled is surfaced).
func TestFetchAndSaveCtxPreCancelled(t *testing.T) {
	romPath := t.TempDir() + "/Game (USA).gb"
	dl := &blockingCtxDownloader{called: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := FetchAndSaveCtx(ctx, dl, "/assets/cover/small.png", romPath, false)
	if out != OutcomeError || err == nil {
		t.Fatalf("pre-cancelled: want OutcomeError+err, got %v / %v", out, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled: want context.Canceled in chain, got %v", err)
	}
}
