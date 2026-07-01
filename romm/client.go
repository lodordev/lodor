// Package romm is a from-scratch, CGO-free HTTP client for the RomM API. It speaks
// the exact wire format the Lodor sync engine relies on (BLUEPRINT §1, §9): bearer
// or basic auth on every request, hand-built query strings, and a multipart save
// upload whose single file field is named "saveFile".
//
// Only the stdlib is used. No third-party query encoder, no sqlite, no CGO.
package romm

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
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
	if host.Socks5Proxy == "" && !skipTLS {
		return nil
	}
	tr := &http.Transport{}
	if skipTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if host.Socks5Proxy != "" {
		tr.DialContext = socks5DialContext(host.Socks5Proxy)
	}
	return tr
}

// NewClient builds a Client for the given host with the supplied request timeout.
// The transport is derived from the host by hostTransport: a tier-1 host with a
// socks5_proxy dials every connection through that local SOCKS5 proxy with remote
// DNS (the Tailscale path), and InsecureSkipVerify installs a TLS-skipping config;
// a plain public host gets the stdlib default transport (nil). A non-nil
// host.CFAccess (tier-2 public endpoint) installs the CF-Access service-token pair
// so every request to this host carries the CF-Access-Client-* headers.
func NewClient(host config.Host, timeout time.Duration) *Client {
	hc := &http.Client{Timeout: timeout}
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
	hc := &http.Client{Timeout: timeout}
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
		return fmt.Errorf("API error: status %d, body: %s", resp.StatusCode, string(raw))
	}

	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// doRaw performs an authenticated GET and returns the full response body. The path
// is run through url.URL so spaces (e.g. in fs_name) are %20-escaped on the wire
// while other already-encoded sequences are preserved.
func (c *Client) doRaw(method, path string) ([]byte, error) {
	full, err := buildURL(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("build url: %w", err)
	}

	req, err := http.NewRequest(method, full, nil)
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
