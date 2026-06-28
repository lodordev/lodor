package platform

import (
	"os"
	"testing"
)

// TestSavesDirHonorsEnv asserts the multi-user SAVES_PATH override redirects the save
// root (so a profile-namespaced boot export lands the saves), and that an unset env
// falls back to <BasePath>/Saves (single-user, historical).
func TestSavesDirHonorsEnv(t *testing.T) {
	old, had := os.LookupEnv("SAVES_PATH")
	defer func() {
		if had {
			os.Setenv("SAVES_PATH", old)
		} else {
			os.Unsetenv("SAVES_PATH")
		}
	}()

	os.Setenv("SAVES_PATH", "/mnt/SDCARD/Saves/alice")
	if got := SavesDir(); got != "/mnt/SDCARD/Saves/alice" {
		t.Errorf("SavesDir with env: got %q want /mnt/SDCARD/Saves/alice", got)
	}

	os.Unsetenv("SAVES_PATH")
	t.Setenv("BASE_PATH", "/tmp/card")
	if got := SavesDir(); got != "/tmp/card/Saves" {
		t.Errorf("SavesDir fallback: got %q want /tmp/card/Saves", got)
	}
}

// TestProfileStateName asserts per-profile namespacing of the integrity-critical state
// files: a profile yields "<base>.<profile>.<ext>" (sanitized), and default/unset keeps
// the historical un-namespaced "<base>.<ext>".
func TestProfileStateName(t *testing.T) {
	cases := []struct {
		prof string
		want string
	}{
		{"", "sync-anchors.json"},
		{"default", "sync-anchors.json"},
		{"alice", "sync-anchors.alice.json"},
		{"Kid A30", "sync-anchors.Kid_A30.json"},
		{"a/b", "sync-anchors.a_b.json"},
	}
	for _, c := range cases {
		if c.prof == "" {
			os.Unsetenv("LODOR_PROFILE")
		} else {
			t.Setenv("LODOR_PROFILE", c.prof)
		}
		if got := ProfileStateName("sync-anchors", "json"); got != c.want {
			t.Errorf("ProfileStateName(%q): got %q want %q", c.prof, got, c.want)
		}
	}
	os.Unsetenv("LODOR_PROFILE")
}
