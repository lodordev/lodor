package romm

// States client tests (Handoff v1). httptest at the wire level: multipart field
// names, query params, the D1 version gate, and the raw-assets encoding against
// the EXACT download_path shape production 4.9.2 serves (spaces in path AND in
// the timestamp query value).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lodor/config"
)

const prodDownloadPath = "/api/raw/assets/users/557365723a32/states/gbc/13528/builtin/Legend of Zelda, The - Oracle of Ages (USA, Australia) [2026-07-01_00-34-06].state.auto?timestamp=2026-07-01 03:44:03"

func TestEncodeRawAssetPath(t *testing.T) {
	enc, err := encodeRawAssetPath(prodDownloadPath)
	if err != nil {
		t.Fatal(err)
	}
	// Path stays RAW (buildURL escapes it exactly once); only the query encodes.
	if !strings.HasPrefix(enc, "/api/raw/assets/users/557365723a32/states/gbc/13528/builtin/Legend of Zelda") {
		t.Fatalf("path should remain raw for buildURL: %q", enc)
	}
	if strings.Contains(enc[strings.IndexByte(enc, '?'):], " ") {
		t.Fatalf("query still has spaces: %q", enc)
	}
	if !strings.Contains(enc, "?timestamp=2026-07-01+03%3A44%3A03") &&
		!strings.Contains(enc, "?timestamp=2026-07-01%2003%3A44%3A03") {
		t.Fatalf("query not encoded: %q", enc)
	}
	if _, err := encodeRawAssetPath(""); err == nil {
		t.Fatal("empty download_path accepted")
	}
}

// stateServer serves heartbeat (version configurable), states list/upload/delete,
// and both content routes, recording what was hit.
func stateServer(t *testing.T, version string, hits *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"SYSTEM": map[string]string{"VERSION": version}})
	})
	mux.HandleFunc("/api/states/delete", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*hits = append(*hits, "delete:"+string(body))
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/states/77/content", func(w http.ResponseWriter, r *http.Request) {
		*hits = append(*hits, "content-route")
		w.Write([]byte("STATEBYTES-50"))
	})
	mux.HandleFunc("/api/states", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart parse: %v", err)
			}
			f, hdr, err := r.FormFile("stateFile")
			if err != nil {
				t.Errorf("stateFile field missing: %v", err)
				w.WriteHeader(400)
				return
			}
			data, _ := io.ReadAll(f)
			*hits = append(*hits, "upload:"+r.URL.Query().Get("rom_id")+":"+r.URL.Query().Get("emulator")+":"+hdr.Filename+":"+string(data))
			_ = json.NewEncoder(w).Encode(State{ID: 99, RomID: 9752, FileName: hdr.Filename})
			return
		}
		*hits = append(*hits, "list:"+r.URL.Query().Get("rom_id"))
		_ = json.NewEncoder(w).Encode([]State{{ID: 77, RomID: 9752, FileName: "x.state", DownloadPath: prodDownloadPath, FileSizeBytes: 13}})
	})
	// raw-assets catch-all: must receive the ENCODED path
	mux.HandleFunc("/api/raw/assets/", func(w http.ResponseWriter, r *http.Request) {
		*hits = append(*hits, "raw-route:"+r.URL.Path)
		w.Write([]byte("STATEBYTES-49"))
	})
	return httptest.NewServer(mux)
}

func stateClient(t *testing.T, url string) *Client {
	t.Helper()
	return NewClient(config.Host{RootURI: url, Token: "t"}, 5*time.Second)
}

func TestStatesListUploadDelete(t *testing.T) {
	var hits []string
	srv := stateServer(t, "4.9.2", &hits)
	defer srv.Close()
	c := stateClient(t, srv.URL)

	states, err := c.GetStates(9752)
	if err != nil || len(states) != 1 || states[0].ID != 77 {
		t.Fatalf("list: %v %+v", err, states)
	}
	up, err := c.UploadState(9752, "lodor/knulli/gambatte@r838/arm64", "Woody [x] (lodor sauto devA).state", []byte("PAYLOAD"))
	if err != nil || up.ID != 99 {
		t.Fatalf("upload: %v %+v", err, up)
	}
	if err := c.DeleteStates([]int{99}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteStates(nil); err != nil {
		t.Fatal("empty delete must be a nil no-op")
	}
	joined := strings.Join(hits, "\n")
	if !strings.Contains(joined, "upload:9752:lodor/knulli/gambatte@r838/arm64:Woody [x] (lodor sauto devA).state:PAYLOAD") {
		t.Fatalf("upload wire wrong:\n%s", joined)
	}
	if !strings.Contains(joined, `delete:{"states":[99]}`) {
		t.Fatalf("delete body wrong:\n%s", joined)
	}
}

func TestDownloadGate492UsesEncodedRawRoute(t *testing.T) {
	var hits []string
	srv := stateServer(t, "4.9.2", &hits)
	defer srv.Close()
	c := stateClient(t, srv.URL)
	states, _ := c.GetStates(9752)
	data, err := c.DownloadStateContent(states[0])
	if err != nil || string(data) != "STATEBYTES-49" {
		t.Fatalf("download 4.9.2: %v %q", err, data)
	}
	joined := strings.Join(hits, "\n")
	if !strings.Contains(joined, "raw-route:/api/raw/assets/users/557365723a32/states/gbc/13528/builtin/Legend of Zelda, The - Oracle of Ages (USA, Australia) [2026-07-01_00-34-06].state.auto") {
		t.Fatalf("raw route not hit with decoded-server-side path:\n%s", joined)
	}
}

func TestDownloadGate500UsesContentRoute(t *testing.T) {
	var hits []string
	srv := stateServer(t, "5.0.0", &hits)
	defer srv.Close()
	c := stateClient(t, srv.URL)
	states, _ := c.GetStates(9752)
	data, err := c.DownloadStateContent(states[0])
	if err != nil || string(data) != "STATEBYTES-50" {
		t.Fatalf("download 5.0: %v %q", err, data)
	}
	if !strings.Contains(strings.Join(hits, "\n"), "content-route") {
		t.Fatal("5.0 content route not used")
	}
}
