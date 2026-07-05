package sync

// Best-effort / honest-reason proof for the on-device rom write-back (task #167) at the
// SYNC layer. Two guarantees: (1) an unresolvable ROM path is reported as reason=resolve
// without ever touching the wire; (2) each client failure class maps to the right honest
// reason token (never a fake success, never a mis-classified pairing-expired). The RomM
// wire shape itself is proven by the client-layer mock tests (romm/props_test.go).

import (
	"fmt"
	"testing"
	"time"

	"lodor/config"
	"lodor/romm"
)

// cfgNoIndex is a config with a host but NO catalog index on disk, so ResolveRomID
// always misses — the resolve-honesty path.
func cfgNoIndex() *config.Config {
	return &config.Config{Hosts: []config.Host{{RootURI: "http://127.0.0.1:1", Token: "scopes:x", DeviceID: "dev-1"}}}
}

func TestWriteBackResolveMiss(t *testing.T) {
	cfg := cfgNoIndex()
	c := romm.NewClient(cfg.ActiveHost(), time.Second)

	if res := SetFavoriteForRom(c, cfg, "/roms/GBA/Nope.gba", true); res.OK || res.Reason != "resolve" {
		t.Fatalf("favorite of unresolvable path should be reason=resolve, got %+v", res)
	}
	if res := SetRomPropsForRom(c, cfg, "/roms/GBA/Nope.gba", romm.RomUserData{Rating: romm.PtrInt(5)}); res.OK || res.Reason != "resolve" {
		t.Fatalf("props of unresolvable path should be reason=resolve, got %+v", res)
	}
}

func TestClassifyWriteBackReasons(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantReason string
		wantAuth   bool
	}{
		{"forbidden", fmt.Errorf("API error: status 403, body: insufficient permissions"), "forbidden", false},
		{"notfound", fmt.Errorf("API error: status 404, body: not found"), "notfound", false},
		{"range", fmt.Errorf("API error: status 422, body: out of range"), "range", false},
		{"autherr", &romm.AuthError{StatusCode: 401}, "autherr", true},
		{"unreachable", fmt.Errorf("execute request: connection refused"), "unreachable", false},
		{"generic", fmt.Errorf("API error: status 500, body: boom"), "error", false},
	} {
		res := classifyWriteBack(7, tc.err)
		if res.OK {
			t.Errorf("%s: OK must be false", tc.name)
		}
		if res.Reason != tc.wantReason {
			t.Errorf("%s: reason=%q, want %q", tc.name, res.Reason, tc.wantReason)
		}
		if res.AuthExpired != tc.wantAuth {
			t.Errorf("%s: authExpired=%v, want %v", tc.name, res.AuthExpired, tc.wantAuth)
		}
	}
}
