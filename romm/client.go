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
	"os"
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
		// Return a typed *StatusError (Error() is byte-identical to the old fmt.Errorf
		// message, so message-matching callers are unaffected) so callers can branch on
		// Code — e.g. a save record that exists but whose content GET = 404 (a "ghost"
		// save: metadata row present, bytes missing).
		return nil, &StatusError{Code: resp.StatusCode, Body: string(raw)}
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
