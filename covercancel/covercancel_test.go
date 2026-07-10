package covercancel

import (
	"context"
	"testing"
	"time"
)

// withTempSentinel points Path at a per-test temp file so tests never touch the real
// /tmp/lodor-cover-cancel and never race each other. It restores the const-backed var
// on cleanup. (Path is a var-like package symbol; we swap it via a local override.)
func withTempSentinel(t *testing.T) func() {
	t.Helper()
	orig := pathOverride
	pathOverride = t.TempDir() + "/cancel"
	return func() { pathOverride = orig }
}

func TestRequestClearRoundTrip(t *testing.T) {
	defer withTempSentinel(t)()

	if Requested() {
		t.Fatal("fresh sentinel path should not report cancel")
	}
	if err := Request(); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !Requested() {
		t.Fatal("after Request, cancel should be reported")
	}
	Clear()
	if Requested() {
		t.Fatal("after Clear, cancel should be gone")
	}
	// Clear on an already-absent sentinel is a no-op, not an error path.
	Clear()
}

func TestWithSignalCancelsWhenSentinelAppears(t *testing.T) {
	defer withTempSentinel(t)()

	ctx, cancel := WithSignal(context.Background())
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("ctx cancelled before any signal")
	case <-time.After(50 * time.Millisecond):
	}

	if err := Request(); err != nil {
		t.Fatalf("Request: %v", err)
	}

	// The watcher polls every pollInterval; allow a few intervals of slack.
	select {
	case <-ctx.Done():
		// good — cancelled promptly after the sentinel appeared
	case <-time.After(2 * time.Second):
		t.Fatal("ctx not cancelled within one iteration after sentinel appeared")
	}
}

func TestWithSignalAlreadyCancelled(t *testing.T) {
	defer withTempSentinel(t)()

	if err := Request(); err != nil {
		t.Fatalf("Request: %v", err)
	}
	ctx, cancel := WithSignal(context.Background())
	defer cancel()

	select {
	case <-ctx.Done():
		// good — a pre-existing sentinel cancels immediately, no request is started
	case <-time.After(time.Second):
		t.Fatal("pre-existing sentinel did not cancel the context immediately")
	}
}

func TestWithSignalParentCancelStopsWatcher(t *testing.T) {
	defer withTempSentinel(t)()

	parent, parentCancel := context.WithCancel(context.Background())
	ctx, cancel := WithSignal(parent)
	defer cancel()

	parentCancel()
	select {
	case <-ctx.Done():
		// good — cancelling the parent cancels the child (and the watcher exits)
	case <-time.After(time.Second):
		t.Fatal("parent cancel did not propagate to child ctx")
	}
}
