//go:build !onion

package platform

import (
	"fmt"

	"lodor/config"
)

// MirrorFolderName builds the Roms/ folder a platform's RomM games are mirrored into on
// MinUI/NextUI (the default CFW). In "own" mode (LodorOS — the card IS the library) it is
// the historical "<Display> (<TAG>)". In "separate"/"merge" it is "<Display> RomM
// (<TAG>)": NextUI's getEmuName binds off the LAST paren so this still launches <TAG>.pak,
// getDisplayName strips the trailing paren so it reads "<Display> RomM", and it can never
// collide with the user's own "<Display> (<TAG>)" folder (issue #68). This is the same
// string catalog.mirrorFolderName used to build inline; it moved here so the OnionOS
// variant (bare TAG) can supply its own under -tags onion. Byte-identical to the prior
// inline form — generate_test stays green.
func MirrorFolderName(display, tag, mode string) string {
	if mode == config.MirrorModeOwn {
		return fmt.Sprintf("%s (%s)", display, tag)
	}
	return fmt.Sprintf("%s RomM (%s)", display, tag)
}
