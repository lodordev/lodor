package main

import (
	"net/url"
	"os"
	"strconv"
	"strings"
)

// The two /tmp side-channels the native launcher reads while a long mode runs
// (BLUEPRINT §8, §10). They are best-effort: a write failure never changes a mode's
// RESULT line or exit code — the launcher's bar/label is cosmetic, the RESULT is the
// real gate.

const (
	progressPath = "/tmp/dl-progress" // integer percent 0..100, newline-terminated
	phasePath    = "/tmp/romm-phase"  // one-line human label, newline-terminated
)

// writeProgress writes an integer percent (0..100) to /tmp/dl-progress for the
// launcher's progress bar. Out-of-range values are clamped so the launcher never
// sees a nonsense bar position.
func writeProgress(pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	_ = os.WriteFile(progressPath, []byte(strconv.Itoa(pct)+"\n"), 0o644)
}

// writePhase writes a one-line human phase label to /tmp/romm-phase for the
// launcher's overlay ("Downloading <game>…", "Verifying…", "Uploading 2/5…"). The
// label is host-free by construction — callers pass game/file names, never URLs.
func writePhase(label string) {
	_ = os.WriteFile(phasePath, []byte(label+"\n"), 0o644)
}

// safeErr renders an error as a SHORT, HOST-FREE string for a stderr diagnostic.
// The romm client wraps transport errors with the full request URL
// (e.g. `Get "https://HOST/api/..."`), which would leak the real host into a log
// line — a hard security gate violation. safeErr strips any URL token from the
// message and collapses transport failures to a generic phrase, so a diagnostic
// never echoes the host, token, or device_id. It is the cmd-layer analogue of the
// sync package's (unexported) cleanErr.
func safeErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// Transport/DNS/TLS failures: the message embeds the URL — collapse to generic.
	low := strings.ToLower(s)
	for _, frag := range []string{
		"dial tcp", "no such host", "connection refused", "connection reset",
		"i/o timeout", "deadline exceeded", "tls", "x509", "eof",
		"execute request", "context deadline",
	} {
		if strings.Contains(low, frag) {
			return "network error"
		}
	}
	// Otherwise scrub any URL tokens (scheme://host/...) from the text, defensively.
	var out []string
	for _, tok := range strings.Fields(s) {
		t := strings.Trim(tok, "\"'<>(),")
		if u, perr := url.Parse(t); perr == nil && u.Host != "" {
			out = append(out, "<redacted-url>")
			continue
		}
		out = append(out, tok)
	}
	res := strings.Join(out, " ")
	if len(res) > 80 {
		res = res[:80]
	}
	return res
}

// b2i maps a bool to the 0|1 the RESULT contracts print.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
