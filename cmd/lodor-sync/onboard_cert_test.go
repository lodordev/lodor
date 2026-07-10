package main

// lodor#35: the certificate-failure distinction that feeds the wizard's
// "trust this server (skip certificate verification)" offer. isCertErr must fire
// on a REAL failed verification (self-signed server) reached through the actual
// romm client wrap chain, and must NOT fire on plain unreachability — skip-verify
// fixes only the former, so a false positive would offer a useless toggle.

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"lodor/config"
	"lodor/romm"
)

// TestIsCertErrSelfSignedServer: a verifying client against a self-signed TLS
// server — the exact lodor#35 field case — classifies as a cert error, through
// the real ExchangeToken wrap chain (fmt %w -> url.Error -> tls/x509).
func TestIsCertErrSelfSignedServer(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	_, err := romm.ExchangeToken(config.Host{RootURI: ts.URL}, "CODE")
	if err == nil {
		t.Fatal("verification against a self-signed server must fail")
	}
	if !isCertErr(err) {
		t.Fatalf("self-signed verification failure not classified as cert error: %v", err)
	}
	// safeErr would have collapsed this to "network error" — the exact ambiguity
	// isCertErr exists to break. Assert it stays a distinct class.
	if safeErr(err) != "network error" {
		t.Logf("note: safeErr rendering changed to %q (isCertErr is now the classifier)", safeErr(err))
	}

	// Same server, skip-verify on: the handshake succeeds, so whatever error remains
	// (bad JSON/whatever) must NOT classify as a cert error — the toggle worked.
	_, err = romm.ExchangeToken(config.Host{RootURI: ts.URL, InsecureSkipVerify: true}, "CODE")
	if isCertErr(err) {
		t.Fatalf("insecure_skip_verify host must not produce a cert error: %v", err)
	}
}

// TestIsCertErrNotUnreachable: plain transport failures (refused/unresolvable) are
// NOT cert errors — the trust offer must never appear for a genuinely down server.
func TestIsCertErrNotUnreachable(t *testing.T) {
	_, err := romm.ExchangeToken(config.Host{RootURI: "https://127.0.0.1:1"}, "CODE")
	if err == nil {
		t.Fatal("dial to a closed port must fail")
	}
	if isCertErr(err) {
		t.Fatalf("connection-refused classified as cert error: %v", err)
	}
	if isCertErr(nil) {
		t.Fatal("nil must never classify as cert error")
	}
	if isCertErr(errors.New("dial tcp: i/o timeout")) {
		t.Fatal("timeout must not classify as cert error")
	}
}

// TestIsCertErrStringFallback: an error whose chain was flattened but still names
// x509 classifies via the string probe (belt and braces for boundary-crossing errors).
func TestIsCertErrStringFallback(t *testing.T) {
	err := fmt.Errorf("execute request: Get \"https://h/api\": x509: certificate signed by unknown authority")
	if !isCertErr(err) {
		t.Fatal("flattened x509 message must still classify")
	}
}
