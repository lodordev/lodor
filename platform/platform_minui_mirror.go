//go:build !onion && !muos

package platform

import (
	"fmt"

	"lodor/config"
)

// MirrorFolderName builds the Roms/ folder a platform's RomM games are mirrored into on
// MinUI/NextUI (the default CFW). In "own" AND "merge" modes it is the clean
// "<Display> (<TAG>)" — merge ADOPTS the user's existing same-tag folder when one
// exists (catalog/generate.go's adopt-by-tag scan overrides this name with theirs), so
// this clean form is only ever CREATED when no user folder exists and there is no
// collision by definition (no " RomM" wart). In "separate" it is "<Display> RomM
// (<TAG>)": NextUI's getEmuName binds off the LAST paren so this still launches
// <TAG>.pak, getDisplayName strips the trailing paren so it reads "<Display> RomM",
// and it can never collide with the user's own "<Display> (<TAG>)" folder (issue #68).
// OnionOS supplies its own variant (bare TAG) under -tags onion.
func MirrorFolderName(display, tag, mode string) string {
	if mode == config.MirrorModeSeparate {
		return fmt.Sprintf("%s RomM (%s)", display, tag)
	}
	return fmt.Sprintf("%s (%s)", display, tag)
}
