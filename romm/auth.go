package romm

// Onboarding / pairing primitives: the RomM client-token exchange, token and
// connection validation, the current-user lookup, and the scope check the wizard
// warns on. The credential is a RomM CLIENT-TOKEN (never an account password):
// a short code minted in the RomM web UI is exchanged here for a scoped, revocable
// bearer token (BLUEPRINT §1; lodor-onboarding-design.md). Stdlib only, CGO-free.

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lodor/config"
)

// PasswordGrantScopes are the scopes a multi-user profile login requests — the full
// sync set (saves + devices + library reads). RomM grants what the user's role allows
// (a viewer gets write scopes when KIOSK_MODE is off), so requesting broadly is safe.
var PasswordGrantScopes = []string{
	"me.read", "me.write", "roms.read", "roms.user.read", "roms.user.write",
	"assets.read", "assets.write", "collections.read", "collections.write",
	"platforms.read", "firmware.read", "devices.read", "devices.write", "users.read",
}

// PasswordGrant performs an OAuth2 password grant against RomM (POST /api/token,
// form-encoded grant_type=password) and returns the bearer token. Used by the
// multi-user "Add Profile" flow to sign IN as an existing RomM user (NOT create an
// account). The password is used once here and NEVER stored (token-only at rest).
// baseURL is the host root_uri (port already appended); insecure mirrors the host TLS.
func PasswordGrant(baseURL, username, password string, insecure bool) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", username)
	form.Set("password", password)
	form.Set("scope", strings.Join(PasswordGrantScopes, " "))

	req, err := http.NewRequest("POST", strings.TrimRight(baseURL, "/")+"/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Go-http-client/1.1")

	tr := &http.Transport{}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	// SECURITY: this request carries the OAuth password-grant form (username/password),
	// which Go would REPLAY to a redirect target. Refuse cross-host and scheme-downgrade
	// redirects so credentials never leave the origin — the same policy the bearer /
	// CF-Access clients use.
	hc := &http.Client{Timeout: 30 * time.Second, Transport: tr, CheckRedirect: secureCheckRedirect}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		// 401 = bad credentials; surface a clean, secret-free message.
		return "", fmt.Errorf("login rejected (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return "", fmt.Errorf("login: bad response")
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("login: empty token")
	}
	return out.AccessToken, nil
}

// TokenExchangeRequest is the body of POST /api/client-tokens/exchange.
type TokenExchangeRequest struct {
	Code string `json:"code"`
}

// TokenExchangeResponse is the server's reply to a client-token exchange: the raw
// bearer token to store, plus the token's name, granted scopes, and expiry — the
// latter two feed the scope warning and the revoke/expiry runbook.
type TokenExchangeResponse struct {
	RawToken  string   `json:"raw_token"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expires_at"`
}

// CurrentUser is the subset of GET /api/users/me used to auto-fill the username
// after pairing.
type CurrentUser struct {
	Username string `json:"username"`
}

// SyncRequiredScopes are the client-token scopes save sync needs end to end:
// reading/writing assets (saves) and reading/writing devices (registration,
// rename). A token missing any of these will 403 on the corresponding endpoints.
// These are the MINIMAL scopes the wizard asks for — never admin (security req #2).
var SyncRequiredScopes = []string{"assets.read", "assets.write", "devices.read", "devices.write"}

// MissingSyncScopes returns the SyncRequiredScopes not present in have. Advisory:
// RomM may model scopes more broadly, so treat a non-empty result as a likely (not
// certain) cause of sync permission failures — the wizard warns, it does not block.
func MissingSyncScopes(have []string) []string {
	present := make(map[string]bool, len(have))
	for _, s := range have {
		present[s] = true
	}
	var missing []string
	for _, s := range SyncRequiredScopes {
		if !present[s] {
			missing = append(missing, s)
		}
	}
	return missing
}

// ExchangeToken trades a pairing code for a client-token bearer via
// POST /api/client-tokens/exchange (body {"code": ...}). It runs PRE-token, using
// only the base URL and the TLS-skip flag — no credentials are needed or sent, the
// code itself is the bearer of trust. baseURL is the host's root_uri (with any
// :port already appended); insecureSkipVerify mirrors the host's HTTPS setting.
func ExchangeToken(host config.Host, code string) (TokenExchangeResponse, error) {
	// Pre-token: clear stored creds so no real Authorization is sent (the code in the
	// body is the bearer of trust), but PRESERVE the transport route — socks5_proxy,
	// tier, port, TLS-skip — so the exchange reaches a Tailscale-served RomM THROUGH the
	// SOCKS5 proxy exactly like the authed client. Dropping socks5_proxy here was the
	// pairing-over-tailnet bug: the exchange dialed the .ts.net name directly, could not
	// resolve it, and never reached RomM (heartbeat worked, the claim POST never landed).
	host.Token = ""
	host.Password = ""
	host.Username = ""
	c := NewClient(host, 30*time.Second)
	var out TokenExchangeResponse
	err := c.doJSON("POST", "/api/client-tokens/exchange", TokenExchangeRequest{Code: code}, &out)
	return out, err
}

