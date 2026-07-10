// Package romm is a from-scratch, CGO-free HTTP client for the RomM API. It speaks
// the exact wire format the Lodor sync engine relies on (BLUEPRINT §1, §9): bearer
// or basic auth on every request, hand-built query strings, and a multipart save
// upload whose single file field is named "saveFile".
//
// Only the stdlib is used. No third-party query encoder, no sqlite, no CGO.
package romm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"lodor/config"
)

// Client wraps an *http.Client with the base URL and Authorization header for one
// RomM host. When the host is a tier-2 (public Cloudflare Access) endpoint it also
// carries the CF Access service-token pair, sent as two extra request headers.
type Client struct {
	baseURL    string
	authHeader string
	// cfClientID / cfClientSecret are the Cloudflare Access service-token pair for
	// a tier-2 (public) host. Empty on a tier-1 (Tailscale) host — authorize() then
	// sends NO CF headers. Set from host.CFAccess in NewClient.
	cfClientID     string
	cfClientSecret string
	httpClient     *http.Client

	// serverVer caches the RomM version string parsed from GET /api/heartbeat
	// (SYSTEM.VERSION), fetched at most once per Client via ServerVersion(). The
	// device_save_sync feature gate (>= 4.9.0) reads it so a per-save best-effort
	// call costs one heartbeat, not one per row. serverVerSet distinguishes "cached
	// empty" (heartbeat answered but omitted a version) from "never fetched".
	serverVer    string
	serverVerSet bool
}

// hostTransport builds the *http.Transport for a host, or returns nil to let the
// stdlib install its default (the public tier-2 / legacy single-host path, kept
// byte-identical). A transport is built ONLY when the host needs one:
//
//   - Socks5Proxy set (tier-1 Tailscale): every connection is dialed through the
//     local SOCKS5 proxy with REMOTE DNS, so the *.ts.net MagicDNS name is resolved
//     by tailscaled, not the system resolver. This is purely additive — a host with
//     no socks5_proxy never gets a SOCKS5 dialer.
//   - InsecureSkipVerify set: a TLS config that skips verification.
//
// Both can apply at once (a tier-1 host that also skips TLS verify). When neither
// applies the function returns nil and the caller leaves http.Client.Transport
// unset — identical to the pre-tier-1 behavior.
func hostTransport(host config.Host) *http.Transport {
	skipTLS := host.SkipTLSVerify()
	dnsServer := os.Getenv(dnsServerEnv)
	if host.Socks5Proxy == "" && !skipTLS && dnsServer == "" {
		return nil
	}
	tr := &http.Transport{}
	if skipTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if host.Socks5Proxy != "" {
		tr.DialContext = socks5DialContext(host.Socks5Proxy)
	} else if dnsServer != "" {
		tr.DialContext = dnsDialContext(dnsServer)
	}
	return tr
}

// secretHeaders are the request headers that MUST NOT follow a redirect off the
// origin host: the RomM bearer and the Cloudflare Access service-token pair. Go's
// default redirect handling REPLAYS all headers set on the original request to the
// redirect target, so a malicious/compromised RomM (or a MITM when TLS verification is
// skipped) that answers a request with a 3xx to an attacker Location would otherwise
// leak these credentials to the attacker's host.
var secretHeaders = []string{"Authorization", "CF-Access-Client-Id", "CF-Access-Client-Secret"}

