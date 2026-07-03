package sync

// Cross-device previews (task #149, Lane-A half): the last-session screenshot
// rides the saves transport as a `<fs_name>.lodorshot.png` meta-save (pushed
// alongside the save it belongs to), and on pull lands at the SAME on-card
// convention the launcher renders from either source:
//
//	<SDCARD>/.userdata/shared/.minui/<TAG>/<on-disk rom basename>.auto.png
//
// so a preview captured on the Brick shows up under Continue on the Flip.
// Everything here is BEST-EFFORT: a missing preview, a foreign (non-MinUI)
// card layout, or any transfer failure is a silent no-op — previews are
// cosmetic and must never affect a save decision.

import (
	"os"
	"path/filepath"
	"strings"

	"lodor/romm"
)

// previewMetaExt is the meta-save extension previews travel under.
const previewMetaExt = ".lodorshot.png"

// PreviewLocalPath computes the on-card preview path for a launched ROM path:
// .minui/<TAG>/<basename>.auto.png under the shared userdata tree. TAG is the
// ROM folder's parenthetical ("Nintendo 64 (N64)" -> "N64" — same rule as the
// wrappers' _rom_systag); the basename is the on-disk name VERBATIM (markers
// included) because that's the name the launcher looks up. "" when the card
// has no .minui shared state (capability gate: never spray onto foreign
// layouts) or the path is degenerate.
func PreviewLocalPath(romPath string) string {
	if romPath == "" {
		return ""
	}
	sd := os.Getenv("SDCARD_PATH")
	if sd == "" {
		sd = "/mnt/SDCARD"
	}
	minuiDir := filepath.Join(sd, ".userdata", "shared", ".minui")
	if fi, err := os.Stat(minuiDir); err != nil || !fi.IsDir() {
		return "" // not a MinUI-family card
	}
	tag := folderTagOf(romPath)
	if tag == "" {
		return ""
	}
	return filepath.Join(minuiDir, tag, filepath.Base(romPath)+".auto.png")
}

// folderTagOf extracts the emulator TAG from a ROM path's parent folder
// (trailing parenthetical); a folder without one names itself.
func folderTagOf(romPath string) string {
	d := filepath.Base(filepath.Dir(romPath))
	if d == "." || d == string(filepath.Separator) {
		return ""
	}
	if i := strings.LastIndex(d, "("); i >= 0 {
		if j := strings.Index(d[i:], ")"); j > 1 {
			return d[i+1 : i+j]
		}
	}
	return d
}

// NewestPreviewMeta picks the newest usable .lodorshot.png record from a raw
// server save list (ghosts excluded; non-preview records ignored). ok=false
// when none exists. Pure — unit-tested without a server.
func NewestPreviewMeta(saves []romm.Save) (romm.Save, bool) {
	var best romm.Save
	found := false
	for _, s := range saves {
		if IsGhostSave(s) || !strings.HasSuffix(strings.ToLower(s.FileName), previewMetaExt) {
			continue
		}
		if !found || s.UpdatedAt.After(best.UpdatedAt) {
			best = s
			found = true
		}
	}
	return best, found
}

// pullPreviewBestEffort lands the newest server preview for rom at the local
// .minui convention — called from PullSaveDirect with the raw (unfiltered)
// save list it already fetched, so the common case costs zero extra requests.
// Skips the download entirely when the local preview already matches the
// server record's content hash. All failures silent (cosmetic layer).
func pullPreviewBestEffort(client *romm.Client, romPath string, saves []romm.Save) {
	best, found := NewestPreviewMeta(saves)
	if !found {
		return
	}
	dest := PreviewLocalPath(romPath)
	if dest == "" {
		return
	}
	if sum, ok := fileMD5(dest); ok && best.ContentHash != nil && strings.EqualFold(*best.ContentHash, sum) {
		return // already showing this preview
	}
	data, err := client.DownloadSaveContent(best.ID, "", false) // side-effect-free read
	if err != nil || len(data) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync() // FAT32: fsync before rename
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return
	}
	if os.Rename(tmp, dest) != nil {
		_ = os.Remove(tmp)
	}
}
