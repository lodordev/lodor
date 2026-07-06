package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// DefaultManifestURL is where releases are announced: a static versions.json
// published to gh-pages ONLY AFTER every named asset has been re-downloaded
// and hash-verified live on GitHub (the manifest never precedes its assets).
// LODOR_VERSIONS_URL overrides it — the test harness points it at a fixture
// server; devices never need to.
const DefaultManifestURL = "https://lodordev.github.io/lodor/versions.json"

// manifestTimeout bounds the versions.json fetch. The check piggybacks on an
// already-up radio session; a slow manifest host must not eat the sync window.
const manifestTimeout = 30 * time.Second

// Asset is one downloadable update artifact for a specific lane/arch.
type Asset struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Channel describes the newest release on one channel.
type Channel struct {
	Version string           `json:"version"`
	Notes   string           `json:"notes"`
	MinRomm string           `json:"min_romm"` // advisory only — warn, never block
	Assets  map[string]Asset `json:"assets"`
}

// Manifest is the versions.json shape (schema 1).
type Manifest struct {
	Schema int                `json:"schema"`
	Stable *Channel           `json:"stable"`
	Beta   *Channel           `json:"beta"`
	Notify map[string]string  `json:"notify"` // store-lane latest versions (nextui/muos/lodoros_card)
}

// ManifestURL returns the override from LODOR_VERSIONS_URL or the default.
func ManifestURL() string {
	if u := os.Getenv("LODOR_VERSIONS_URL"); u != "" {
		return u
	}
	return DefaultManifestURL
}

// FetchManifest GETs and parses versions.json. It uses a plain direct
// transport — github.io is public internet, NOT behind the RomM host's
// Tailscale/CF-Access tiers — and honors SSL_CERT_FILE via Go's system cert
// pool (the pak exports the bundled Mozilla CA file).
func FetchManifest(url string) (*Manifest, error) {
	client := &http.Client{Timeout: manifestTimeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "lodor-sync-updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest fetch: HTTP %d", resp.StatusCode)
	}
	var m Manifest
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest parse: %w", err)
	}
	if m.Schema != 1 {
		return nil, fmt.Errorf("manifest schema %d not understood (want 1)", m.Schema)
	}
	return &m, nil
}

// ChannelFor returns the manifest channel for a settings.conf update_channel
// value. Anything but "beta" is stable — unknown values must degrade to the
// safe channel, not error a background check.
func (m *Manifest) ChannelFor(name string) *Channel {
	if name == "beta" && m.Beta != nil {
		return m.Beta
	}
	return m.Stable
}
