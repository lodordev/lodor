package update

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkZip builds an in-memory zip of name->content entries.
func mkZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serveBytes(t *testing.T, b []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestStageHappyPath(t *testing.T) {
	zipBytes := mkZip(t, map[string]string{
		"lodor-sync":       "fake-binary",
		"lib/some-lib.sh":  "echo lib",
	})
	srv := serveBytes(t, zipBytes)
	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)), SHA256: sum(zipBytes)}
	if err := Stage(asset, dir, "0.9.9", time.Minute); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	for _, p := range []string{
		filepath.Join(dir, ReadyFileName),
		filepath.Join(dir, "tree", "lodor-sync"),
		filepath.Join(dir, "tree", "lib", "some-lib.sh"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s after Stage: %v", p, err)
		}
	}
	ready, _ := os.ReadFile(filepath.Join(dir, ReadyFileName))
	if !bytes.Contains(ready, []byte("version=0.9.9")) {
		t.Errorf("READY missing version stamp: %q", ready)
	}
	if _, err := os.Stat(filepath.Join(dir, "download.zip")); !os.IsNotExist(err) {
		t.Errorf("download.zip should be removed after a good stage")
	}
}

func TestStageHashMismatchRemovesStaging(t *testing.T) {
	zipBytes := mkZip(t, map[string]string{"lodor-sync": "fake"})
	srv := serveBytes(t, zipBytes)
	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)), SHA256: sum([]byte("something else"))}
	if err := Stage(asset, dir, "0.9.9", time.Minute); err != ErrHashMismatch {
		t.Fatalf("Stage = %v, want ErrHashMismatch", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("staging dir must be removed on hash mismatch")
	}
}

func TestStageSizeMismatchIsHashMismatch(t *testing.T) {
	zipBytes := mkZip(t, map[string]string{"lodor-sync": "fake"})
	srv := serveBytes(t, zipBytes)
	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)) + 5, SHA256: sum(zipBytes)}
	if err := Stage(asset, dir, "0.9.9", time.Minute); err != ErrHashMismatch {
		t.Fatalf("Stage = %v, want ErrHashMismatch on size mismatch", err)
	}
}

func TestStageZipSlipRejected(t *testing.T) {
	// hand-build a zip with a ../ escape entry
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.CreateHeader(&zip.FileHeader{Name: "../evil.sh"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("evil"))
	_ = w.Close()
	zipBytes := buf.Bytes()

	srv := serveBytes(t, zipBytes)
	parent := t.TempDir()
	dir := filepath.Join(parent, "inner", StageDirName)
	asset := Asset{URL: srv.URL, Size: int64(len(zipBytes)), SHA256: sum(zipBytes)}
	if err := Stage(asset, dir, "0.9.9", time.Minute); err == nil {
		t.Fatal("Stage accepted a zip-slip archive")
	}
	if _, err := os.Stat(filepath.Join(parent, "inner", "evil.sh")); !os.IsNotExist(err) {
		t.Errorf("zip-slip file escaped the staging tree")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("staging dir must be removed on extract failure")
	}
}

func TestStageUnreachableHost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), StageDirName)
	asset := Asset{URL: "http://127.0.0.1:1/nope.zip", Size: 1, SHA256: "00"}
	if err := Stage(asset, dir, "0.9.9", 2*time.Second); err == nil {
		t.Fatal("Stage succeeded against a dead host")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("staging dir must be removed on download failure")
	}
}

func TestFetchManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"schema": 1,
			"stable": {"version": "0.9.5", "notes": "fixes", "assets": {"lodoros-miyoomini": {"url": "https://x/z.zip", "size": 10, "sha256": "ab"}}},
			"beta":   {"version": "0.9.6-beta", "notes": "try me", "assets": {}}
		}`))
	}))
	defer srv.Close()
	m, err := FetchManifest(srv.URL)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if m.Stable == nil || m.Stable.Version != "0.9.5" {
		t.Errorf("stable channel wrong: %+v", m.Stable)
	}
	if got := m.ChannelFor("beta").Version; got != "0.9.6-beta" {
		t.Errorf("ChannelFor(beta) = %s", got)
	}
	if got := m.ChannelFor("garbage").Version; got != "0.9.5" {
		t.Errorf("unknown channel must degrade to stable, got %s", got)
	}
}

func TestFetchManifestRejectsWrongSchema(t *testing.T) {
	srv := serveBytes(t, []byte(`{"schema": 2}`))
	if _, err := FetchManifest(srv.URL); err == nil {
		t.Fatal("accepted unknown schema")
	}
}

func TestFetchManifestRejectsGarbage(t *testing.T) {
	srv := serveBytes(t, []byte(`not json at all`))
	if _, err := FetchManifest(srv.URL); err == nil {
		t.Fatal("accepted non-JSON manifest")
	}
}