// secureCheckRedirect returns a net/http CheckRedirect policy that keeps credentials
// on the origin only. It REFUSES a redirect that leaves the origin host or downgrades
// the scheme (https -> http), and caps the redirect chain at 10. Defensively it also
// strips the secret headers on any hop it does allow whose host differs from the
// origin (belt-and-suspenders — a refused hop never reaches this, but a future policy
// relaxation would still not leak). origHost/origScheme are taken from the request
// that STARTED the chain (via[0]) so every hop is compared to the true origin, not the
// immediately preceding (possibly attacker-chosen) URL.
func secureCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	orig := via[0].URL
	// Cross-host or scheme-downgrade: refuse outright so no credential-bearing request
	// is ever dispatched to the new location.
	if !strings.EqualFold(req.URL.Host, orig.Host) {
		return fmt.Errorf("refusing cross-host redirect to %q", req.URL.Host)
	}
	if orig.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing scheme-downgrade redirect to %q", req.URL.Scheme)
	}
	// Same host + scheme not weakened: allowed (a RomM behind a path change). Still, if
	// the host somehow differs by anything but case, strip secrets defensively.
	if req.URL.Host != orig.Host {
		for _, h := range secretHeaders {
			req.Header.Del(h)
		}
	}
	return nil
}

// dnsServerEnv names a DNS server ("ip" or "ip:port", port defaults to 53) that
// hostname lookups must use, overriding the system resolver. Exists for the
// Android lane: an exec'd pure-Go binary there has no /etc/resolv.conf, so Go's
// fallback resolver dials 127.0.0.1:53 and every hostname lookup fails even
// though the OS resolves fine — the app reads the live DNS server from Android's
// LinkProperties (100.100.100.100 when the Tailscale VPN is up) and exports it.
// Unset = stdlib behavior, byte-identical on every other lane. A host with a
// socks5_proxy never needs it (SOCKS5 does remote DNS).
const dnsServerEnv = "LODOR_DNS_SERVER"

// dnsDialContext returns a DialContext whose hostname lookups go through the
// given DNS server instead of the system resolver. TLS verification is untouched
// — the certificate is still checked against the HOSTNAME, so this is strictly a
// resolution override, not a trust change.
func dnsDialContext(server string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "53")
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, server)
		},
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, Resolver: resolver}
	return dialer.DialContext
}

// NewClient builds a Client for the given host with the supplied request timeout.
// The transport is derived from the host by hostTransport: a tier-1 host with a
// socks5_proxy dials every connection through that local SOCKS5 proxy with remote
// DNS (the Tailscale path), and InsecureSkipVerify installs a TLS-skipping config;
// a plain public host gets the stdlib default transport (nil). A non-nil
// host.CFAccess (tier-2 public endpoint) installs the CF-Access service-token pair
// so every request to this host carries the CF-Access-Client-* headers.
func NewClient(host config.Host, timeout time.Duration) *Client {
	hc := &http.Client{Timeout: timeout, CheckRedirect: secureCheckRedirect}
	if tr := hostTransport(host); tr != nil {
		hc.Transport = tr
	}
	c := &Client{
		baseURL:    strings.TrimSuffix(host.URL(), "/"),
		authHeader: host.AuthHeader(),
		httpClient: hc,
	}
	if host.CFAccess != nil {
		c.cfClientID = host.CFAccess.ClientID
		c.cfClientSecret = host.CFAccess.ClientSecret
	}
	return c
}

// authorize sets the Authorization header if one is configured, plus — for a
// tier-2 public host that carries a CF Access service token — the two
// CF-Access-Client-* headers. The CF headers are orthogonal to Authorization
// (Cloudflare consumes them at its edge and never touches Authorization); they
// are sent ONLY when both halves of the service token are present, so a tier-1
// Tailscale host (no cf_access block) sends none. Called by every request method.
//
// SECURITY: the secret is written ONLY into the live request header over the
// configured transport — never logged.
func (c *Client) authorize(req *http.Request) {
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	if c.cfClientID != "" && c.cfClientSecret != "" {
		req.Header.Set("CF-Access-Client-Id", c.cfClientID)
		req.Header.Set("CF-Access-Client-Secret", c.cfClientSecret)
	}
}

