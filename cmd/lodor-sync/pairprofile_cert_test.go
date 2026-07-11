package main

// lodor#35, --pair-profile leg: a certificate-verification failure during profile
// pairing must carry the SAME machine-readable reason=tls RESULT token --pair emits,
// so the wizard's trust offer works for profiles too instead of showing a generic
// failure. emitCertFail is the shared emitter; these tests drive it with a REAL
// verification failure through the actual ExchangeToken wrap chain and assert the
// exact stdout line (captured via a pipe — the RESULT stream is the contract).

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"lodor/config"
	"lodor/romm"
)

// captureStdout runs fn with os.Stdout swapped for a pipe and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out)
}

// TestEmitCertFailTagsProfileResult: a real self-signed verification failure emits
// exactly the caller's tagged RESULT line and reports true (the --pair-profile line
// shape: paired=0 username= reason=tls — additive over the untagged failure line).
func TestEmitCertFailTagsProfileResult(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	_, err := romm.ExchangeToken(config.Host{RootURI: ts.URL}, "CODE")
	if err == nil {
		t.Fatal("verification against a self-signed server must fail")
	}

	var fired bool
	out := captureStdout(t, func() {
		fired = emitCertFail(err, "RESULT paired=0 username= reason=tls")
	})
	if !fired {
		t.Fatalf("cert failure not classified: %v", err)
	}
	if out != "RESULT paired=0 username= reason=tls\n" {
		t.Fatalf("RESULT line = %q", out)
	}
}

// TestEmitCertFailSilentOnOtherErrors: nil and non-cert errors print NOTHING and
// report false — the generic failure path stays in charge, and no reason=tls token
// can appear for a plainly unreachable server (the trust offer would be useless).
func TestEmitCertFailSilentOnOtherErrors(t *testing.T) {
	for name, err := range map[string]error{
		"nil":      nil,
		"non-cert": errors.New("connection refused"),
	} {
		var fired bool
		out := captureStdout(t, func() {
			fired = emitCertFail(err, "RESULT paired=0 username= reason=tls")
		})
		if fired || out != "" {
			t.Errorf("%s: fired=%v out=%q — must be silent", name, fired, out)
		}
	}
}
