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
	"net"
	"net/url"
	"os"
	"strconv"
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

	// ProfileLabel is the MULTI-USER profile this host belongs to (e.g. "Player One").
	// Empty on a single-user card. active-profile.txt names which label is live;
	// ActiveHost() resolves it. Each profile = its own host (same server, that user's
	// token), so per-user save namespacing + auth just work off the active host.
	ProfileLabel string `json:"profile_label,omitempty"`

	// Tier is the endpoint-preference rank for two-tier external access (#87):
	// LOWER wins. Tier 1 = the Tailscale internal URL (PREFERRED — traffic never
	// leaves the tailnet, origin never exposed); tier 2 = the public Cloudflare
	// Access URL (FALLBACK for devices that can't run Tailscale). Absent/0 on a
	// legacy single-endpoint config, which selects hosts[0] unconditionally
	// (ResolveHost stays byte-identical to ActiveHost there). The two tiers point
	// at the SAME RomM, so they share one identity (token/device).
	Tier int `json:"tier,omitempty"`

	// CFAccess carries THIS endpoint's Cloudflare Access service-token pair. Set
	// ONLY on a tier-2 (public) host; a tier-1 (Tailscale) host has none. When
	// present the romm client sends the two CF-Access-Client-* headers on every
	// request to this host, IN ADDITION to RomM's own Authorization bearer (the
	// two are orthogonal — Cloudflare consumes its headers at the edge and never
	// touches Authorization). omitempty keeps it out of single-tier configs.
	CFAccess *CFAccess `json:"cf_access,omitempty"`

	// Socks5Proxy routes THIS endpoint's traffic through a local SOCKS5 proxy with
	// REMOTE DNS (socks5h): the romm client dials "host:port" and connects to the
	// proxy, which resolves the destination NAME itself. Set ONLY on a tier-1
	// (Tailscale) host whose root_uri is a *.ts.net MagicDNS name — the userspace
	// tailscaled started by the pak exposes a SOCKS5 server (default
	// "localhost:1055") that both resolves the MagicDNS name and routes the
	// connection over the tailnet, so NO system resolver / TUN / resolvconf is
	// needed (#84). Empty on a tier-2 (public Cloudflare) host, which dials direct,
	// so the fallback path is byte-identical to before this field existed. Like
	// cf_access/tier/root_uri this is an ENDPOINT field: ResolveHost never inherits
	// it from hosts[0] and no profile overlays it.
	Socks5Proxy string `json:"socks5_proxy,omitempty"`
}

// CFAccess is a Cloudflare Access service-token pair used to authenticate a
// browserless client (the engine) to a RomM endpoint published behind Cloudflare
// Access (tier 2 — the public-internet fallback path). It is sent as the
// CF-Access-Client-Id / CF-Access-Client-Secret request headers. The Client ID
// keeps its ".access" suffix verbatim. Defense in depth: BOTH this service token
// AND RomM's own bearer are required end to end.
//
// SECURITY: ClientSecret is a secret at rest exactly like a token — never
// logged, printed, or written to any sample/wiki file (samples use placeholders).
type CFAccess struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
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

// cgnatNet is the Tailscale CGNAT range (100.64.0.0/10). A RomM host entered as a bare
// 100.x IP is a tailnet peer (WireGuard already authenticates+encrypts it), so the
// redundant TLS cert (issued for the *.ts.net name, never the IP) is skipped for it.
var cgnatNet = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

