package romm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lodor/config"
)

// TestAuthErrorClassification locks the 401/403 → AuthError mapping: a 401 is
// ALWAYS a pairing problem; a 403 is one only when the body blames the token /
// credentials; scope/permission 403s and other statuses stay generic errors.
func TestAuthErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"401 bare", 401, "", true},
		{"401 fastapi detail", 401, `{"detail":"Could not validate credentials"}`, true},
		{"403 invalid token", 403, `{"detail":"Invalid token"}`, true},
		{"403 token expired", 403, `{"detail":"token expired"}`, true},
		{"403 token revoked", 403, `{"detail":"This token was revoked"}`, true},
		{"403 not authenticated", 403, `{"detail":"Not authenticated"}`, true},
		{"403 plain forbidden (scope) is NOT auth-expired", 403, `{"detail":"Forbidden"}`, false},
		{"403 insufficient permissions is NOT auth-expired", 403, `{"detail":"Insufficient permissions"}`, false},
		{"403 cloudflare access html is NOT auth-expired", 403, "<html>error code: 1010</html>", false},
		{"500 is NOT auth-expired", 500, "boom", false},
		{"409 is NOT auth-expired", 409, `{"error":"conflict"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := authErrorFromStatus(c.status, []byte(c.body))
			if got := err != nil; got != c.want {
				t.Errorf("authErrorFromStatus(%d, %q) auth-error = %v, want %v", c.status, c.body, got, c.want)
			}
			if err != nil && !IsAuthError(err) {
				t.Errorf("IsAuthError(authErrorFromStatus(...)) = false, want true")
			}
		})
	}
}

// TestAuthErrorWireMapping proves the typed error survives each transport method
// end-to-end against a live (test) HTTP server — the shape every cmd call site
// switches on via IsAuthError.
func TestAuthErrorWireMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"detail":"Could not validate credentials"}`)
	}))
	defer srv.Close()

	c := NewClient(config.Host{RootURI: srv.URL, Token: "dead"}, 5*time.Second)

	// doJSON path
	if _, err := c.GetRom(1); !IsAuthError(err) {
		t.Errorf("GetRom on 401: IsAuthError = false, want true (err=%v)", err)
	}
	// doRaw path
	if _, err := c.doRaw("GET", "/api/users"); !IsAuthError(err) {
		t.Errorf("doRaw on 401: IsAuthError = false, want true (err=%v)", err)
	}
	// doRawStreamTo path
	if _, err := c.doRawStreamTo("/api/roms/1/content/x", discardWriter{}, nil); !IsAuthError(err) {
		t.Errorf("doRawStreamTo on 401: IsAuthError = false, want true (err=%v)", err)
	}

	// 500 must stay generic on the same paths.
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv500.Close()
	c500 := NewClient(config.Host{RootURI: srv500.URL, Token: "dead"}, 5*time.Second)
	if _, err := c500.GetRom(1); err == nil || IsAuthError(err) {
		t.Errorf("GetRom on 500: want generic error, got %v (IsAuthError=%v)", err, IsAuthError(err))
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
