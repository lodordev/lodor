package sync

// Unit tests for the tiered upload-verification decision (verifyStored) and the
// ghost-proof local guard. Pure — no server, no filesystem beyond t.TempDir.

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"lodor/romm"
)

func md5of(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func strp(s string) *string { return &s }

var (
	saveBytes = []byte("SAVEDATA-0123456789")
	saveMD5   = md5of(saveBytes)
	saveSize  = int64(len(saveBytes))
)

func noFetch(id int) ([]byte, error)  { return nil, fmt.Errorf("fetch should not run") }
func noList() ([]string, error)       { return nil, fmt.Errorf("list should not run") }
func fetchOK(id int) ([]byte, error)  { return saveBytes, nil }
func fetchBad(id int) ([]byte, error) { return []byte("DIFFERENT"), nil }

func TestVerifyStoredTier1ResponseHash(t *testing.T) {
	// Matching content_hash + non-zero stored size: verified for FREE, no requests.
	up := romm.Save{ID: 7, ContentHash: strp(saveMD5), FileSizeBytes: saveSize}
	if err := verifyStored(up, saveMD5, saveSize, noFetch, noList); err != nil {
		t.Fatalf("tier1 verified upload rejected: %v", err)
	}
}

func TestVerifyStoredTier1GhostShapeFallsToBytes(t *testing.T) {
	// Hash matches but the record claims ZERO stored bytes — the ghost shape must
	// NOT pass on the hash alone; the byte re-download decides.
	up := romm.Save{ID: 7, ContentHash: strp(saveMD5), FileSizeBytes: 0}
	if err := verifyStored(up, saveMD5, saveSize, fetchOK, noList); err != nil {
		t.Fatalf("zero-size record with good server bytes should verify via tier2: %v", err)
	}
	// Same shape but the server holds nothing → must FAIL (this is a minted ghost).
	empty := func(id int) ([]byte, error) { return nil, nil }
	if err := verifyStored(up, saveMD5, saveSize, empty, noList); err == nil {
		t.Fatal("zero-size record with EMPTY server bytes verified — ghost slipped through")
	}
}

func TestVerifyStoredTier2ByteCheck(t *testing.T) {
	// No hash in the create response → the uploaded record's bytes are re-downloaded.
	up := romm.Save{ID: 7}
	if err := verifyStored(up, saveMD5, saveSize, fetchOK, noList); err != nil {
		t.Fatalf("tier2 byte-verified upload rejected: %v", err)
	}
	if err := verifyStored(up, saveMD5, saveSize, fetchBad, noList); err == nil {
		t.Fatal("tier2 mismatched server bytes verified — corruption would be marked synced")
	}
}

func TestVerifyStoredTier1MismatchDecidedByBytes(t *testing.T) {
	// Response hash DIFFERS from local — tier 2 is the tiebreaker, both directions.
	up := romm.Save{ID: 7, ContentHash: strp("00000000000000000000000000000000"), FileSizeBytes: saveSize}
	if err := verifyStored(up, saveMD5, saveSize, fetchOK, noList); err != nil {
		t.Fatalf("stale response hash but good server bytes should verify: %v", err)
	}
	if err := verifyStored(up, saveMD5, saveSize, fetchBad, noList); err == nil {
		t.Fatal("hash mismatch + byte mismatch verified")
	}
}

func TestVerifyStoredTier3ListFallback(t *testing.T) {
	// No response hash, no usable ID → only the list fallback can vouch.
	up := romm.Save{}
	listHit := func() ([]string, error) { return []string{"aa", saveMD5}, nil }
	listMiss := func() ([]string, error) { return []string{"aa"}, nil }
	if err := verifyStored(up, saveMD5, saveSize, noFetch, listHit); err != nil {
		t.Fatalf("tier3 list-hash match rejected: %v", err)
	}
	if err := verifyStored(up, saveMD5, saveSize, noFetch, listMiss); err == nil {
		t.Fatal("tier3 with no matching hash verified")
	}
	// Fetch error (transport blip) falls through to the list.
	fetchErr := func(id int) ([]byte, error) { return nil, fmt.Errorf("timeout") }
	up = romm.Save{ID: 7}
	if err := verifyStored(up, saveMD5, saveSize, fetchErr, listHit); err != nil {
		t.Fatalf("tier2 fetch error should fall to tier3 list match: %v", err)
	}
}

func TestVerifyStoredAuthErrorSurfaces(t *testing.T) {
	// A 401 during verification must surface as an AuthError (PAIRING_EXPIRED map),
	// not be swallowed into a generic mismatch.
	authErr := &romm.AuthError{StatusCode: 401}
	fetch401 := func(id int) ([]byte, error) { return nil, authErr }
	list401 := func() ([]string, error) { return nil, authErr }
	up := romm.Save{ID: 7}
	if err := verifyStored(up, saveMD5, saveSize, fetch401, list401); !romm.IsAuthError(err) {
		t.Fatalf("verify under 401: IsAuthError = false (err=%v)", err)
	}
}

func TestEmptyLocalSaveGuard(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.sav")
	real := filepath.Join(dir, "real.sav")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, saveBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if !emptyLocalSave(empty) {
		t.Error("0-byte save not flagged — it would upload and mint a server ghost")
	}
	if !emptyLocalSave(filepath.Join(dir, "missing.sav")) {
		t.Error("missing file not flagged")
	}
	if !emptyLocalSave(dir) {
		t.Error("directory not flagged")
	}
	if emptyLocalSave(real) {
		t.Error("real save flagged as empty")
	}
}