// SkipTLSVerify reports whether HTTPS cert verification should be skipped for this host:
// the explicit insecure_skip_verify flag, OR an automatic skip when the host is a bare
// Tailscale CGNAT IP reached over the tunnel (a cert can never match a bare IP).
func (h Host) SkipTLSVerify() bool {
	if h.InsecureSkipVerify {
		return true
	}
	u, err := url.Parse(h.URL())
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && cgnatNet.Contains(ip)
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

// activeProfileFileName is the launcher's active-profile.txt, read CWD-relative (the
// engine runs CWD = the pak dir, like config.json). One line = the active profile label.
const activeProfileFileName = "active-profile.txt"

// ActiveProfileLabel returns the live multi-user profile label from active-profile.txt
// (CWD-relative), trimmed. "" when absent/empty (single-user).
func ActiveProfileLabel() string {
	b, err := os.ReadFile(activeProfileFileName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ActiveHost returns the host for the active multi-user profile (the Host whose
// ProfileLabel matches active-profile.txt). Falls back to Hosts[0] when there is no
// active profile, no match, or "default" — so a single-user card behaves EXACTLY as
// before. Returns a zero Host only when Hosts is empty (caller already gates on that).
func (c *Config) ActiveHost() Host {
	if len(c.Hosts) == 0 {
		return Host{}
	}
	label := ActiveProfileLabel()
	if label != "" && !strings.EqualFold(label, "default") {
		for _, h := range c.Hosts {
			if h.ProfileLabel != "" && strings.EqualFold(h.ProfileLabel, label) {
				return h
			}
		}
	}
	return c.Hosts[0]
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
// historical, only behavior). "merge" is the NextUI default (C2, 2026-07-02): RomM
// games mix into the user's OWN folders — existing same-tag folders are ADOPTED
// (adopt-by-tag), a user's exact-named file is never stubbed (dedup-by-index-adoption:
// the index adopts THEIR path so save-sync attaches to their file), stub basenames are
// canonical under the ✘/✓ markers, and every destructive path is scoped to the
// mirror-owned manifest so the user's files are structurally untouchable. "separate"
// keeps RomM in distinct "<Display> RomM (<TAG>)" folders with " (RomM)"-suffixed
// basenames — the quarantine/trial mode, and the home for same-name-different-content
// collections merge's dedup would hide.
const (
	MirrorModeOwn      = "own"
	MirrorModeSeparate = "separate"
	MirrorModeMerge    = "merge"
)

// maxDownloadTimeout is the upper bound clamp for DownloadTimeout (seconds). Even a
// large multi-disc download finishes well inside 24h; a value above this is a typo or
// a stray nanosecond legacy value that slipped the Seconds fallback, and an unbounded
// timeout would let a wedged transfer hang the sync forever.
const maxDownloadTimeout Seconds = 86400 // 24h

// Config is the parsed config.json. ApiTimeout and DownloadTimeout are stored in
// whole seconds; see Seconds-typed unmarshal for the legacy nanosecond fallback.
type Config struct {
	Hosts             []Host                `json:"hosts,omitempty"`
	DirectoryMappings map[string]DirMapping `json:"directory_mappings,omitempty"`
	ApiTimeout        Seconds               `json:"api_timeout"`
	DownloadTimeout   Seconds               `json:"download_timeout"`

	// MirrorMode is the coexist mode (own|separate|merge). Written by the pak's
	// settings toggle (and by NextUI onboarding, which writes merge explicitly).
	// Absent/unrecognized => ResolvedMirrorMode() picks the host default (own on
	// LodorOS/unknown, merge on NextUI — see hostMirrorModeDefault).
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

	// StateRetain caps how many of THIS device's own save-state uploads survive
	// per (rom, slot) after a landed push (Handoff retention, design 6.4).
	// 0/absent = the default; user-owned via settings.conf state_retain
	// (decided 2026-07-07: retention depth is the user's knob).
	StateRetain int `json:"state_retain,omitempty"`

	// RAUsername / RAToken are the RetroAchievements credential spine (task #46):
	// the RA account handle and its long-lived login token, stored TOP-LEVEL (not
	// under a host) because RA is account-global, independent of any RomM host. The
	// RA PASSWORD is NEVER stored — only the token, mirroring the token-only-at-rest
	// rule for the RomM bearer. Written by `lodor-sync --ra-login`; read by the menu
	// (--ra-status) and, on LodorOS, by the minarch fork's vendored rc_client.
	RAUsername string `json:"ra_username,omitempty"`
	RAToken    string `json:"ra_token,omitempty"`
}

// ResolvedStateRetain returns the per-(rom,slot) cap on this device's own
// save-state uploads: settings.conf/config state_retain when sane, else 5.
// Clamped to [1, 50] — 0/absent means default, never "keep nothing" (retention
// may only ever trim history, not erase it).
func (c *Config) ResolvedStateRetain() int {
	if c == nil || c.StateRetain <= 0 {
		return 5
	}
	if c.StateRetain > 50 {
		return 50
	}
	return c.StateRetain
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
// falls back to the host default (hostMirrorModeDefault) — with ONE hold: a
// DEFAULTED merge over a card whose directory_mappings still carry the separate
// layout ("… RomM (TAG)" folders) resolves SEPARATE. Mode flips are prompt-only
// (C2): a build upgrade changing the default must not silently re-layout an
// existing card — the user opts into merge via the pak prompt, which writes the
// mode explicitly and lifts the hold. Nil-safe.
func (c *Config) ResolvedMirrorMode() string {
	if mode, ok := c.ExplicitMirrorMode(); ok {
		return mode
	}
	def := hostMirrorModeDefault()
	if def == MirrorModeMerge && c != nil && hasSeparateLayoutMappings(c.DirectoryMappings) {
		return MirrorModeSeparate
	}
	return def
}

// hasSeparateLayoutMappings reports whether any directory mapping still points at a
// separate-mode "<Display> RomM (<TAG>)" folder — the signature of a card mirrored
// under the separate layout.
func hasSeparateLayoutMappings(m map[string]DirMapping) bool {
	for _, dm := range m {
		if strings.Contains(dm.RelativePath, " RomM (") {
			return true
		}
	}
	return false
}

// ExplicitMirrorMode returns the mirror mode ONLY when one was explicitly set (a
// recognized value in config.json or the pak's settings.conf overlay) — ok=false
// when the mode is merely the host default. Migration/mapping-regeneration keys off
// THIS, never off a defaulted value: a build upgrade that changes the default must
// not silently restructure an existing card (mode-flip stays prompt-only — the pak's
// consent prompt writes the mode, which makes it explicit).
func (c *Config) ExplicitMirrorMode() (string, bool) {
	if c == nil {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(c.MirrorMode)) {
	case MirrorModeOwn:
		return MirrorModeOwn, true
	case MirrorModeSeparate:
		return MirrorModeSeparate, true
	case MirrorModeMerge:
		return MirrorModeMerge, true
	}
	return "", false
}

// hostMirrorModeDefault picks the mirror mode for a config that carries no explicit
// mirror_mode. The host is identified by the LODOR_HOST_OS env the launcher exports:
//
//	"nextui"             => MERGE (C2, the 2026-07-02 gate decision: RomM games mix
//	                        into the user's own folders — adopt-by-tag + dedup +
//	                        manifest-scoped mutation make it safe; the NextUI pak
//	                        also writes mirror_mode=merge explicitly at onboarding)
//	absent/"lodoros"/…   => OWN (LodorOS: the card IS the library; existing LodorOS
//	                        and muOS cards export nothing and stay byte-identical)
//	anything else        => OWN (unknown host: the historical default, unchanged)
//
// NOTE: merge and own share canonical basenames and the clean "<Display> (<TAG>)"
// folder shape, so a NextUI card that previously resolved the de-facto "own" default
// (the pre-C2 state) upgrades onto "merge" with ZERO on-disk renames — it gains the
// dedup/adoption + manifest safety, not a re-layout. Cards with separate-layout
// mappings are held on separate until the user explicitly opts in (see
// ensureDirectoryMappings' layout hold).
func hostMirrorModeDefault() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LODOR_HOST_OS"))) {
	case "nextui":
		return MirrorModeMerge
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
		c.ApiTimeout = 300 // clamp to the 300s ceiling, don't reset to the default
	}
	if c.DownloadTimeout == 0 {
		c.DownloadTimeout = 3600
	}
	if c.DownloadTimeout > maxDownloadTimeout {
		c.DownloadTimeout = maxDownloadTimeout // clamp a runaway/typo'd value
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
		case "state_retain":
			if n, err := strconv.Atoi(v); err == nil {
				c.StateRetain = n
			}
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