// ProbeReachableHost reports whether a RomM endpoint answers at all within timeout —
// the tier-1 (Tailscale) reachability check that drives endpoint selection. It uses
// the SAME transport as a real client (hostTransport): a tier-1 host is probed
// THROUGH its socks5_proxy with remote DNS, so "reachable" means "tailscaled is up
// and the .ts.net host answered over the tailnet" — exactly the condition that
// should select tier 1. It makes a cheap GET /api/heartbeat where ANY HTTP response
// (even a 401/403 from Cloudflare Access, or RomM's own 200) counts as reachable: we
// only care that the endpoint is ROUTABLE, not that auth passes. A transport error —
// no route off the tailnet, DNS failure, connection refused, timeout — returns false,
// so the engine falls back to the public tier. No auth or CF headers are sent (it must
// stay cheap and side-effect-free); the body is closed and discarded.
func ProbeReachableHost(host config.Host, timeout time.Duration) bool {
	// Same redirect posture as a real client even though this probe sends no auth/CF
	// headers: a cross-host/scheme-downgrade 3xx must not be silently followed (it would
	// mis-report an attacker-controlled host as "the RomM endpoint is reachable").
	hc := &http.Client{Timeout: timeout, CheckRedirect: secureCheckRedirect}
	if tr := hostTransport(host); tr != nil {
		hc.Transport = tr
	}
	full, err := buildURL(strings.TrimSuffix(host.URL(), "/"), "/api/heartbeat")
	if err != nil {
		return false
	}
	req, err := http.NewRequest("GET", full, nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// doJSON performs an authenticated request with an optional JSON body and decodes a
// JSON response into out (when out is non-nil and the status is not 204). A 409
// response is surfaced as a *ConflictError.
func (c *Client) doJSON(method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		return parseConflictError(raw)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		// Auth classification first (401 / token-invalid 403 -> *AuthError, which the
		// re-pair flow branches on); every other non-2xx is a *StatusError whose Error()
		// string is byte-identical to the old fmt.Errorf message, and whose Code lets
		// callers errors.As a 404 from an endpoint an older RomM doesn't expose.
		if aerr := authErrorFromStatus(resp.StatusCode, raw); aerr != nil {
			return aerr
		}
		return &StatusError{Code: resp.StatusCode, Body: string(raw)}
	}

	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// StatusError is a non-2xx (and non-409) HTTP response from the RomM API. Its
// Error() string is byte-identical to the fmt.Errorf message doJSON returned before
// this type existed, so any caller matching on the message text keeps working; the
// Code field lets newer callers branch on a specific status (e.g. a 404 from an
// endpoint an older RomM server doesn't expose -> graceful no-op) via errors.As.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("API error: status %d, body: %s", e.Code, e.Body)
}

// doRaw performs an authenticated GET and returns the full response body. The path
// is run through url.URL so spaces (e.g. in fs_name) are %20-escaped on the wire
// while other already-encoded sequences are preserved. It is doRawCtx with a
// background context — the httpClient.Timeout still bounds it exactly as before, so
// every existing caller's behaviour is byte-for-byte unchanged.
func (c *Client) doRaw(method, path string) ([]byte, error) {
	return c.doRawCtx(context.Background(), method, path)
}

// doRawCtx is doRaw wired to a caller-supplied context so an in-flight request can be
// CANCELLED (or bounded by a shorter deadline than the client-wide Timeout) from the
// outside. Used by the cover fetch: a per-cover timeout + the user's B-press cancel are
// carried in ctx, so a slow-radio cover download aborts the moment cancel fires instead
// of waiting out the network. On ctx cancellation httpClient.Do returns a context error,
// which propagates here as the request error — no bytes, no partial write.
func (c *Client) doRawCtx(ctx context.Context, method, path string) ([]byte, error) {
	full, err := buildURL(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("build url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if aerr := authErrorFromStatus(resp.StatusCode, raw); aerr != nil {
			return nil, aerr
		}
		return nil, fmt.Errorf("API error: status %d, body: %s", resp.StatusCode, string(raw))
	}
	return raw, nil
}

// doRawStreamTo performs an authenticated GET and STREAMS the response body to dst
// via io.Copy, returning the number of bytes written. Unlike doRaw it never buffers
// the whole body in memory — essential for multi-hundred-MB ROM files on a 128 MB
// device, where io.ReadAll of a 482 MB disc would OOM. On a non-2xx status it reads
// (only) a bounded error snippet so the failure is still host-free-diagnosable
// without buffering a large body. The caller owns dst (file create/sync/close).
// onProgress (nil-safe) is invoked as bytes arrive with (bytesWritten, totalBytes),
// where totalBytes is the response Content-Length (or -1 if the server omits it) —
// this drives the launcher's real download progress bar.
func (c *Client) doRawStreamTo(path string, dst io.Writer, onProgress func(done, total int64)) (int64, error) {
	full, err := buildURL(c.baseURL, path)
	if err != nil {
		return 0, fmt.Errorf("build url: %w", err)
	}
	req, err := http.NewRequest("GET", full, nil)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if aerr := authErrorFromStatus(resp.StatusCode, snippet); aerr != nil {
			return 0, aerr
		}
		return 0, fmt.Errorf("API error: status %d, body: %s", resp.StatusCode, string(snippet))
	}
	w := dst
	if onProgress != nil {
		w = &progressCountWriter{dst: dst, total: resp.ContentLength, cb: onProgress}
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, fmt.Errorf("stream response body: %w", err)
	}
	// Truncation guard: if the server declared a Content-Length, the streamed byte count MUST
	// match it. A connection that drops but still closes cleanly yields a short file — for a CHD
	// (whose container bytes we deliberately don't hash) that would otherwise pass as a successful
	// download and only fail at load time. Catch it here so a truncated ROM is never downloaded=1.
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return n, fmt.Errorf("short read: got %d of %d bytes", n, resp.ContentLength)
	}
	return n, nil
}

