package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A real signature over fixtureManifest, produced by the OFFLINE production
// signing key (lodor-signmanifest against
// /mnt/user/appdata/lodor/update-signing-ed25519.key). It exercises
// VerifyManifestSig against the embedded TrustedPubKey end-to-end WITHOUT the
// private key existing in CI. If the trusted key ever rotates, regenerate this
// pair (see release/lodor-update-signing.md).
const (
	fixtureManifest = "lodor-signed-manifest-fixture-v1"
	fixtureSigB64   = "p+h/GiTM8ryRTZ3RCmeOCgwT2T9Y98MzSz1pl3WQHAe01P52uulvVUQNKrrGJABsfclKGp8MQjDGwWQa5ZBQAg=="
)

func TestVerifyManifestSigValid(t *testing.T) {
	if err := VerifyManifestSig([]byte(fixtureManifest), []byte(fixtureSigB64)); err != nil {
		t.Fatalf("valid signature from the production key must verify against TrustedPubKey: %v", err)
	}
}

func TestVerifyManifestSigToleratesTrailingNewline(t *testing.T) {
	// The publisher/editor may append a newline to the .sig; it must still verify.
	if err := VerifyManifestSig([]byte(fixtureManifest), []byte(fixtureSigB64+"\n")); err != nil {
		t.Fatalf("signature with trailing newline should verify: %v", err)
	}
}

func TestVerifyManifestSigTamperedManifest(t *testing.T) {
	tampered := []byte(fixtureManifest)
	tampered[0] ^= 0xFF
	err := VerifyManifestSig(tampered, []byte(fixtureSigB64))
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered manifest = %v, want ErrBadSignature", err)
	}
}

func TestVerifyManifestSigWrongKey(t *testing.T) {
	// Sign with a DIFFERENT key; verification against TrustedPubKey must fail.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig := ed25519.Sign(priv, []byte(fixtureManifest))
	sigB64 := base64.StdEncoding.EncodeToString(sig)
	err := VerifyManifestSig([]byte(fixtureManifest), []byte(sigB64))
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong-key signature = %v, want ErrBadSignature", err)
	}
}

func TestVerifyManifestSigMalformedBase64(t *testing.T) {
	err := VerifyManifestSig([]byte(fixtureManifest), []byte("!!!not base64!!!"))
	if !errors.Is(err, ErrMalformedSignature) {
		t.Fatalf("malformed base64 = %v, want ErrMalformedSignature", err)
	}
	if errors.Is(err, ErrBadSignature) {
		t.Fatal("malformed base64 must be distinct from ErrBadSignature")
	}
}

func TestVerifyManifestSigWrongLength(t *testing.T) {
	// valid base64, but not 64 bytes → malformed, not a verification failure.
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	err := VerifyManifestSig([]byte(fixtureManifest), []byte(short))
	if !errors.Is(err, ErrMalformedSignature) {
		t.Fatalf("short signature = %v, want ErrMalformedSignature", err)
	}
}

// sigServer serves the manifest at "/versions.json" and, when sig != "",
// the signature at "/versions.json.sig". A "" sig makes the .sig 404.
func sigServer(t *testing.T, manifest, sig string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/versions.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifest))
	})
	mux.HandleFunc("/versions.json.sig", func(w http.ResponseWriter, r *http.Request) {
		if sig == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(sig))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const goodManifest = `{"schema":1,"stable":{"version":"0.9.9","notes":"n","assets":{}}}`

// signWith signs b with an ephemeral key and returns (base64 sig, pubkey).
func signWith(t *testing.T, b []byte) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, b)), pub
}

