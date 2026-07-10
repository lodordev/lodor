package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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

// maxManifestBytes caps the manifest body we read into memory for signature
// verification. versions.json is a few KB; this only exists so a hostile host
// can't stream gigabytes into the RAM of a handheld. It is generous.
const maxManifestBytes = 4 << 20 // 4 MiB

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
	Schema int               `json:"schema"`
	Stable *Channel          `json:"stable"`
	Beta   *Channel          `json:"beta"`
	Notify map[string]string `json:"notify"` // store-lane latest versions (nextui/muos/lodoros_card)
}

// ManifestURL returns the override from LODOR_VERSIONS_URL or the default.
func ManifestURL() string {
	if u := os.Getenv("LODOR_VERSIONS_URL"); u != "" {
		return u
	}
	return DefaultManifestURL
}

// sigURL derives the detached-signature URL from a manifest URL by appending
// ".sig" (before any query string). The release signer writes versions.json.sig
// beside versions.json and the publish workflow copies it to gh-pages, so the
// signature is always at <manifestURL>.sig — including under LODOR_VERSIONS_URL.
func sigURL(manifestURL string) string {
	if i := strings.IndexByte(manifestURL, '?'); i >= 0 {
		return manifestURL[:i] + ".sig" + manifestURL[i:]
	}
	return manifestURL + ".sig"
}

// FetchManifest GETs and parses versions.json. It uses a plain direct
// transport — github.io is public internet, NOT behind the RomM host's
// Tailscale/CF-Access tiers — and honors SSL_CERT_FILE via Go's system cert
// pool (the pak exports the bundled Mozilla CA file).
//
// Signature handling (SigMode): the raw response body is captured and the
// detached signature at <url>.sig is verified OVER THOSE EXACT BYTES before the
// same bytes are unmarshalled — ed25519 signs bytes, so verifying the parsed-
// then-reserialized form would break on any formatting difference. See SigMode
// for the off/warn/enforce policy; the shipped mode is "warn" (verify + log,
// never block), so this can only add safety to the existing update path.
func FetchManifest(url string) (*Manifest, error) {
	client := &http.Client{Timeout: manifestTimeout}
	raw, err := httpGetBytes(client, url)
	if err != nil {
		return nil, err
	}

	// Verify the signature over the RAW bytes we just fetched, before parsing,
	// per SigMode. In "warn" a missing/bad signature logs but does not stop us.
	if err := checkManifestSig(client, url, raw, SigMode); err != nil {
		return nil, err
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest parse: %w", err)
	}
	if m.Schema != 1 {
		return nil, fmt.Errorf("manifest schema %d not understood (want 1)", m.Schema)
	}
	return &m, nil
}

// checkManifestSig applies the given signature mode (FetchManifest passes
// SigMode; tests pass explicit modes) to the manifest's detached signature.
//
//	off     — no fetch, no verify.
//	warn    — fetch <url>.sig and verify over manifestRaw; log OK or a clear
//	          WARNING, but ALWAYS return nil (the update proceeds exactly as it
//	          did before signing existed, including when the .sig 404s).
//	enforce — a missing or non-verifying signature returns an error, refusing
//	          the manifest.
//
// It never returns a non-nil error in "warn" mode: the whole point of the
// fail-open rollout is that this cannot break a live fleet.
func checkManifestSig(client *http.Client, manifestURL string, manifestRaw []byte, mode string) error {
	if mode == "off" {
		return nil
	}
	enforce := mode == "enforce"

	sig, err := httpGetBytes(client, sigURL(manifestURL))
	if err != nil {
		// Missing/unreachable signature (e.g. 404 on gh-pages before the .sig is
		// copied). In warn mode this is expected during rollout: warn + proceed.
		if enforce {
			return fmt.Errorf("update: manifest signature required but not fetchable: %w", err)
		}
		logSig("WARNING: manifest signature not available (%v) — proceeding UNVERIFIED (warn mode)", err)
		return nil
	}

	if err := VerifyManifestSig(manifestRaw, sig); err != nil {
		if enforce {
			return fmt.Errorf("update: manifest signature verification failed: %w", err)
		}
		logSig("WARNING: manifest signature did NOT verify (%v) — proceeding anyway (warn mode)", err)
		return nil
	}

	logSig("manifest signature OK")
	return nil
}

// logSig emits a one-line signature-verification note to stderr, matching how
// the rest of the update modes talk to the launcher's log (fmt.Fprintln to
// os.Stderr). No third-party logger; the shell captures stderr.
func logSig(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "update-sig: "+format+"\n", args...)
}

// httpGetBytes GETs url and returns the body (bounded by maxManifestBytes),
// erroring on any non-200. Shared by the manifest and its .sig so both honor
// the same timeout/transport/User-Agent.
func httpGetBytes(client *http.Client, url string) ([]byte, error) {
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
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return nil, err
	}
	return b, nil
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
