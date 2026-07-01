package config

import "testing"

// reachableSet returns a probe that reports the given base URLs (Host.URL()) as up.
func reachableSet(up ...string) func(Host) bool {
	set := map[string]bool{}
	for _, u := range up {
		set[u] = true
	}
	return func(h Host) bool { return set[h.URL()] }
}

func TestSelectHostIndex_SingleHostNeverProbes(t *testing.T) {
	c := &Config{Hosts: []Host{{RootURI: "https://only.example"}}}
	called := false
	got := c.SelectHostIndex(func(Host) bool { called = true; return false })
	if got != 0 {
		t.Fatalf("single host: got index %d, want 0", got)
	}
	if called {
		t.Fatal("single host must not probe")
	}
}

func TestSelectHostIndex_PrefersReachableTier1(t *testing.T) {
	c := &Config{Hosts: []Host{
		{Tier: 2, RootURI: "https://public.example"},
		{Tier: 1, RootURI: "https://tail.example"},
	}}
	// tier-1 reachable -> pick it (index 1) despite config order.
	got := c.SelectHostIndex(reachableSet("https://tail.example"))
	if got != 1 {
		t.Fatalf("tier1 up: got index %d, want 1", got)
	}
}

func TestSelectHostIndex_FallsBackToTier2(t *testing.T) {
	c := &Config{Hosts: []Host{
		{Tier: 1, RootURI: "https://tail.example"},
		{Tier: 2, RootURI: "https://public.example"},
	}}
	// nothing reachable -> final fallback is the highest-tier (last in order) host.
	got := c.SelectHostIndex(reachableSet())
	if got != 1 {
		t.Fatalf("tier1 down: got index %d, want 1 (tier2 fallback)", got)
	}
}

func TestResolveHost_InheritsIdentityAndCFAccess(t *testing.T) {
	c := &Config{Hosts: []Host{
		{Tier: 1, RootURI: "https://tail.example", Token: "rmm_shared", DeviceID: "dev-1"},
		{Tier: 2, RootURI: "https://public.example",
			CFAccess: &CFAccess{ClientID: "id.access", ClientSecret: "sek"}},
	}}
	// tier-1 down -> resolve to tier-2, inheriting token/device from hosts[0],
	// keeping tier-2's own endpoint + cf_access.
	h := c.ResolveHost(reachableSet())
	if h.RootURI != "https://public.example" {
		t.Fatalf("endpoint: got %q", h.RootURI)
	}
	if h.Token != "rmm_shared" {
		t.Fatalf("token not inherited: got %q", h.Token)
	}
	if h.DeviceID != "dev-1" {
		t.Fatalf("device not inherited: got %q", h.DeviceID)
	}
	if h.CFAccess == nil || h.CFAccess.ClientID != "id.access" {
		t.Fatalf("cf_access lost on resolve")
	}
}

func TestResolveHost_LegacySingleHostByteIdentical(t *testing.T) {
	c := &Config{Hosts: []Host{{RootURI: "https://only.example", Token: "rmm_x", DeviceID: "d"}}}
	h := c.ResolveHost(nil)
	a := c.ActiveHost()
	if h.RootURI != a.RootURI || h.Token != a.Token || h.DeviceID != a.DeviceID {
		t.Fatalf("legacy single host: ResolveHost must equal ActiveHost (got %+v want %+v)", h, a)
	}
	if h.CFAccess != nil {
		t.Fatal("legacy host must carry no cf_access")
	}
}


// TestResolveHost_Socks5ProxyIsEndpointField asserts socks5_proxy travels with its
// OWN endpoint and is NEVER inherited from hosts[0] (it is an endpoint field like
// cf_access/tier/root_uri): the tier-1 host keeps its proxy when selected, and the
// tier-2 host carries none when it is the fallback — so the public path never gets
// a stray SOCKS5 dialer.
func TestResolveHost_Socks5ProxyIsEndpointField(t *testing.T) {
	c := &Config{Hosts: []Host{
		{Tier: 1, RootURI: "https://box.romm.tailnet.ts.net", Token: "rmm_shared",
			DeviceID: "dev-1", Socks5Proxy: "localhost:1055"},
		{Tier: 2, RootURI: "https://public.example",
			CFAccess: &CFAccess{ClientID: "id.access", ClientSecret: "sek"}},
	}}
	// tier-1 up -> resolve to it, proxy preserved.
	up := c.ResolveHost(reachableSet("https://box.romm.tailnet.ts.net"))
	if up.Socks5Proxy != "localhost:1055" {
		t.Fatalf("tier1 selected: socks5_proxy=%q want localhost:1055", up.Socks5Proxy)
	}
	// tier-1 down -> fall back to tier-2; it must NOT inherit hosts[0].socks5_proxy.
	down := c.ResolveHost(reachableSet())
	if down.RootURI != "https://public.example" {
		t.Fatalf("fallback endpoint: got %q", down.RootURI)
	}
	if down.Socks5Proxy != "" {
		t.Fatalf("tier2 fallback must carry NO socks5_proxy, got %q", down.Socks5Proxy)
	}
}
