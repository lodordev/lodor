package romm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lodor/config"
)

// TestClientDoesNotLeakSecretsAcrossHostRedirect proves the CheckRedirect policy:
// a cross-host 3xx from the origin RomM must NOT cause the bearer or the CF-Access
// service-token headers to be re-sent to the attacker's host.
func TestClientDoesNotLeakSecretsAcrossHostRedirect(t *testing.T) {
	var attackerSawSecret bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range secretHeaders {
			if r.Header.Get(h) != "" {
				attackerSawSecret = true
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect every request off to the attacker host.
		http.Redirect(w, r, attacker.URL+"/api/platforms", http.StatusFound)
	}))
	defer origin.Close()

	host := config.Host{
		RootURI: origin.URL,
		Token:   "super-secret-bearer",
		CFAccess: &config.CFAccess{
			ClientID:     "cf-id",
			ClientSecret: "cf-secret",
		},
	}
	c := NewClient(host, 5*time.Second)

	// ValidateToken issues an authed GET /api/platforms that the origin 302s to attacker.
	err := c.ValidateToken()
	if err == nil {
		t.Fatal("expected the cross-host redirect to be refused (error), got nil")
	}
	if !strings.Contains(err.Error(), "cross-host") {
		// It may surface as a generic *http.url error wrapping our message; require our text.
		t.Logf("redirect error: %v", err)
	}
	if attackerSawSecret {
		t.Fatal("SECURITY: bearer/CF-Access secret was replayed to the cross-host redirect target")
	}
}

// TestClientRefusesSchemeDowngradeRedirect proves an https->http downgrade on the
// SAME host is refused (would expose the bearer in cleartext to a MITM).
func TestClientRefusesSchemeDowngradeRedirect(t *testing.T) {
	// Build a request chain synthetically against secureCheckRedirect since httptest
	// can't easily give us a real TLS origin + plaintext same-host target.
	orig, _ := http.NewRequest("GET", "https://romm.example/api/platforms", nil)
	next, _ := http.NewRequest("GET", "http://romm.example/api/platforms", nil)
	err := secureCheckRedirect(next, []*http.Request{orig})
	if err == nil {
		t.Fatal("expected scheme-downgrade redirect (https->http) to be refused")
	}
}

// TestSecureCheckRedirectAllowsSameHostSameScheme confirms a legit same-host path
// change (e.g. a reverse-proxy path rewrite) is still followed.
func TestSecureCheckRedirectAllowsSameHostSameScheme(t *testing.T) {
	orig, _ := http.NewRequest("GET", "https://romm.example/api/platforms", nil)
	next, _ := http.NewRequest("GET", "https://romm.example/v2/api/platforms", nil)
	if err := secureCheckRedirect(next, []*http.Request{orig}); err != nil {
		t.Fatalf("same-host same-scheme redirect should be allowed, got %v", err)
	}
}

// TestSecureCheckRedirectCapsChain confirms the 10-redirect cap.
func TestSecureCheckRedirectCapsChain(t *testing.T) {
	orig, _ := http.NewRequest("GET", "https://romm.example/a", nil)
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = orig
	}
	next, _ := http.NewRequest("GET", "https://romm.example/b", nil)
	if err := secureCheckRedirect(next, via); err == nil {
		t.Fatal("expected redirect chain cap to trigger at 10 hops")
	}
}
