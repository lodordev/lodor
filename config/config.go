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

// ActiveProfileName returns the name of the currently selected profile: the
// LODOR_PROFILE env (the launcher exports it on a switch) wins over the config's
// active_profile field. Returns "" when no profile is selected (single-user card),
// in which case ActiveHost is hosts[0] verbatim. Trimmed; case preserved (labels are
// user-facing). Nil-safe.
func (c *Config) ActiveProfileName() string {
	if env := strings.TrimSpace(os.Getenv("LODOR_PROFILE")); env != "" && !strings.EqualFold(env, "default") {
		return env
	}
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.ActiveProfile)
}

// ActiveProfileIndex returns the index of the active profile in Profiles, matched by
// Label (case-insensitive, trimmed), or -1 when none is selected/found. Nil-safe.
func (c *Config) ActiveProfileIndex() int {
	name := c.ActiveProfileName()
	if name == "" || c == nil {
		return -1
	}
	for i := range c.Profiles {
		if strings.EqualFold(strings.TrimSpace(c.Profiles[i].Label), name) {
			return i
		}
	}
	return -1
}

// ActiveProfile returns a pointer to the selected Profile, or nil when none is
// selected/resolvable. Nil-safe.
func (c *Config) ActiveProfilePtr() *Profile {
	i := c.ActiveProfileIndex()
	if i < 0 {
		return nil
	}
	return &c.Profiles[i]
}

// ActiveHost returns the host the engine should authenticate AND sync as. It starts
// from hosts[0] (the shared family server: root_uri/port/insecure/timeouts) and, when
// a profile is selected and resolvable, OVERLAYS that profile's per-user identity —
// token, device_id, device_name, username, and token metadata. The shared server
// address is NEVER taken from the profile. When no profile is selected (single-user
// card, the historical case) this is hosts[0] byte-identical, so existing configs are
// unaffected. Caller must have checked len(Hosts) > 0 (same precondition as Hosts[0]).
//
// SECURITY: returns a value; never logs the token/device_id (callers already scrub).
func (c *Config) ActiveHost() Host {
	host := c.Hosts[0]
	p := c.ActiveProfilePtr()
	if p == nil {
		return host
	}
	if p.Token != "" {
		host.Token = p.Token
		host.Password = "" // token-only at rest: a profile bearer never authenticates with a stale password
	}
	if p.DeviceID != "" {
		host.DeviceID = p.DeviceID
	}
	if p.DeviceName != "" {
		host.DeviceName = p.DeviceName
	}
	if p.Username != "" {
		host.Username = p.Username
	}
	if p.TokenName != "" {
		host.TokenName = p.TokenName
	}
	if p.TokenExpiresAt != "" {
		host.TokenExpiresAt = p.TokenExpiresAt
	}
	if len(p.Scopes) > 0 {
		host.Scopes = p.Scopes
	}
	return host
}

// Profile is one family member's per-user identity on the SHARED family server
// (multi-user, approach #1). hosts[0] holds the shared server address (root_uri/
// port/insecure); a Profile layers that user's OWN client token + server-registered
// device_id on top so saves/states/Continue sync under THEIR account. Label is the
// display name (its first letter drives the corner badge); Username is the RomM
// account handle (best-effort, for display). A profile is selected by name via
// active_profile (config) or the LODOR_PROFILE env (the launcher exports it on a
// switch); ActiveHost() merges the selected profile onto hosts[0].
//
// SECURITY: Token is a secret at rest exactly like Host.Token — never logged/printed.
type Profile struct {
	Label      string `json:"label"`
	Token      string `json:"token,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	Username   string `json:"username,omitempty"`

	// Token metadata captured at pairing, same role as Host's (expiry warnings /
	// per-device revoke). Optional; omitempty keeps older configs clean.
	TokenName      string   `json:"token_name,omitempty"`
	TokenExpiresAt string   `json:"token_expires_at,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
}

// DirMapping overrides where a RomM platform's ROMs live on the card: an optional
// slug override and a relative folder under the ROM root.
type DirMapping struct {
	Slug         string `json:"slug,omitempty"`
	RelativePath string `json:"relative_path,omitempty"`
}

// Mirror modes govern HOW the RomM library is laid down on the card relative to a
// user's own existing games (BLUEPRINT §coexist / issue #68).
//
// "own" is the LodorOS default: the card IS the RomM library — folders are named
// "<Display> (<TAG>)" and stub basenames are byte-identical to the server's (the
// historical, only behavior). "separate" is the NextUI default: RomM is mirrored into
// distinct "<Display> RomM (<TAG>)" folders that bind the right emulator yet never
// collide with the user's own "<Display> (<TAG>)" folders, and stub basenames are
// disambiguated so a RomM stub never shares a save file with a user's local game — the
// user's existing folders and games are NEVER touched. "merge" (adopt-folder-by-tag +
// filename-normalize dedup + scoped prune + saves-off-until-opt-in) is NOT YET
// IMPLEMENTED and is treated as "separate" (safe, non-destructive) until the #68 design
// lands; see the TODO in catalog/generate.go.
const (
	MirrorModeOwn      = "own"
	MirrorModeSeparate = "separate"
	MirrorModeMerge    = "merge"
)

