// Large-disc-image prefetch census (beta1 T2.1). The launch path downloads a
// stubbed ROM on first play; for a multi-hundred-MB disc image over the Miyoo
// Mini's Wi-Fi that first launch can stall badly. --prefetch-large is the daemon
// background leg that pre-warms those big stubs on a charging pass, off the launch
// path. This file is the OFFLINE (manifest + filesystem only) census of which
// stubs qualify — no network, no device — so the daemon can ask "is a cycle worth
// the radio?" before any config/host exists (parity with IncompleteMultiDiscDownloads).
package catalog

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/platform"
)

// discPlatformSlugs are the disc-based platforms whose STUB entries --prefetch-large
// pre-warms (segacd/psx/saturn/dreamcast — the CD/DVD-image systems where a single
// game is routinely hundreds of MB). A .chd container on ANY platform also qualifies
// (see isLargeDiscRel). Kept deliberately narrow: cartridge systems download fast
// enough on the launch path that pre-warming them would just burn charge-time radio.
var discPlatformSlugs = map[string]bool{
	"segacd":    true,
	"psx":       true,
	"saturn":    true,
	"dreamcast": true,
}

// isLargeDiscRel reports whether a manifest-relative path is a large disc image:
// a .chd container (any platform), or any file under a disc-based platform folder
// (segacd/psx/saturn/dreamcast). The manifest key is SDCARD-relative with a leading
// "/" (e.g. "/Roms/SEGACD/game.chd") — the folder under Roms is the host TAG, which
// FsSlugForTag reverses to the RomM slug the disc-platform set is keyed on.
func isLargeDiscRel(rel string) bool {
	if strings.EqualFold(filepath.Ext(rel), ".chd") {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(rel, "/"), "/")
	if len(parts) < 3 || !strings.EqualFold(parts[0], "Roms") {
		return false
	}
	slug, ok := platform.FsSlugForTag(parts[1])
	if !ok {
		return false
	}
	return discPlatformSlugs[slug]
}

// LargeStubDownloads walks the mirror manifest for STUB entries (0-byte placeholders
// the mirror wrote, not yet downloaded) that are large disc images per isLargeDiscRel.
// Offline by design (manifest + filesystem only). A stub that has since gained bytes
// (the user launched + downloaded it) or vanished is skipped — only a still-0-byte
// stub wants pre-warming. Path-sorted for deterministic RESULT lines and logs.
func LargeStubDownloads() []string {
	man := platform.LoadManifest()
	sd := sdcardRoot()
	var out []string
	for rel, e := range man.Entries {
		if e.Kind != platform.ManifestStub {
			continue
		}
		if !isLargeDiscRel(rel) {
			continue
		}
		abs := filepath.Join(sd, rel)
		fi, err := os.Stat(abs)
		if err != nil || fi.IsDir() || fi.Size() != 0 {
			continue // gone, or already has bytes (downloaded since the stub was written)
		}
		out = append(out, abs)
	}
	sort.Strings(out)
	return out
}
