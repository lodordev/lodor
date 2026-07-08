// Package clocksync sets the system wall clock from the RomM server's Date header before
// the engine makes its real HTTPS requests. RTC-less handhelds (the Mini Plus on OnionOS,
// the Flip V2 on my355) boot to a garbage date (1970/2018). With the clock wrong, TLS
// certificate validation fails ("certificate is not yet valid") and EVERY RomM fetch dies —
// the exact failure that broke downloads on those devices. The shell set_clock can lose the
// race, so the engine fixes the clock itself, right before it needs the network.
//
// Security: the probe connection is clock-TOLERANT, not insecure. It still validates the
// certificate CHAIN against the CA pool AND the server hostname; it only pins the time check
// to the certificate's own validity window so a wrong system clock cannot fail it. A cert
// from an untrusted CA, or for the wrong host, is still rejected.
package clocksync

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// minYear is the floor below which we consider the clock implausible and worth fixing.
const minYear = 2024

// probeTimeout bounds the single Date-fetch request.
const probeTimeout = 10 * time.Second

// sane reports whether t is plausibly a real current time (so we leave the clock alone).
func sane(t time.Time) bool { return t.Year() >= minYear }

// setClock sets the system wall clock; the real implementation is per-build-tag in
// clocksync_settime*.go (Android forbids settimeofday to apps). A var so tests can stub
// it (settimeofday needs root and would mutate the test host's clock).

// nowFn is time.Now, indirected for tests.
var nowFn = time.Now

// verifyChainIgnoringTime validates the peer certificate chain in rawCerts against roots and
// serverName, but pins the validity-time check to the leaf's own NotBefore so the (possibly
// wrong) system clock is irrelevant. Chain trust and hostname are still fully enforced.
func verifyChainIgnoringTime(rawCerts [][]byte, roots *x509.CertPool, serverName string) error {
	if len(rawCerts) == 0 {
		return errors.New("clocksync: peer sent no certificates")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("clocksync: parse cert: %w", err)
		}
		certs = append(certs, c)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	leaf := certs[0]
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		DNSName:       serverName,
		CurrentTime:   leaf.NotBefore.Add(time.Hour), // inside the cert window; ignores system clock
	})
	return err
}

// clockTolerantTLS builds a tls.Config that trusts roots + enforces serverName but tolerates a
// wrong system clock (see verifyChainIgnoringTime). InsecureSkipVerify disables ONLY the stdlib
// default verification (which uses the system time); we replace it with our own chain+host check.
func clockTolerantTLS(roots *x509.CertPool, serverName string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyChainIgnoringTime(rawCerts, roots, serverName)
		},
	}
}

// parseDate parses an HTTP Date header value (RFC1123 et al). Thin wrapper for testability.
func parseDate(s string) (time.Time, error) { return http.ParseTime(s) }

// fetchServerDate opens a clock-tolerant connection to baseURL and returns the server's Date.
// When insecure is set (the user's explicit insecure_skip_verify), verification is skipped
// entirely, matching the main client's behavior.
func fetchServerDate(baseURL string, insecure bool) (time.Time, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return time.Time{}, fmt.Errorf("clocksync: bad base url: %w", err)
	}
	var tlsCfg *tls.Config
	if insecure {
		tlsCfg = &tls.Config{InsecureSkipVerify: true}
	} else {
		roots, _ := x509.SystemCertPool() // honors SSL_CERT_FILE (the CA bundle shipped in the pak)
		if roots == nil {
			roots = x509.NewCertPool()
		}
		tlsCfg = clockTolerantTLS(roots, u.Hostname())
	}
	client := &http.Client{
		Timeout:   probeTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	resp, err := client.Get(baseURL)
	if err != nil {
		return time.Time{}, fmt.Errorf("clocksync: probe: %w", err)
	}
	defer resp.Body.Close()
	d := resp.Header.Get("Date")
	if d == "" {
		return time.Time{}, errors.New("clocksync: server sent no Date header")
	}
	return parseDate(d)
}

// Ensure sets the system clock from the RomM server's Date header IFF the local clock is
// implausible (year < minYear). It is a no-op — and makes no network call — when the clock is
// already sane. Best-effort: it returns any error for the caller to log, but a failure is no
// worse than the pre-existing wrong clock, so callers must not treat it as fatal.
func Ensure(baseURL string, insecure bool) error {
	if sane(nowFn()) {
		return nil
	}
	t, err := fetchServerDate(baseURL, insecure)
	if err != nil {
		return err
	}
	if !sane(t) {
		return fmt.Errorf("clocksync: server Date implausible (%s)", t.Format(time.RFC3339))
	}
	return setClock(t)
}