// Config is the parsed config.json. ApiTimeout and DownloadTimeout are stored in
// whole seconds; see Seconds-typed unmarshal for the legacy nanosecond fallback.
type Config struct {
	Hosts             []Host                `json:"hosts,omitempty"`
	// Profiles are the per-user identities sharing hosts[0] (multi-user). Empty/absent
	// on a single-user card — every existing single-account config stays byte-identical
	// (ActiveHost falls back to hosts[0] verbatim). ActiveProfile names the selected one;
	// the LODOR_PROFILE env overrides it at runtime when the launcher switches.
	Profiles      []Profile `json:"profiles,omitempty"`
	ActiveProfile string    `json:"active_profile,omitempty"`
	DirectoryMappings map[string]DirMapping `json:"directory_mappings,omitempty"`
	ApiTimeout        Seconds               `json:"api_timeout"`
	DownloadTimeout   Seconds               `json:"download_timeout"`

	// MirrorMode is the coexist mode (own|separate|merge). Written by the pak's
	// settings toggle. Absent/unrecognized => ResolvedMirrorMode() picks a host-aware
	// default (own on LodorOS, separate elsewhere), so an older config stays own and a
	// NextUI card defaults to the non-destructive separate layout.
	MirrorMode string `json:"mirror_mode,omitempty"`

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

	// RAUsername / RAToken are the RetroAchievements credential spine (task #46):
	// the RA account handle and its long-lived login token, stored TOP-LEVEL (not
	// under a host) because RA is account-global, independent of any RomM host. The
	// RA PASSWORD is NEVER stored — only the token, mirroring the token-only-at-rest
	// rule for the RomM bearer. Written by `lodor-sync --ra-login`; read by the menu
	// (--ra-status) and, on LodorOS, by the minarch fork's vendored rc_client.
	RAUsername string `json:"ra_username,omitempty"`
	RAToken    string `json:"ra_token,omitempty"`
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

// RALoggedIn reports whether a usable RetroAchievements credential pair (username +
// token) is stored. The menu surfaces "Logged in as <user>" off this; the minarch
// fork skips rc_client login when it is false. Nil-safe.
func (c *Config) RALoggedIn() bool {
	return c != nil && c.RAUsername != "" && c.RAToken != ""
}

// ResolvedMirrorMode returns the effective coexist mode. An explicit, recognized
// mirror_mode (case/space-insensitive) always wins. When absent or unrecognized it
// falls back to the host default: "own" on a LodorOS host (byte-identical to today),
// "separate" on any other host so RomM games land in distinct, non-colliding folders.
// Nil-safe.
func (c *Config) ResolvedMirrorMode() string {
	if c != nil {
		switch strings.ToLower(strings.TrimSpace(c.MirrorMode)) {
		case MirrorModeOwn:
			return MirrorModeOwn
		case MirrorModeSeparate:
			return MirrorModeSeparate
		case MirrorModeMerge:
			return MirrorModeMerge
		}
	}
	return hostMirrorModeDefault()
}

// hostMirrorModeDefault picks the mirror mode for a config that carries no explicit
// mirror_mode. The host is identified by the LODOR_HOST_OS env the launcher exports:
// "nextui" (or any other recognized non-LodorOS host) => separate; "lodoros"/"minui"
// => own. When the hint is ABSENT we assume LodorOS and return "own", so existing
// LodorOS cards — which export nothing new — stay byte-identical to today, while the
// NextUI Lodor.pak (which writes mirror_mode explicitly AND exports LODOR_HOST_OS=
// nextui) gets separate. An explicitly-set but unrecognized host is, by definition,
// not LodorOS, so it defaults separate.
func hostMirrorModeDefault() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LODOR_HOST_OS"))) {
	case "", "lodoros", "lodor", "minui":
		return MirrorModeOwn
	case "nextui":
		return MirrorModeOwn // RomM-first: merge in place, no " (RomM)" disambiguator (cut 2026-06-28)
	default:
		return MirrorModeOwn
	}
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

	// Overlay the pak's UI-owned toggles from settings.conf (same CWD as config.json).
	// The launcher writes mirror_mode / fetch_covers there — NOT into the token-bearing
	// config.json — so a UI toggle can never corrupt the RomM credentials. Present keys
	// win over config.json (the toggle is the user's live choice).
	applySettingsConf(&c)

	return &c, nil
}

// settingsFileName is the pak's UI-toggle file, read CWD-relative beside config.json.
const settingsFileName = "settings.conf"

// applySettingsConf overlays the launcher's key=value UI toggles onto the loaded config.
// Only the UI-owned keys are honored (mirror_mode, fetch_covers); everything else in the
// file is ignored. A missing file is a no-op. This is the single coordination point with
// the host launcher's settings.conf writer (Lodor.pak launch.sh).
func applySettingsConf(c *Config) {
	data, err := os.ReadFile(settingsFileName)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "mirror_mode":
			// ResolvedMirrorMode validates/host-defaults an unrecognized value.
			c.MirrorMode = v
		case "fetch_covers":
			b := settingTruthy(v)
			c.FetchCovers = &b
		}
	}
}

// settingTruthy reads a settings.conf boolean (on/true/1/yes => true; anything else false).
func settingTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "1", "yes", "y":
		return true
	}
	return false
}
