// FrontendMediaESDEDir gate tests: absent config, the esde style (case/space
// tolerant), and unknown styles failing toward "" (the harmless NextUI default).
// Plus the writer round-trip: WriteFrontendMedia sets frontend_media +
// fetch_covers together, preserves unknown keys, and "" clears both.
package config

import (
	"encoding/json"
	"os"
	"testing"
)

func TestFrontendMediaESDEDir(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil config", nil, ""},
		{"absent", &Config{}, ""},
		{"esde", &Config{FrontendMedia: &FrontendMediaConfig{Style: "esde", Dir: "/sd/ES-DE/downloaded_media"}}, "/sd/ES-DE/downloaded_media"},
		{"case+space", &Config{FrontendMedia: &FrontendMediaConfig{Style: " ESDE ", Dir: " /m "}}, "/m"},
		{"unknown style", &Config{FrontendMedia: &FrontendMediaConfig{Style: "cocoon", Dir: "/m"}}, ""},
		{"empty dir", &Config{FrontendMedia: &FrontendMediaConfig{Style: "esde", Dir: "  "}}, ""},
	}
	for _, c := range cases {
		if got := c.cfg.FrontendMediaESDEDir(); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestWriteFrontendMediaRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	// Seed a config with an unknown key the writer must not eat.
	seed := `{"launcher_custom":"keep-me","hosts":[{"root_uri":"https://x"}]}`
	if err := os.WriteFile("config.json", []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteFrontendMedia("esde", "/sd/ES-DE/downloaded_media"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FrontendMediaESDEDir() != "/sd/ES-DE/downloaded_media" {
		t.Fatalf("dir not persisted: %q", cfg.FrontendMediaESDEDir())
	}
	if !cfg.CoversEnabled() {
		t.Fatal("fetch_covers must ride along with frontend media")
	}
	raw, _ := os.ReadFile("config.json")
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	if tree["launcher_custom"] != "keep-me" {
		t.Fatal("unknown key lost in round-trip")
	}

	// Clear: both keys gone, unknown key still intact.
	if err := WriteFrontendMedia("", ""); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile("config.json")
	tree = nil
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	if _, ok := tree["frontend_media"]; ok {
		t.Fatal("frontend_media not cleared")
	}
	if _, ok := tree["fetch_covers"]; ok {
		t.Fatal("fetch_covers not cleared")
	}
	if tree["launcher_custom"] != "keep-me" {
		t.Fatal("unknown key lost on clear")
	}
}
