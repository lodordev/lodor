package romm

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"net/http"

	"lodor/config"
)

// TestUploadSaveFileNameOverride locks the #126 wire contract: when
// UploadSaveQuery.FileName is set, the multipart part filename the server sees is
// that CANONICAL name — not the (marker-bearing) local basename. And when it is
// empty, the local basename still travels (back-compat).
func TestUploadSaveFileNameOverride(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "✓ Sonic (USA).gba.sav")
	if err := os.WriteFile(local, []byte("SAVEBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("multipart parse: %v", err)
		}
		if fhs := r.MultipartForm.File["saveFile"]; len(fhs) == 1 {
			gotFilename = fhs[0].Filename
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 9, "file_name": "x"}`))
	}))
	defer srv.Close()

	c := NewClient(config.Host{RootURI: srv.URL, Token: "t"}, 5*time.Second)

	if _, err := c.UploadSave(UploadSaveQuery{RomID: 1, FileName: "Sonic (USA).gba.sav"}, local); err != nil {
		t.Fatalf("upload (override): %v", err)
	}
	if gotFilename != "Sonic (USA).gba.sav" {
		t.Errorf("override: server saw filename %q, want canonical %q", gotFilename, "Sonic (USA).gba.sav")
	}

	if _, err := c.UploadSave(UploadSaveQuery{RomID: 1}, local); err != nil {
		t.Fatalf("upload (default): %v", err)
	}
	if gotFilename != "✓ Sonic (USA).gba.sav" {
		t.Errorf("default: server saw filename %q, want local basename", gotFilename)
	}
}
