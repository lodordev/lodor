// Package config loads the Lodor engine's config.json (CWD-relative) into typed
// structs. The schema mirrors BLUEPRINT §7: a list of RomM hosts, per-platform
// directory mappings, and two integer-second timeouts.
//
// CGO-free, stdlib only.
package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Host describes one RomM server and the credentials used to reach it. Auth is
// either a client-token bearer (preferred) or HTTP Basic; URL() and AuthHeader()
// derive the wire values the romm client needs.
type Host struct {
	RootURI            string `json:"root_uri"`
	Port               int    `json:"port,omitempty"`
	Username           string `json:"username,omitempty"`
	Password           string `json:"password,omitempty"`
	Token              string `json:"token,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
	DeviceID           string `json:"device_id,omitempty"`
	DeviceName         string `json:"device_name,omitempty"`

	// Token metadata captured at pairing. Not used to authenticate (the bearer is
	// Token); these feed expiry warnings and the per-device revoke runbook. Optional
	// and omitempty so older configs (and the launcher) stay backward compatible.
	TokenName      string   `json:"token_name,omitempty"`
	TokenExpiresAt string   `json:"token_expires_at,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
}

// URL returns the base URL for the host: root_uri with ":port" inserted after the
// host (before any subpath) when a port is set, and any trailing slash trimmed so
// path concatenation is clean. root_uri may carry a subpath (e.g.
// "https://example.com/romm"); the port must land on the authority, NOT the tail —
// a naive "%s:%d" yields the malformed "example.com/romm:8443". We parse the URI and
// rebuild scheme://host:port + path. No-port and no-subpath cases round-trip
// byte-identically to the old behavior.
func (h Host) URL() string {
	base := strings.TrimSuffix(h.RootURI, "/")
	if h.Port == 0 {
		return base
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		// Unparseable or authority-less root_uri: fall back to the legacy append so a
		// weird value still produces *something* rather than dropping the port. (The
		// onboarding --set-server gate rejects such URIs up front, so this is defensive.)
		return strings.TrimSuffix(fmt.Sprintf("%s:%d", h.RootURI, h.Port), "/")
	}
	// Set the port on the authority only; url.Host is "host" (or "host:oldport") — strip
	// any existing port first, then attach ours, leaving u.Path (the subpath) untouched.
	hostOnly := u.Hostname()
	u.Host = fmt.Sprintf("%s:%d", hostOnly, h.Port)
	return strings.TrimSuffix(u.String(), "/")
}

// AuthHeader returns the Authorization header value: a bearer token when one is
// configured, otherwise HTTP Basic from username:password.
func (h Host) AuthHeader() string {
	if h.Token != "" {
		return "Bearer " + h.Token
	}
	creds := h.Username + ":" + h.Password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

// DirMapping overrides where a RomM platform's ROMs live on the card: an optional
// slug override and a relative folder under the ROM root.
type DirMapping struct {
	Slug         string `json:"slug,omitempty"`
	RelativePath string `json:"relative_path,omitempty"`
}

// Config is the parsed config.json. ApiTimeout and DownloadTimeout are stored in
// whole seconds; see Seconds-typed unmarshal for the legacy nanosecond fallback.
type Config struct {
	Hosts             []Host                `json:"hosts,omitempty"`
	DirectoryMappings map[string]DirMapping `json:"directory_mappings,omitempty"`
	ApiTimeout        Seconds               `json:"api_timeout"`
	DownloadTimeout   Seconds               `json:"download_timeout"`

	// FetchCovers gates ONLY the BULK box-art fetch during --mirror-catalog. It is a
	// *bool whose DEFAULT is OFF (opt-in): an absent key (older configs, the
	// onboarding writer) leaves it nil, which CoversEnabled() reads as FALSE, so a
	// plain --mirror-catalog stubs the library WITHOUT pulling thousands of covers
	// (≈0.7 GB + thousands of FAT32 files over flaky wifi, and the cover only shows
	// one-game-at-a-time in Details). Set "fetch_covers": true to opt INTO bulk
	// cover-fetch on mirror for users who want all art up front. Note: --download
	// ALWAYS fetches the downloaded game's own cover regardless of this toggle, so a
	// downloaded game's Details view shows art even with bulk off.
	FetchCovers *bool `json:"fetch_covers,omitempty"`
}

// CoversEnabled reports whether the BULK cover fetch on --mirror-catalog is on.
// Default OFF (opt-in): a nil FetchCovers (absent key) is treated as false; only an
// explicit "fetch_covers": true enables bulk-on-mirror. This does NOT govern the
// --download per-game cover fetch, which is unconditional.
func (c *Config) CoversEnabled() bool {
	if c == nil || c.FetchCovers == nil {
		return false
	}
	return *c.FetchCovers
}

// Seconds is a duration expressed as whole seconds in JSON. To stay compatible
// with old configs that stored nanoseconds, any value greater than 1e9 on read is
// treated as already being a nanosecond count rather than a second count.
type Seconds int64

const nsThreshold = int64(1_000_000_000) // 1e9

// UnmarshalJSON reads an integer. Values over 1e9 are interpreted as nanoseconds
// (legacy configs, e.g. 1800000000000 = 30min) and converted down to seconds;
// smaller values are taken as seconds directly.
func (s *Seconds) UnmarshalJSON(b []byte) error {
	var raw int64
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw > nsThreshold {
		raw = raw / nsThreshold
	}
	*s = Seconds(raw)
	return nil
}

// MarshalJSON writes the value back out as whole seconds.
func (s Seconds) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(s))
}

// Int returns the timeout in seconds as a plain int.
func (s Seconds) Int() int { return int(s) }

// Load reads ./config.json relative to the current working directory, parses it,
// and applies defaults: ApiTimeout 30s (clamped to 300s max), DownloadTimeout
// 3600s.
func Load() (*Config, error) {
	data, err := os.ReadFile(configFileName)
	if err != nil {
		return nil, fmt.Errorf("reading config.json: %w", err)
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config.json: %w", err)
	}

	if c.ApiTimeout == 0 {
		c.ApiTimeout = 30
	}
	if c.ApiTimeout > 300 {
		c.ApiTimeout = 30
	}
	if c.DownloadTimeout == 0 {
		c.DownloadTimeout = 3600
	}

	return &c, nil
}
