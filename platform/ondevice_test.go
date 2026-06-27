package platform

import (
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/romm"
)

// TestOnDeviceFolderName locks the cloud->on-device folder twin transform used by the
// NextUI folder-as-badge scheme: "<Display> RomM (<TAG>)" -> "<Display> (<TAG>)", with
// own-mode / unmarked names returned unchanged and the rewrite anchored on the trailing
// " (<TAG>)" so a Display name containing " RomM (" earlier is never mis-rewritten.
func TestOnDeviceFolderName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Nintendo 64 RomM (N64)", "Nintendo 64 (N64)"},
		{"Sony PlayStation RomM (PS)", "Sony PlayStation (PS)"},
		{"Nintendo 64 (N64)", "Nintendo 64 (N64)"}, // own-mode: unchanged
		{"Some Folder", "Some Folder"},             // no tag suffix: unchanged
		{"My RomM ( Game ) RomM (FC)", "My RomM ( Game ) (FC)"}, // only the trailing marker is stripped
	}
	for _, c := range cases {
		if got := OnDeviceFolderName(c.in); got != c.want {
			t.Errorf("OnDeviceFolderName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestOnDeviceRomPathSaveInvariance proves the move is SAVE-SAFE: relocating a ROM from
// its "<System> RomM (<TAG>)" cloud folder to the on-device "<System> (<TAG>)" folder
// changes ONLY the parent directory — the basename is byte-identical, and since the
// MinUI/NextUI save path is <Saves>/<TAG>/<basename>.sav (folder-independent), the save
// round-trips unchanged. own-mode is a no-op (no cloud folder).
func TestOnDeviceRomPathSaveInvariance(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	roms := filepath.Join(base, "Roms")

	single := romm.Rom{PlatformFsSlug: "n64", Files: []romm.RomFile{{FileName: "Zelda (USA).z64"}}}

	sep := &config.Config{
		MirrorMode: config.MirrorModeSeparate,
		DirectoryMappings: map[string]config.DirMapping{
			"n64": {Slug: "n64", RelativePath: "Nintendo 64 RomM (N64)"},
		},
	}
	own := &config.Config{
		MirrorMode: config.MirrorModeOwn,
		DirectoryMappings: map[string]config.DirMapping{
			"n64": {Slug: "n64", RelativePath: "Nintendo 64 (N64)"},
		},
	}

	src := LocalRomPath(sep, single)
	dst := OnDeviceRomPath(sep, single)
	wantDst := filepath.Join(roms, "Nintendo 64 (N64)", "Zelda (USA) (RomM).z64")
	if dst != wantDst {
		t.Errorf("separate OnDeviceRomPath = %q, want %q", dst, wantDst)
	}
	if filepath.Base(src) != filepath.Base(dst) {
		t.Errorf("basename changed by move: src=%q dst=%q (would orphan the save)", filepath.Base(src), filepath.Base(dst))
	}
	if filepath.Dir(src) == filepath.Dir(dst) {
		t.Errorf("folder did not change: %q (badge would not flip)", filepath.Dir(src))
	}

	// own mode: no cloud folder, so the path is unchanged (no move).
	if OnDeviceRomPath(own, single) != LocalRomPath(own, single) {
		t.Errorf("own-mode OnDeviceRomPath should equal LocalRomPath (no relocate in own mode)")
	}
}
