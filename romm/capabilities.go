package romm

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Version is a parsed RomM server version (major.minor.patch). Raw keeps the original
// string for diagnostics. Unparseable components default to zero.
type Version struct {
	Major, Minor, Patch int
	Raw                 string
}

// ParseVersion parses a RomM version string liberally: a leading 'v'/'V' is dropped,
// any pre-release/build suffix ("-beta", "+meta", trailing space) is ignored, and
// missing components default to 0. An empty or fully-unparseable string yields the
// zero Version (AtLeast treats it as older than everything), so an unknown server
// safely falls back to the legacy strategy — we never assume a capability we cannot
// prove from a real version.
func ParseVersion(s string) Version {
	v := Version{Raw: s}
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "v")
	t = strings.TrimPrefix(t, "V")
	for _, sep := range []string{"-", "+", " "} {
		if i := strings.Index(t, sep); i >= 0 {
			t = t[:i]
		}
	}
	parts := strings.Split(t, ".")
	if len(parts) > 0 {
		v.Major = atoiSafe(parts[0])
	}
	if len(parts) > 1 {
		v.Minor = atoiSafe(parts[1])
	}
	if len(parts) > 2 {
		v.Patch = atoiSafe(parts[2])
	}
	return v
}

// atoiSafe parses a non-negative integer, returning 0 on any error or negative value.
func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// AtLeast reports whether v >= major.minor.patch, compared component by component.
// The zero Version (unknown server) is at least only 0.0.0.
func (v Version) AtLeast(major, minor, patch int) bool {
	if v.Major != major {
		return v.Major > major
	}
	if v.Minor != minor {
		return v.Minor > minor
	}
	return v.Patch >= patch
}

// Known reports whether a real server version was parsed (non-zero major), as opposed
// to the zero Version returned when the heartbeat carried no usable version.
func (v Version) Known() bool { return v.Major > 0 }

// String renders the canonical major.minor.patch.
func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// Capability version thresholds — the single source of truth for the version at which
// each server feature became available. SyncNegotiate/TrustServerHash at 4.9.0 and
// DeviceAuth at 5.0.0 are per the Argosy research (lodor-argosy-research.md #2/#3).
// PlaySession is a CONSERVATIVE placeholder pending live verification (the endpoint
// exists on the deployed server; the exact introduction version is unconfirmed) — the
// reconciler and negotiate paths do NOT depend on it.
var (
	verSyncNegotiate   = [3]int{4, 9, 0}
	verTrustServerHash = [3]int{4, 9, 0}
	verDeviceAuth      = [3]int{5, 0, 0}
	verPlaySession     = [3]int{3, 6, 0} // UNVERIFIED threshold — see comment above
)

// Capabilities is the set of RomM server features the sync engine gates on, derived
// from the version reported by GET /api/heartbeat (Argosy's RomMCapabilities.from).
// Every flag is conservative: an unknown version leaves all flags false and the engine
// uses the legacy direct strategy.
type Capabilities struct {
	Version Version

	// SupportsSyncNegotiate: server runs the 3-way reconcile itself via
	// POST /api/sync/negotiate (RomM >= 4.9.0). Gates the negotiate strategy.
	SupportsSyncNegotiate bool
	// TrustsServerHash: server returns a reliable per-save content_hash the client can
	// compare to a local MD5 without re-downloading (RomM >= 4.9.0).
	TrustsServerHash bool
	// SupportsDeviceAuth: device-code pairing available (RomM >= 5.0.0).
	SupportsDeviceAuth bool
	// SupportsPlaySessionIngest: POST /api/play-sessions accepted. Threshold UNVERIFIED.
	SupportsPlaySessionIngest bool
}

// CapabilitiesFrom derives the capability set from a parsed server version.
func CapabilitiesFrom(v Version) Capabilities {
	return Capabilities{
		Version:                   v,
		SupportsSyncNegotiate:     v.AtLeast(verSyncNegotiate[0], verSyncNegotiate[1], verSyncNegotiate[2]),
		TrustsServerHash:          v.AtLeast(verTrustServerHash[0], verTrustServerHash[1], verTrustServerHash[2]),
		SupportsDeviceAuth:        v.AtLeast(verDeviceAuth[0], verDeviceAuth[1], verDeviceAuth[2]),
		SupportsPlaySessionIngest: v.AtLeast(verPlaySession[0], verPlaySession[1], verPlaySession[2]),
	}
}

// HeartbeatInfo is the parsed result of GET /api/heartbeat: the server version (when
// the server reports one) and the capabilities derived from it.
type HeartbeatInfo struct {
	Version      Version
	Capabilities Capabilities
}

// GetHeartbeat performs GET /api/heartbeat and returns the parsed server version and
// derived capabilities. A reachable server whose heartbeat carries no recognizable
// version yields an unknown (zero) Version and an all-false capability set — caller
// then uses the legacy strategy. A transport error is returned as-is (host-free
// handling is the caller's job).
func (c *Client) GetHeartbeat() (HeartbeatInfo, error) {
	raw, err := c.doRaw("GET", "/api/heartbeat")
	if err != nil {
		return HeartbeatInfo{}, err
	}
	v := parseHeartbeatVersion(raw)
	return HeartbeatInfo{Version: v, Capabilities: CapabilitiesFrom(v)}, nil
}

// parseHeartbeatVersion extracts the server version from a heartbeat body. The body
// shape has shifted across RomM releases ({"system":{"version":...}},
// {"SYSTEM":{"VERSION":...}}, top-level), so it searches the decoded JSON for the
// first string value under any key that case-insensitively equals "version". An
// unparseable body yields the zero Version.
//
// NOTE: the exact heartbeat schema for the deployed/target server is UNVERIFIED here
// (live integration deferred). The liberal search is deliberately schema-tolerant so
// a shape change cannot crash capability detection — worst case it finds no version
// and the engine falls back to legacy, which is always safe.
func parseHeartbeatVersion(raw []byte) Version {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Version{}
	}
	if s := searchVersion(doc); s != "" {
		return ParseVersion(s)
	}
	return Version{}
}

// searchVersion walks a decoded JSON value, returning the first string value found
// under a key case-insensitively equal to "version". A direct hit at the current
// object level is preferred before recursing into nested objects/arrays.
func searchVersion(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if strings.EqualFold(k, "version") {
				if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
		for _, val := range t {
			if s := searchVersion(val); s != "" {
				return s
			}
		}
	case []any:
		for _, val := range t {
			if s := searchVersion(val); s != "" {
				return s
			}
		}
	}
	return ""
}
