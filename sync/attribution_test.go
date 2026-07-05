package sync

// Tests for the READ-side device attribution helpers (#176 follow-up): the
// caller-scoped-projection fix. GetSavesAttributed must send this device's device_id
// so RomM populates device_syncs, but degrade to the plain query on any error;
// AttributedDeviceName must never mislabel a foreign save as this device (the
// self-first synthetic-placeholder trap).

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"lodor/config"
	"lodor/romm"
)

func TestAttributedDeviceName(t *testing.T) {
	self := "dev-self"
	ds := func(id, name string, cur bool) romm.DeviceSaveSync {
		return romm.DeviceSaveSync{DeviceID: id, DeviceName: name, IsCurrent: cur}
	}
	cases := []struct {
		name  string
		syncs []romm.DeviceSaveSync
		want  string
	}{
		{"no attribution", nil, ""},
		// foreign save: self is synthesized FIRST (not current), the real owner second.
		{"foreign, self placeholder first", []romm.DeviceSaveSync{ds(self, "Self", false), ds("dev-a", "RG34XX", true)}, "RG34XX"},
		// self made it and holds it alone -> fall back to self.
		{"self only", []romm.DeviceSaveSync{ds(self, "Self", true)}, "Self"},
		// prefer the CURRENT other over a stale other.
		{"prefer current other", []romm.DeviceSaveSync{ds(self, "Self", false), ds("dev-a", "Stale", false), ds("dev-b", "Current", true)}, "Current"},
		// no other is current -> take any other (never self while an other exists).
		{"any other when none current", []romm.DeviceSaveSync{ds(self, "Self", false), ds("dev-a", "Other", false)}, "Other"},
	}
	for _, c := range cases {
		got := AttributedDeviceName(romm.Save{DeviceSyncs: c.syncs}, self)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// attribMock records whether the last GET /api/saves carried a device_id, and can be
// told to 500 specifically when a device_id is present (to exercise the fallback).
type attribMock struct {
	mu             sync.Mutex
	version        string
	failOnDeviceID bool
	sawDeviceID    bool
	sawPlain       bool
}

func (m *attribMock) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fmt.Sprintf(`{"SYSTEM":{"VERSION":%q}}`, m.version))
	})
	mux.HandleFunc("GET /api/saves", func(w http.ResponseWriter, r *http.Request) {
		dev := r.URL.Query().Get("device_id")
		m.mu.Lock()
		if dev != "" {
			m.sawDeviceID = true
		} else {
			m.sawPlain = true
		}
		fail := m.failOnDeviceID && dev != ""
		m.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"detail":"missing devices.read"}`)
			return
		}
		_, _ = io.WriteString(w, `[{"id":1,"rom_id":7}]`)
	})
	return httptest.NewServer(mux)
}

func attribCfg(url string) *config.Config {
	return &config.Config{Hosts: []config.Host{{RootURI: url, Token: "t", DeviceID: "dev-self"}}}
}

func TestGetSavesAttributedSendsDeviceID(t *testing.T) {
	m := &attribMock{version: "4.9.2"}
	srv := m.server()
	defer srv.Close()
	cfg := attribCfg(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	saves, err := GetSavesAttributed(c, cfg, romm.SaveQuery{RomID: 7})
	if err != nil || len(saves) != 1 {
		t.Fatalf("expected 1 save no error, got %d err=%v", len(saves), err)
	}
	if !m.sawDeviceID {
		t.Fatalf("attributed fetch must carry device_id on 4.9.2")
	}
}

func TestGetSavesAttributedFallsBackOnError(t *testing.T) {
	m := &attribMock{version: "4.9.2", failOnDeviceID: true}
	srv := m.server()
	defer srv.Close()
	cfg := attribCfg(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	saves, err := GetSavesAttributed(c, cfg, romm.SaveQuery{RomID: 7})
	if err != nil {
		t.Fatalf("device_id 403 must fall back cleanly, got err=%v", err)
	}
	if len(saves) != 1 {
		t.Fatalf("fallback should still return the saves, got %d", len(saves))
	}
	if !m.sawDeviceID || !m.sawPlain {
		t.Fatalf("expected an attributed attempt THEN a plain fallback; devID=%v plain=%v", m.sawDeviceID, m.sawPlain)
	}
}

func TestGetSavesAttributedGatedBelow490(t *testing.T) {
	m := &attribMock{version: "4.8.0"}
	srv := m.server()
	defer srv.Close()
	cfg := attribCfg(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	if _, err := GetSavesAttributed(c, cfg, romm.SaveQuery{RomID: 7}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if m.sawDeviceID {
		t.Fatalf("must NOT send device_id to a pre-4.9.0 server")
	}
	if !m.sawPlain {
		t.Fatalf("expected a plain (un-attributed) query below 4.9.0")
	}
}
