package romm

// RomM server-version discovery and the device_save_sync feature gate (task #176).
//
// The device-ledger endpoints (POST /api/saves/{id}/track, /untrack, /downloaded and
// GET /api/saves/summary) landed in RomM 4.9.0. A client talking to an older server
// must NEVER attempt them — an older RomM 404s the path, which our best-effort layer
// already tolerates, but gating up front keeps the wire quiet and the intent honest.
//
// The version is read from GET /api/heartbeat (SYSTEM.VERSION). On THIS deployment the
// heartbeat is auth-gated (a bad token 500s it — see onboard.go), so ServerVersion runs
// with the client's normal bearer. Stdlib only, CGO-free.

import (
	"strconv"
	"strings"
)

// deviceSaveSyncMinVersion is the first RomM release whose saves carry the
// track/untrack/downloaded/summary device_save_sync endpoints (verified against the
// 4.9.2 stable contract; byte-identical to master per the #176 source read).
var deviceSaveSyncMinVersion = [3]int{4, 9, 0}

// HeartbeatInfo is the subset of GET /api/heartbeat the engine consumes: the server
// version, nested under SYSTEM exactly as RomM emits it. Every other heartbeat block
// (WATCHER/SCHEDULER/METADATA_SOURCES/…) is deliberately ignored.
type HeartbeatInfo struct {
	System struct {
		Version string `json:"VERSION"`
	} `json:"SYSTEM"`
}

// HeartbeatInfo fetches GET /api/heartbeat and returns the parsed version block. Unlike
// Heartbeat() (a bare reachability probe) this decodes the body. Used by ServerVersion.
func (c *Client) HeartbeatInfo() (HeartbeatInfo, error) {
	var hb HeartbeatInfo
	err := c.doJSON("GET", "/api/heartbeat", nil, &hb)
	return hb, err
}

// ServerVersion returns the RomM version string ("4.9.2"), fetching GET /api/heartbeat
// at most once per Client and caching the result (including a cached "" when the server
// answers but omits a version). On a heartbeat error it returns "" and does NOT cache,
// so a later call can retry once the server is reachable. Never errors to the caller —
// an unknown version simply reads as "not new enough" at the gate.
func (c *Client) ServerVersion() string {
	if c.serverVerSet {
		return c.serverVer
	}
	hb, err := c.HeartbeatInfo()
	if err != nil {
		return "" // transient: leave uncached so a later call can retry
	}
	c.serverVer = strings.TrimSpace(hb.System.Version)
	c.serverVerSet = true
	return c.serverVer
}

// SupportsDeviceSaveSync reports whether the connected RomM is new enough (>= 4.9.0)
// to carry the device_save_sync ledger endpoints. It is the ONE gate every #176
// best-effort call checks first. An unknown/unparseable version reads as false (fail
// closed — we never fire an endpoint the server may not have).
func (c *Client) SupportsDeviceSaveSync() bool {
	return versionAtLeast(c.ServerVersion(), deviceSaveSyncMinVersion)
}

// versionAtLeast reports whether the dotted version string ver is >= min ([major,
// minor, patch]). It is lenient by design: a leading 'v' is stripped, a pre-release or
// build suffix ("4.9.0-rc1", "4.9.2+build.3") is cut at the first non-digit in each
// component, and missing components read as 0. An empty or unparseable major reads as
// version 0.0.0 (false for any positive min) — fail closed.
func versionAtLeast(ver string, min [3]int) bool {
	ver = strings.TrimSpace(ver)
	ver = strings.TrimPrefix(ver, "v")
	if ver == "" {
		return false
	}
	// Cut any build/pre-release tail so "4.9.2+meta" / "4.9.0-rc1" parse to their core.
	for _, sep := range []string{"-", "+", " "} {
		if i := strings.IndexAny(ver, sep); i >= 0 {
			ver = ver[:i]
		}
	}
	parts := strings.Split(ver, ".")
	var got [3]int
	for i := 0; i < 3; i++ {
		if i < len(parts) {
			got[i] = leadingInt(parts[i])
		}
	}
	for i := 0; i < 3; i++ {
		if got[i] != min[i] {
			return got[i] > min[i]
		}
	}
	return true // exactly equal
}

// leadingInt parses the leading run of digits in s (e.g. "9rc" -> 9, "" -> 0). Any
// non-digit tail is ignored, so a component like "0beta" reads as 0.
func leadingInt(s string) int {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}
