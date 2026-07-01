package config

// Two-tier external-access endpoint selection (#87/#84). A device may carry more
// than one RomM endpoint in hosts[]: a PREFERRED tier-1 Tailscale internal URL
// (reached through a local SOCKS5 proxy, see Host.Socks5Proxy) and a FALLBACK
// tier-2 public Cloudflare Access URL. The engine prefers tier 1 when it is
// reachable and falls back to tier 2 otherwise. Both endpoints terminate at the
// SAME RomM, so they share one identity (the rmm_ bearer + device_id); only the
// URL, the tier rank, and the optional cf_access/socks5_proxy block differ per
// entry.
//
// COMPOSITION WITH MULTI-USER (ProfileLabel model): two-tier selection ranges over
// the ENDPOINT axis (which URL), while multi-user ranges over the IDENTITY axis
// (which user's host, keyed by ProfileLabel + active-profile.txt). They are kept
// orthogonal: when an active profile is selected, ResolveHost stays profile-driven
// and byte-identical to ActiveHost (no tier probe) — the proven per-user path is
// NEVER re-routed. A single-user / untiered config resolves to hosts[0] with ZERO
// probe. Tier fallback engages only on a single-user, multi-endpoint config.
//
// CGO-free, stdlib only.

import (
	"sort"
	"strings"
)

// orderedHostIndices returns host indices sorted by ascending Tier (the
// preference rank; LOWER = preferred). The sort is stable, so hosts with equal
// tiers keep their config order. Tier 0 (legacy/unset) sorts first, so an
// untiered single-host config is unaffected.
func (c *Config) orderedHostIndices() []int {
	idx := make([]int, len(c.Hosts))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return c.Hosts[idx[a]].Tier < c.Hosts[idx[b]].Tier
	})
	return idx
}

// SelectHostIndex chooses which configured endpoint to use, honoring the tier
// preference. It walks hosts in ascending Tier order and returns the index of the
// FIRST one that probe() reports reachable; the LAST candidate in tier order is
// the unconditional fallback (never probed — there is nowhere left to fall back
// to). A nil probe, a single host, or all-equal tiers degenerates to the first
// host in tier order, which for a legacy single-host config is hosts[0] —
// byte-identical to the old behavior, and with ZERO network probe.
//
// probe receives the candidate Host (so it can honor that endpoint's
// socks5_proxy / insecure-skip-verify when checking reachability); it must be
// cheap and side-effect-free. Returning true means "the endpoint answered"
// (reachability), NOT "auth passed". A tier-1 Tailscale host is therefore probed
// THROUGH its SOCKS5 proxy — "reachable" implies tailscaled is up.
func (c *Config) SelectHostIndex(probe func(h Host) bool) int {
	if c == nil || len(c.Hosts) == 0 {
		return 0
	}
	order := c.orderedHostIndices()
	if probe == nil || len(order) == 1 {
		return order[0]
	}
	// Probe every candidate except the last; the last is the final fallback.
	for _, i := range order[:len(order)-1] {
		if probe(c.Hosts[i]) {
			return i
		}
	}
	return order[len(order)-1]
}

// ResolveHost returns the host the engine should authenticate AND sync as,
// composing two-tier endpoint selection with the multi-user (ProfileLabel) model.
//
//  1. MULTI-USER FIRST: when active-profile.txt names a live profile, this is
//     ActiveHost() byte-identical — the per-user identity host is returned verbatim
//     (it carries its own endpoint, including any socks5_proxy) and NO tier probe
//     runs. The proven multi-user path is never re-routed by tier selection.
//  2. SINGLE-USER / TWO-TIER: pick the preferred reachable endpoint via
//     SelectHostIndex (tier-1 Tailscale when up, else the tier-2 Cloudflare
//     fallback) and inherit the SHARED identity (token/username/password/device/
//     scopes) from hosts[0] when the chosen endpoint carries none of its own. The
//     ENDPOINT fields root_uri/port/insecure/cf_access/socks5_proxy/tier are NEVER
//     inherited.
//
// With a single host and no active profile this is hosts[0] byte-identical — the
// legacy single-user, single-endpoint path is unchanged, with ZERO probe. Caller
// must have checked len(Hosts) > 0 (same precondition as ActiveHost/Hosts[0]).
//
// SECURITY: returns a value; never logs the token/device_id/cf secret.
func (c *Config) ResolveHost(probe func(h Host) bool) Host {
	if c == nil || len(c.Hosts) == 0 {
		return Host{}
	}
	// (1) Multi-user: an active profile pins the identity host; stay byte-identical
	// to ActiveHost and skip tier probing entirely.
	if label := ActiveProfileLabel(); label != "" && !strings.EqualFold(label, "default") {
		return c.ActiveHost()
	}
	// (2) Single-user / two-tier endpoint selection.
	i := c.SelectHostIndex(probe)
	host := c.Hosts[i]
	if i != 0 {
		base := c.Hosts[0]
		if host.Token == "" {
			host.Token = base.Token
		}
		if host.Username == "" {
			host.Username = base.Username
		}
		if host.Password == "" {
			host.Password = base.Password
		}
		if host.DeviceID == "" {
			host.DeviceID = base.DeviceID
		}
		if host.DeviceName == "" {
			host.DeviceName = base.DeviceName
		}
		if host.TokenName == "" {
			host.TokenName = base.TokenName
		}
		if host.TokenExpiresAt == "" {
			host.TokenExpiresAt = base.TokenExpiresAt
		}
		if len(host.Scopes) == 0 {
			host.Scopes = base.Scopes
		}
	}
	return host
}
