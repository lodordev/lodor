package romm

import (
	"encoding/json"
	"testing"
)

func TestResolveFsSlug(t *testing.T) {
	defer func(f func(string) bool) { KnownFsSlug = f }(KnownFsSlug)
	KnownFsSlug = func(s string) bool { return s == "gba" || s == "gb" }

	var p Platform
	if err := json.Unmarshal([]byte(`{"slug":"gba","fs_slug":"Gameboy Advance"}`), &p); err != nil || p.FsSlug != "gba" {
		t.Fatalf("custom folder: fs_slug=%q err=%v, want gba", p.FsSlug, err)
	}
	var r Rom
	if err := json.Unmarshal([]byte(`{"id":7,"platform_slug":"gba","platform_fs_slug":"Gameboy Advance","fs_name":"x.gba"}`), &r); err != nil || r.PlatformFsSlug != "gba" || r.ID != 7 || r.FsName != "x.gba" {
		t.Fatalf("custom folder rom: %+v err=%v", r, err)
	}
	for _, c := range []struct{ fs, slug, want string }{
		{"gb", "gba", "gb"},                 // a known fs_slug is never replaced
		{"My Stuff", "unknown", "My Stuff"}, // nothing better to use
		{"My Stuff", "", "My Stuff"},
	} {
		if got := resolveFsSlug(c.fs, c.slug); got != c.want {
			t.Errorf("resolveFsSlug(%q,%q)=%q, want %q", c.fs, c.slug, got, c.want)
		}
	}
}
