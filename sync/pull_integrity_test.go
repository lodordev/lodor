package sync

// The writeSave INTEGRITY GATE: downloaded bytes are fingerprinted against the save
// record's content_hash BEFORE any filesystem mutation. Before the gate, only a
// zero-length body was rejected, so a truncated-but-non-empty transfer overwrote the
// live save and the caller then anchored the ledger to bytes the server never held.
//
// The invariant these tests pin: on a hash mismatch NOTHING on the card moves — no
// .bak rename, no .tmp, no write — and the outcome is an honest PullError.

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lodor/romm"
)

// contentServer serves body at GET /api/saves/{id}/content and 404s everything else
// (the version probe included, so the best-effort confirm stays a no-op).
func contentServer(body string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saves/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})
	return httptest.NewServer(mux)
}

func md5Of(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hashPtr(s string) *string { return &s }

// A record whose content_hash does NOT match the delivered bytes must abort with the
// existing local save byte-identical and no .bak left behind.
func TestWriteSaveRejectsHashMismatchAndLeavesLocalIntact(t *testing.T) {
	srv := contentServer("TRUNCATED")
	defer srv.Close()
	cfg := cfgFor(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	dir := t.TempDir()
	local := filepath.Join(dir, "game.sav")
	const good = "REAL-LOCAL-PROGRESS"
	if err := os.WriteFile(local, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}

	save := romm.Save{ID: 100, ContentHash: hashPtr(md5Of("THE-WHOLE-FILE"))}
	res := writeSave(c, cfg, save, local)

	if res.Outcome != PullError {
		t.Fatalf("hash mismatch must abort the write; got outcome %v reason=%q", res.Outcome, res.Reason)
	}
	if res.Reason != "corrupt download (hash mismatch)" {
		t.Fatalf("reason should name the corruption honestly, got %q", res.Reason)
	}
	b, err := os.ReadFile(local)
	if err != nil || string(b) != good {
		t.Fatalf("live save must be untouched by a rejected download: %q err=%v", string(b), err)
	}
	// The old code renamed the live file to .bak BEFORE writing. Nothing may move now.
	if _, err := os.Stat(local + ".bak"); !os.IsNotExist(err) {
		t.Fatalf(".bak must not exist — the local save was never moved (stat err=%v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("save dir must hold only the untouched save, got %v", names)
	}
}

// The happy path still writes: bytes whose MD5 equals content_hash are applied.
func TestWriteSaveAppliesMatchingHash(t *testing.T) {
	const body = "GOOD-SERVER-SAVE"
	srv := contentServer(body)
	defer srv.Close()
	cfg := cfgFor(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	local := filepath.Join(t.TempDir(), "game.sav")
	save := romm.Save{ID: 100, ContentHash: hashPtr(md5Of(body))}
	res := writeSave(c, cfg, save, local)

	if res.Outcome != PullWritten {
		t.Fatalf("a matching hash must write; got %v reason=%q", res.Outcome, res.Reason)
	}
	if b, err := os.ReadFile(local); err != nil || string(b) != body {
		t.Fatalf("bytes not written: %q err=%v", string(b), err)
	}
}

// Honest by omission (the AlreadyOnServer / --list-saves rule): a record with no
// content_hash can't be checked, so the pull proceeds on the length guard alone
// rather than blocking every save on an older/hashless server.
func TestWriteSaveAllowsHashlessRecord(t *testing.T) {
	const body = "UNVERIFIABLE-BUT-REAL"
	srv := contentServer(body)
	defer srv.Close()
	cfg := cfgFor(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	for _, tc := range []struct {
		name string
		save romm.Save
	}{
		{"nil hash", romm.Save{ID: 100}},
		{"empty hash", romm.Save{ID: 100, ContentHash: hashPtr("")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := filepath.Join(t.TempDir(), "game.sav")
			if res := writeSave(c, cfg, tc.save, local); res.Outcome != PullWritten {
				t.Fatalf("hashless record must not block the pull; got %v reason=%q", res.Outcome, res.Reason)
			}
			if b, err := os.ReadFile(local); err != nil || string(b) != body {
				t.Fatalf("bytes not written: %q err=%v", string(b), err)
			}
		})
	}
}

// RomM hands back a lower-case hex digest, but the comparison is case-insensitive
// everywhere else in the engine (EqualFold) — keep it that way here.
func TestWriteSaveHashCompareIsCaseInsensitive(t *testing.T) {
	const body = "MIXED-CASE-HASH"
	srv := contentServer(body)
	defer srv.Close()
	cfg := cfgFor(srv.URL)
	c := romm.NewClient(cfg.ActiveHost(), 10*time.Second)

	local := filepath.Join(t.TempDir(), "game.sav")
	upper := md5Of(body)
	save := romm.Save{ID: 100, ContentHash: hashPtr(upperHex(upper))}
	if res := writeSave(c, cfg, save, local); res.Outcome != PullWritten {
		t.Fatalf("upper-case content_hash must still match; got %v reason=%q", res.Outcome, res.Reason)
	}
}

func upperHex(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}
