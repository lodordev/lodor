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
// RomM host.
type Client struct {
	baseURL    string
	authHeader string
	httpClient *http.Client
}

// NewClient builds a Client for the given host with the supplied request timeout.
// InsecureSkipVerify on the host installs a TLS-skipping transport.
func NewClient(host config.Host, timeout time.Duration) *Client {
	hc := &http.Client{Timeout: timeout}
	if host.InsecureSkipVerify {
		hc.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return &Client{
		baseURL:    strings.TrimSuffix(host.URL(), "/"),
		authHeader: host.AuthHeader(),
		httpClient: hc,
	}
}

// authorize sets the Authorization header if one is configured.
func (c *Client) authorize(req *http.Request) {
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
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