// progressCountWriter wraps a writer and reports cumulative bytes written to a
// callback after each Write, so a streamed download can drive a real progress bar
// without buffering. total is the known content length (or -1 if unknown).
type progressCountWriter struct {
	dst   io.Writer
	done  int64
	total int64
	cb    func(done, total int64)
}

func (p *progressCountWriter) Write(b []byte) (int, error) {
	n, err := p.dst.Write(b)
	p.done += int64(n)
	if p.cb != nil {
		p.cb(p.done, p.total)
	}
	return n, err
}

// doRawStreamResumeTo performs an authenticated GET that RESUMES a partial download.
// When startOffset > 0 it sends "Range: bytes=<startOffset>-" so the server returns
// only the remaining bytes (HTTP 206), which are APPENDED to f from startOffset; the
// progress callback is primed with startOffset already done so the bar never jumps
// backward. Three server behaviors are handled so a resume can never corrupt the file:
//
//   - 206 Partial Content: honor the range. f is truncated to startOffset (dropping
//     any bytes a previous run wrote past the requested offset) and the remainder is
//     appended. Final size MUST equal startOffset + Content-Length.
//   - 200 OK: the server ignored the Range header and is sending the WHOLE file from
//     byte 0 (older RomM / proxy that strips Range). f is truncated to 0 and rewritten
//     from scratch — the partial is discarded, so the result is always a clean file.
//   - 416 Range Not Satisfiable: our offset is at/beyond EOF (a stale or already-complete
//     .tmp). f is truncated to 0 and the whole file is re-fetched from byte 0 in a single
//     retry, so a bad partial self-heals instead of wedging forever.
//
// On any other non-2xx a bounded error snippet is read (never the whole body). The
// caller owns f (open/sync/close) and the final hash verify remains the ultimate gate.
func (c *Client) doRawStreamResumeTo(path string, f *os.File, startOffset int64, onProgress func(done, total int64)) (int64, error) {
	full, err := buildURL(c.baseURL, path)
	if err != nil {
		return 0, fmt.Errorf("build url: %w", err)
	}
	req, err := http.NewRequest("GET", full, nil)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	c.authorize(req)
	if startOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent: // 206 — resume: append from startOffset
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek for resume: %w", err)
		}
		if err := f.Truncate(startOffset); err != nil {
			return 0, fmt.Errorf("truncate for resume: %w", err)
		}
		var total int64 = -1
		if resp.ContentLength > 0 {
			total = startOffset + resp.ContentLength
		}
		w := io.Writer(f)
		if onProgress != nil {
			w = &progressCountWriter{dst: f, done: startOffset, total: total, cb: onProgress}
		}
		n, cErr := io.Copy(w, resp.Body)
		if cErr != nil {
			return startOffset + n, fmt.Errorf("stream response body: %w", cErr)
		}
		if total > 0 && startOffset+n != total {
			return startOffset + n, fmt.Errorf("short read: got %d of %d bytes", startOffset+n, total)
		}
		return startOffset + n, nil

	case http.StatusRequestedRangeNotSatisfiable: // 416 — stale/complete partial: restart from 0
		if startOffset == 0 {
			// A 416 for a request that sent no Range header means the server is broken;
			// refuse to loop — exactly one clean re-fetch is allowed.
			return 0, fmt.Errorf("API error: status 416 on full fetch")
		}
		resp.Body.Close()
		if err := f.Truncate(0); err != nil {
			return 0, fmt.Errorf("truncate for restart: %w", err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek for restart: %w", err)
		}
		return c.doRawStreamResumeTo(path, f, 0, onProgress)

	default:
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return 0, fmt.Errorf("API error: status %d, body: %s", resp.StatusCode, string(snippet))
		}
		// 200 (or any other 2xx) — full body from byte 0: discard the partial, rewrite clean.
		if err := f.Truncate(0); err != nil {
			return 0, fmt.Errorf("truncate for full: %w", err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek for full: %w", err)
		}
		w := io.Writer(f)
		if onProgress != nil {
			w = &progressCountWriter{dst: f, total: resp.ContentLength, cb: onProgress}
		}
		n, cErr := io.Copy(w, resp.Body)
		if cErr != nil {
			return n, fmt.Errorf("stream response body: %w", cErr)
		}
		if resp.ContentLength > 0 && n != resp.ContentLength {
			return n, fmt.Errorf("short read: got %d of %d bytes", n, resp.ContentLength)
		}
		return n, nil
	}
}

