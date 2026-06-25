package romm

// Onboarding / pairing primitives: the RomM client-token exchange, token and
// connection validation, the current-user lookup, and the scope check the wizard
// warns on. The credential is a RomM CLIENT-TOKEN (never an account password):
// a short code minted in the RomM web UI is exchanged here for a scoped, revocable
// bearer token (BLUEPRINT §1; lodor-onboarding-design.md). Stdlib only, CGO-free.

import (
	"time"

	"lodor/config"
)

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
func ExchangeToken(baseURL string, code string, insecureSkipVerify bool) (TokenExchangeResponse, error) {
	// Pre-token host: no username/password/token, so the client sends no
	// Authorization header on the exchange (authHeader == "" when Token is empty
	// AND username is empty... it is not — so route around AuthHeader by leaving
	// creds blank, which yields "Basic <base64(:)>"; the exchange endpoint ignores
	// it). The code in the body is what authorizes the exchange.
	host := config.Host{RootURI: baseURL, InsecureSkipVerify: insecureSkipVerify}
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