func mustFetch(t *testing.T, client *http.Client, url string) []byte {
	t.Helper()
	b, err := httpGetBytes(client, url)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCheckManifestSigOff: mode "off" never fetches or verifies.
func TestCheckManifestSigOff(t *testing.T) {
	srv := sigServer(t, goodManifest, "") // .sig 404s; "off" must not care
	client := &http.Client{Timeout: manifestTimeout}
	raw := mustFetch(t, client, srv.URL+"/versions.json")
	if err := checkManifestSig(client, srv.URL+"/versions.json", raw, "off"); err != nil {
		t.Fatalf("off mode must never error: %v", err)
	}
}

// TestCheckManifestSigWarnMissing: warn + missing .sig → proceeds (nil).
func TestCheckManifestSigWarnMissing(t *testing.T) {
	srv := sigServer(t, goodManifest, "")
	client := &http.Client{Timeout: manifestTimeout}
	url := srv.URL + "/versions.json"
	raw := mustFetch(t, client, url)
	if err := checkManifestSig(client, url, raw, "warn"); err != nil {
		t.Fatalf("warn + missing sig must proceed (nil), got %v", err)
	}
}

// TestCheckManifestSigWarnBad: warn + tampered manifest (sig no longer matches)
// → still proceeds (nil).
func TestCheckManifestSigWarnBad(t *testing.T) {
	sig, _ := signWith(t, []byte(goodManifest))
	tampered := strings.Replace(goodManifest, "0.9.9", "6.6.6", 1)
	srv := sigServer(t, tampered, sig) // sig is over the ORIGINAL bytes
	client := &http.Client{Timeout: manifestTimeout}
	url := srv.URL + "/versions.json"
	raw := mustFetch(t, client, url)
	if err := checkManifestSig(client, url, raw, "warn"); err != nil {
		t.Fatalf("warn + bad sig must proceed (nil), got %v", err)
	}
}

// TestCheckManifestSigEnforceMissing: enforce + missing .sig → error.
func TestCheckManifestSigEnforceMissing(t *testing.T) {
	srv := sigServer(t, goodManifest, "")
	client := &http.Client{Timeout: manifestTimeout}
	url := srv.URL + "/versions.json"
	raw := mustFetch(t, client, url)
	if err := checkManifestSig(client, url, raw, "enforce"); err == nil {
		t.Fatal("enforce + missing sig must error")
	}
}

// TestCheckManifestSigEnforceBad: enforce + tampered manifest → error.
func TestCheckManifestSigEnforceBad(t *testing.T) {
	sig, _ := signWith(t, []byte(goodManifest))
	tampered := strings.Replace(goodManifest, "0.9.9", "6.6.6", 1)
	srv := sigServer(t, tampered, sig)
	client := &http.Client{Timeout: manifestTimeout}
	url := srv.URL + "/versions.json"
	raw := mustFetch(t, client, url)
	if err := checkManifestSig(client, url, raw, "enforce"); err == nil {
		t.Fatal("enforce + bad sig must error")
	}
}

// TestFetchManifestWarnMissingSigStillReturns: the shipped path — SigMode is
// "warn", a real fleet manifest has no .sig yet, FetchManifest must still return
// the parsed manifest exactly as before signing existed.
func TestFetchManifestWarnMissingSigStillReturns(t *testing.T) {
	if SigMode != "warn" {
		t.Skipf("shipped SigMode is %q, this test asserts the warn rollout", SigMode)
	}
	srv := sigServer(t, goodManifest, "") // no signature published
	m, err := FetchManifest(srv.URL + "/versions.json")
	if err != nil {
		t.Fatalf("warn mode must return the manifest with no .sig present: %v", err)
	}
	if m.Stable == nil || m.Stable.Version != "0.9.9" {
		t.Fatalf("manifest not parsed: %+v", m)
	}
}

// TestSigURL confirms the .sig URL derivation, including with a query string
// (LODOR_VERSIONS_URL may carry one).
func TestSigURL(t *testing.T) {
	cases := map[string]string{
		"https://x/versions.json":     "https://x/versions.json.sig",
		"https://x/versions.json?v=2": "https://x/versions.json.sig?v=2",
	}
	for in, want := range cases {
		if got := sigURL(in); got != want {
			t.Errorf("sigURL(%q) = %q, want %q", in, got, want)
		}
	}
}
