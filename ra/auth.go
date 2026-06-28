// Package ra is a from-scratch, CGO-free client for the RetroAchievements (RA)
// login API. It exchanges an RA username + password for the long-lived account
// TOKEN that rc_client authenticates with (rc_client_begin_login_with_token), so
// the password is NEVER persisted — only {username, token} land in the Lodor
// config (config.WriteRACredentials).
//
// The endpoint is RA's public Connect API dorequest.php (r=login2). That host is
// NOT the RomM server and carries no Lodor secret, so its base URL is a plain
// default constant (overridable so tests can point at httptest). Stdlib only,
// CGO-free.
package ra

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is RetroAchievements' public Connect API origin. Not a secret
// (unlike the RomM host), so it may appear in code and config. Overridable in tests.
const DefaultBaseURL = "https://retroachievements.org"

// loginPath is the Connect API endpoint; r=login2 trades user+password for a token.
const loginPath = "/dorequest.php"

// UserAgent identifies LodorOS to the RA server for version negotiation (RA gates
// client capabilities on the UA; format <product>/<version> per the rc_client
// integration guide). The minarch fork's rc_client server_call sends the same UA so
// the engine login and the in-game client present one identity. Bump on releases.
const UserAgent = "LodorOS/0.1"

// LoginResponse is the subset of the r=login2 reply Lodor consumes. On success
// Success is true and Token holds the long-lived account token; on failure Success
// is false and Code/Error explain why (e.g. Code "invalid_credentials"). Status is
// RA's in-body status code (e.g. 401), distinct from the HTTP status.
type LoginResponse struct {
	Success bool   `json:"Success"`
	User    string `json:"User"`
	Token   string `json:"Token"`
	Status  int    `json:"Status"`
	Code    string `json:"Code"`
	Error   string `json:"Error"`
}

// Client posts to the RA login API. baseURL defaults to DefaultBaseURL; tests point
// it at an httptest server. timeout bounds the single login round-trip.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient builds an RA login client. An empty baseURL selects DefaultBaseURL.
func NewClient(baseURL string, timeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{Timeout: timeout},
	}
}

// buildLoginRequest constructs the r=login2 POST. The password rides in the
// x-www-form-urlencoded BODY, never the query string / URL — a GET query would
// leak the password into proxy and server access logs. This is the security-load-
// bearing part and is unit-tested without a socket.
func (c *Client) buildLoginRequest(username, password string) (*http.Request, error) {
	form := url.Values{}
	form.Set("r", "login2")
	form.Set("u", username)
	form.Set("p", password)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+loginPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", UserAgent)
	return req, nil
}

// Login posts username+password to r=login2 and returns the parsed reply. A
// transport failure returns a non-nil error (the caller scrubs it host-free). A
// reachable server that rejects the credentials returns (LoginResponse{Success:
// false}, nil) so the caller can distinguish "bad password" from "network down".
// RA returns its real signal in the JSON Success field regardless of HTTP status,
// so the body is parsed even on a non-2xx; the body is never echoed on a parse
// failure (it can be an HTML WAF page).
func (c *Client) Login(username, password string) (LoginResponse, error) {
	req, err := c.buildLoginRequest(username, password)
	if err != nil {
		return LoginResponse{}, fmt.Errorf("build login request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return LoginResponse{}, fmt.Errorf("execute login request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return LoginResponse{}, fmt.Errorf("read login response: %w", err)
	}
	var out LoginResponse
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		// Non-JSON (HTML error page, WAF block, captcha): surface a generic,
		// host-free error; NEVER echo the body (it may contain request context).
		return LoginResponse{}, fmt.Errorf("login response not JSON (http %d)", resp.StatusCode)
	}
	return out, nil
}