// doMultipart POSTs a multipart/form-data body with exactly one file field named
// "saveFile" (filename = fileName), appending the supplied query string to the URL.
// A 409 is surfaced as a *ConflictError; otherwise a JSON response is decoded into
// out.
func (c *Client) doMultipart(path string, query url.Values, fileField, fileName string, fileBytes []byte, out any) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(fileField, fileName)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(fileBytes); err != nil {
		return fmt.Errorf("write file part: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	full := c.baseURL + path
	if enc := query.Encode(); enc != "" {
		full += "?" + enc
	}

	req, err := http.NewRequest(http.MethodPost, full, &buf)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		return parseConflictError(raw)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		if aerr := authErrorFromStatus(resp.StatusCode, raw); aerr != nil {
			return aerr
		}
		return fmt.Errorf("API error: status %d, body: %s", resp.StatusCode, string(raw))
	}

	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// buildURL joins a base and a path, percent-escaping the path's spaces (and other
// reserved characters in path segments) while leaving the query intact. This keeps
// content URLs like /api/roms/{id}/content/{fs_name} valid when fs_name has spaces.
func buildURL(base, path string) (string, error) {
	rawQuery := ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		rawQuery = path[i+1:]
		path = path[:i]
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	// Append the path segments. Splitting on "/" and setting via url.URL ensures
	// each segment is individually escaped (spaces -> %20) without double-escaping.
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = rawQuery
	return u.String(), nil
}