// ValidateToken makes a cheap authenticated GET (the platform list) that fails if
// the configured token/credentials are bad. A nil error means the token is good.
func (c *Client) ValidateToken() error {
	var platforms []Platform
	return c.doJSON("GET", "/api/platforms", nil, &platforms)
}

// GetCurrentUser returns the authenticated user (GET /api/users/me), used to
// auto-fill the username after a successful pairing.
func (c *Client) GetCurrentUser() (CurrentUser, error) {
	var user CurrentUser
	err := c.doJSON("GET", "/api/users/me", nil, &user)
	return user, err
}

// User is the subset of GET /api/users the MULTI-USER "Switch User" picker consumes:
// the username to switch/sign-in to, the role for display, and whether the account is
// enabled. Listing requires a token with users.read — the admin/onboarding token
// (Hosts[0]) carries it; a viewer profile token does not (hence the picker uses
// Hosts[0], not ActiveHost).
type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Enabled  bool   `json:"enabled"`
}

// GetUsers lists the RomM server's users (GET /api/users), for the Switch-User picker.
// The live server returns a bare JSON array; a future server that wraps it in
// {"items":[...]} is also accepted. Requires users.read on the bearer (admin token).
func (c *Client) GetUsers() ([]User, error) {
	raw, err := c.doRaw("GET", "/api/users")
	if err != nil {
		return nil, err
	}
	var users []User
	if jerr := json.Unmarshal(raw, &users); jerr == nil {
		return users, nil
	}
	var wrapped struct {
		Items []User `json:"items"`
	}
	if jerr := json.Unmarshal(raw, &wrapped); jerr == nil {
		return wrapped.Items, nil
	}
	return nil, fmt.Errorf("users: bad response")
}

// Device is the subset of a RomM device record the onboarding flow consumes. The
// server keys saves and "which device" columns on ID; Name is the human label.
type Device struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Platform      string    `json:"platform"`
	Client        string    `json:"client"`
	ClientVersion string    `json:"client_version"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// RegisterDeviceRequest is the body of POST /api/devices. Name is required at
// pairing (an empty device_name blanks the cross-device "which device" column).
type RegisterDeviceRequest struct {
	Name          string `json:"name"`
	Platform      string `json:"platform"`
	Client        string `json:"client"`
	ClientVersion string `json:"client_version"`
	SyncMode      string `json:"sync_mode,omitempty"`
}

// UpdateDeviceRequest is the body of PUT /api/devices/{id}; only set fields are
// sent (used here to rename a device).
type UpdateDeviceRequest struct {
	Name          string `json:"name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
}

// registerDeviceResponse is the server's POST /api/devices reply. The live server
// returns the new id under "device_id" (not "id"); accept both so a future server
// that returns "id" still parses.
type registerDeviceResponse struct {
	DeviceID  string    `json:"device_id"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func (r registerDeviceResponse) device() Device {
	id := r.DeviceID
	if id == "" {
		id = r.ID
	}
	return Device{ID: id, Name: r.Name, CreatedAt: r.CreatedAt}
}

// RegisterDevice creates (or returns the existing) device record for this client
// via POST /api/devices, returning the device with its server-assigned ID. name is
// the required human label. The server registers with minimal client metadata; the
// engine sets sync_mode=api (headless, no negotiate session).
func (c *Client) RegisterDevice(name string) (Device, error) {
	var resp registerDeviceResponse
	err := c.doJSON("POST", "/api/devices", RegisterDeviceRequest{
		Name:     name,
		Platform: "MINUI",
		Client:   "lodor",
		SyncMode: "api",
	}, &resp)
	if err != nil {
		return Device{}, err
	}
	return resp.device(), nil
}

// GetDevices lists the account's devices (GET /api/devices). Non-mutating; used to
// confirm the device wire shape without creating records.
func (c *Client) GetDevices() ([]Device, error) {
	var devices []Device
	err := c.doJSON("GET", "/api/devices", nil, &devices)
	return devices, err
}

// UpdateDevice renames an existing device via PUT /api/devices/{id}, returning the
// updated record.
func (c *Client) UpdateDevice(deviceID, name string) (Device, error) {
	var device Device
	err := c.doJSON("PUT", "/api/devices/"+deviceID, UpdateDeviceRequest{Name: name}, &device)
	return device, err
}
