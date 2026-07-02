package romm

// Token-expiry / revocation honesty (task #123 deliverable 2). An expired or
// revoked client-token used to surface as the generic "API error: status 401"
// string, indistinguishable from any other server error — so the launcher could
// only say "sync failed" when the real fix is "re-pair this device". This file
// gives that condition a TYPED error the cmd layer maps to the distinct
// PAIRING_EXPIRED contract (stdout line + exit code 6).
//
// Classification (deliberately narrow — a transient server misconfig must NOT
// look like a dead pairing):
//   - 401 Unauthorized  → always an AuthError (the bearer was rejected).
//   - 403 Forbidden     → an AuthError ONLY when the body blames the token /
//     credentials themselves (invalid/expired/revoked). A plain 403 — a scope or
//     permission problem, or Cloudflare Access rejecting at the edge — stays a
//     generic error, because re-pairing would not fix it.
//
// The cmd layer NEVER deletes config.json on an AuthError — it only reports
// distinctly, so the pairing survives a transient server misconfig.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// AuthError marks a request the server rejected because the bearer token is
// invalid, expired, or revoked — the "re-pair this device" condition. It carries
// only the status code: never the body, URL, token, or host (security gate).
type AuthError struct {
	StatusCode int
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("auth rejected (status %d) — pairing expired or revoked", e.StatusCode)
}

// IsAuthError reports whether err (anywhere in its chain) is an *AuthError.
// The cmd layer's PAIRING_EXPIRED mapping switches on this.
func IsAuthError(err error) bool {
	var ae *AuthError
	return errors.As(err, &ae)
}

// authErrorFromStatus classifies a non-2xx response: it returns an *AuthError
// for a 401, or for a 403 whose body blames the token/credentials; otherwise
// nil (the caller falls back to its generic error). body may be a bounded
// snippet — only fragment sniffing is done on it.
func authErrorFromStatus(status int, body []byte) error {
	if status == http.StatusUnauthorized {
		return &AuthError{StatusCode: status}
	}
	if status == http.StatusForbidden && tokenInvalidBody(body) {
		return &AuthError{StatusCode: status}
	}
	return nil
}

// tokenInvalidBody sniffs a 403 body for the shapes RomM/FastAPI use when the
// TOKEN ITSELF is at fault ("invalid token", "token expired", "could not
// validate credentials", "not authenticated"), as opposed to a scope/permission
// 403 ("forbidden", "insufficient permissions") or a Cloudflare Access edge
// 403 — neither of which a re-pair would fix.
func tokenInvalidBody(body []byte) bool {
	s := strings.ToLower(string(body))
	if !strings.Contains(s, "token") && !strings.Contains(s, "credential") && !strings.Contains(s, "authenticated") {
		return false
	}
	for _, frag := range []string{"invalid", "expired", "revoked", "could not validate", "not authenticated"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}
