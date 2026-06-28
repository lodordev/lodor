package ra

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestBuildLoginRequestPasswordNotInURL is the security-load-bearing test: the
// password must travel in the POST body, never the request URL/query (where it
// would land in access logs).
func TestBuildLoginRequestPasswordNotInURL(t *testing.T) {
	c := NewClient("https://example.test", 5*time.Second)
	req, err := c.buildLoginRequest("Bob", "s3cr3t-p@ss")
	if err != nil {
		t.Fatalf("buildLoginRequest: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.URL.Path != loginPath {
		t.Errorf("path = %q, want %q", req.URL.Path, loginPath)
	}
	if req.URL.RawQuery != "" {
		t.Errorf("URL has a query string %q — password/params must be in the BODY, not the URL", req.URL.RawQuery)
	}
	if strings.Contains(req.URL.String(), "s3cr3t") {
		t.Errorf("password leaked into the request URL: %q", req.URL.String())
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", ct)
	}
	if ua := req.Header.Get("User-Agent"); ua != UserAgent {
		t.Errorf("user-agent = %q, want %q", ua, UserAgent)
	}
	// Body carries r=login2 + creds.
	body := make([]byte, 4096)
	n, _ := req.Body.Read(body)
	form, perr := url.ParseQuery(string(body[:n]))
	if perr != nil {
		t.Fatalf("parse body: %v", perr)
	}
	if form.Get("r") != "login2" {
		t.Errorf("body r = %q, want login2", form.Get("r"))
	}
	if form.Get("u") != "Bob" {
		t.Errorf("body u = %q, want Bob", form.Get("u"))
	}
	if form.Get("p") != "s3cr3t-p@ss" {
		t.Errorf("body p mismatch: %q", form.Get("p"))
	}
}

// TestLoginSuccess: a reachable server returning Success:true yields the token.
func TestLoginSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server-side: the password must arrive in the form body, not the query.
		if r.URL.RawQuery != "" {
			t.Errorf("server saw a query string %q — must be empty (creds in body)", r.URL.RawQuery)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("server ParseForm: %v", err)
		}
		if r.PostForm.Get("p") != "pw" || r.PostForm.Get("u") != "Bob" {
			t.Errorf("server got u=%q p=%q", r.PostForm.Get("u"), r.PostForm.Get("p"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Success":true,"User":"Bob","Token":"TOKEN123","Score":10}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second)
	resp, err := c.Login("Bob", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !resp.Success || resp.Token != "TOKEN123" || resp.User != "Bob" {
		t.Errorf("got %+v", resp)
	}
}

// TestLoginBadCredentials: a reachable server rejecting the creds returns
// Success:false with a nil error (NOT a transport error), so the caller treats it
// as an auth failure (exit 4), not unreachable (exit 3).
func TestLoginBadCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"Success":false,"Status":401,"Code":"invalid_credentials","Error":"Invalid user/password combination."}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second)
	resp, err := c.Login("Bob", "wrong")
	if err != nil {
		t.Fatalf("Login returned transport error for a reachable rejection: %v", err)
	}
	if resp.Success {
		t.Error("Success should be false")
	}
	if resp.Code != "invalid_credentials" {
		t.Errorf("Code = %q", resp.Code)
	}
	if resp.Token != "" {
		t.Error("no token should be present on a rejected login")
	}
}

// TestLoginNonJSON: an HTML/WAF page yields an error whose message does NOT echo
// the body (no leak of server-returned content).
func TestLoginNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>blocked by SECRETWAF token=leak</body></html>"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second)
	_, err := c.Login("Bob", "pw")
	if err == nil {
		t.Fatal("expected an error for a non-JSON body")
	}
	if strings.Contains(err.Error(), "SECRETWAF") || strings.Contains(err.Error(), "leak") {
		t.Errorf("error message echoed the response body: %q", err.Error())
	}
}

// TestNewClientDefaultsBaseURL: empty baseURL selects the public RA origin.
func TestNewClientDefaultsBaseURL(t *testing.T) {
	c := NewClient("", time.Second)
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
}
