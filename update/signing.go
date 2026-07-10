package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// trustedPubKeyHex is the OFFLINE ed25519 public key whose matching private key
// (out of repo, on the release host at /mnt/user/appdata/lodor/, backed up to
// titan) signs versions.json. Embedding the public key here is the whole point
// of the feature: a device trusts a manifest because it carries a signature
// from THIS key, not because it arrived from github.io. Compromising gh-pages
// or the publishing token no longer lets an attacker forge an accepted update
// (security HIGH finding #4).
const trustedPubKeyHex = "71498c04b8d318579d4fefe6c9064b8f67345ed2df1c34b7bd8d950eda75a6e5"

// TrustedPubKey is trustedPubKeyHex decoded once at init. A malformed constant
// is a build-time-caught programmer error, so a decode failure panics rather
// than silently disabling verification.
var TrustedPubKey = mustDecodePubKey(trustedPubKeyHex)

func mustDecodePubKey(h string) ed25519.PublicKey {
	b, err := hex.DecodeString(h)
	if err != nil {
		panic(fmt.Sprintf("update: trusted pubkey hex is invalid: %v", err))
	}
	if len(b) != ed25519.PublicKeySize {
		panic(fmt.Sprintf("update: trusted pubkey is %d bytes, want %d", len(b), ed25519.PublicKeySize))
	}
	return ed25519.PublicKey(b)
}

// SigMode selects how a manifest signature is treated. Build-time const:
//
//	"off"     — do not fetch or verify the .sig at all (pre-signing behaviour).
//	"warn"    — fetch + verify, LOG the result, but a missing/invalid signature
//	            NEVER blocks the update (behaves exactly as before signing existed).
//	"enforce" — a missing or invalid signature REFUSES the manifest.
//
// SHIPPING VALUE: "warn". This feature can only ADD safety; it must not break
// the live update path on a fleet that has never seen a signature. Flip to
// "enforce" ONLY after the signing keys are confirmed working on real hardware
// AND the maintainer signs off — at that point change the one line below to:
//
//	const SigMode = "enforce"
//
// (the single, deliberate go/no-go switch for hard signature enforcement).
const SigMode = "warn"

// ErrBadSignature is returned when a signature is well-formed (64 bytes) but
// does not verify against TrustedPubKey over the manifest bytes — i.e. the
// manifest was tampered with or signed by the wrong key.
var ErrBadSignature = fmt.Errorf("update: manifest signature does not verify against the trusted key")

// ErrMalformedSignature is returned when the signature blob cannot even be
// interpreted as an ed25519 signature (bad base64, or not 64 bytes). It is kept
// distinct from ErrBadSignature so callers/tests can tell "garbage sig file"
// from "valid sig, wrong content".
var ErrMalformedSignature = fmt.Errorf("update: manifest signature is malformed")

// VerifyManifestSig verifies a detached ed25519 signature (base64, as written
// to versions.json.sig by the release signer) over the EXACT raw manifest
// bytes. It must be handed the identical bytes that were fetched for
// versions.json — ed25519 signs bytes, not JSON semantics, so any
// re-serialization (json.Marshal of a parsed struct) would change the bytes and
// fail an otherwise-valid signature. Callers capture the raw response body,
// verify it here, and only THEN json.Unmarshal that same slice.
func VerifyManifestSig(manifestRaw, sigB64 []byte) error {
	// base64.StdEncoding.DecodeString tolerates a trailing newline poorly, so
	// trim whitespace the publisher's shell (or an editor) may have appended to
	// the .sig file before decoding.
	sig, err := base64.StdEncoding.DecodeString(trimASCIISpace(sigB64))
	if err != nil {
		return fmt.Errorf("%w: base64: %v", ErrMalformedSignature, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: %d bytes, want %d", ErrMalformedSignature, len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(TrustedPubKey, manifestRaw, sig) {
		return ErrBadSignature
	}
	return nil
}

// trimASCIISpace strips leading/trailing ASCII whitespace from a base64 blob
// without pulling in strings (this file stays dependency-light) and returns a
// string for DecodeString.
func trimASCIISpace(b []byte) string {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return string(b[i:j])
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
