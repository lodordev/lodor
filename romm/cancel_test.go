package romm

// Real download cancel (lodor#7 follow-up): the streaming chunk loops poll the
// client's CancelCheck BETWEEN chunks (time-gated) and stop with ErrCancelled —
// bytes already written STAY (the caller keeps its partial .tmp for the HTTP-Range
// resume). These tests lock:
//
//   1. MID-STREAM CANCEL: a slow streaming transfer is cut short the moment the
//      check flips true — ErrCancelled (wrapped, errors.Is-detectable), a partial
//      byte count > 0 and < total.
//   2. NIL CHECK = NO-OP: a client without CancelCheck streams to completion —
//      byte-identical to the pre-cancel behavior (the daemons' guarantee).
//   3. PRE-CANCELLED: a check that is already true stops the copy before the first
//      chunk lands (the launcher's leftover-sentinel case is cleared by Clear()
//      before every run; this is the belt if it ever isn't).

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lodor/config"
)

// slowBodyServer streams `chunks` chunks of `chunk` bytes, flushing each and
// sleeping `delay` between them — long enough for the copy loop's time-gated
// cancel poll (200ms) to fire mid-body.
func slowBodyServer(t *testing.T, chunks int, chunk []byte, delay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(chunks*len(chunk)))
		fl, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			if _, err := w.Write(chunk); err != nil {
				return // client hung up (cancelled) — expected
			}
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(delay)
		}
	}))
}

func cancelTestClient(url string) *Client {
	return NewClient(config.Host{RootURI: url, Token: "t"}, 30*time.Second)
}

func TestCancelCheckStopsMidStream(t *testing.T) {
	chunk := []byte(strings.Repeat("x", 8*1024))
	srv := slowBodyServer(t, 20, chunk, 120*time.Millisecond) // ~2.4s total
	defer srv.Close()

	c := cancelTestClient(srv.URL)
	var polls atomic.Int64
	c.CancelCheck = func() bool {
		// False on the pre-copy check, true once the copy is under way — the
		// launcher's B-press arriving mid-transfer.
		return polls.Add(1) > 1
	}
	var buf bytes.Buffer
	n, err := c.DownloadRomContentTo(5, "Game.chd", &buf, nil)
	if err == nil {
		t.Fatalf("cancelled transfer reported success (n=%d)", n)
	}
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want errors.Is ErrCancelled", err)
	}
	if n <= 0 || n >= int64(20*len(chunk)) {
		t.Errorf("partial bytes = %d, want >0 and < total %d (partial kept, not complete)", n, 20*len(chunk))
	}
	if int64(buf.Len()) != n {
		t.Errorf("reported n=%d but %d bytes written — the partial count must be honest", n, buf.Len())
	}
}

func TestNilCancelCheckStreamsToCompletion(t *testing.T) {
	chunk := []byte(strings.Repeat("y", 4*1024))
	srv := slowBodyServer(t, 3, chunk, 10*time.Millisecond)
	defer srv.Close()

	c := cancelTestClient(srv.URL) // CancelCheck nil — the default everywhere
	var buf bytes.Buffer
	n, err := c.DownloadRomContentTo(5, "Game.chd", &buf, nil)
	if err != nil {
		t.Fatalf("uncancellable transfer failed: %v", err)
	}
	if want := int64(3 * len(chunk)); n != want || int64(buf.Len()) != want {
		t.Errorf("n=%d buf=%d, want full %d bytes", n, buf.Len(), want)
	}
}

func TestPreCancelledStopsBeforeFirstChunk(t *testing.T) {
	chunk := []byte(strings.Repeat("z", 4*1024))
	srv := slowBodyServer(t, 3, chunk, 10*time.Millisecond)
	defer srv.Close()

	c := cancelTestClient(srv.URL)
	c.CancelCheck = func() bool { return true }
	var buf bytes.Buffer
	n, err := c.DownloadRomContentTo(5, "Game.chd", &buf, nil)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if n != 0 || buf.Len() != 0 {
		t.Errorf("pre-cancelled copy wrote %d bytes, want 0", buf.Len())
	}
}
