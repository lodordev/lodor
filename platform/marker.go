// State markers (BLUEPRINT §A — cloud / on-device delineation).
//
// Lodor stubs every not-yet-downloaded RomM game as a 0-byte file in Roms/ and lets
// the stock launcher render the filename. To show a game's sync state WITHOUT forking
// the launcher, the engine bakes a leading marker into the ROM filename:
//
//	MarkerCloud     "[^] "  — a stub: the game lives in the cloud, not on the device
//	MarkerOnDevice  "[v] "  — a real, downloaded file present on the device
//
// WHY ASCII, not a glyph: NextUI renders names with the user-selectable font1.ttf
// (Rounded Mplus 1c) or font2.ttf (BPreplay). font2 carries NO arrow/circle/cloud
// glyphs and NEITHER font has a cloud (U+2601). SDL2_ttf does no glyph fallback, so a
// missing glyph renders as tofu. The ONLY symbols certain to render in whichever font
// the user picks are ASCII, so the markers are pure ASCII: "[^]" (up = in the cloud)
// and "[v]" (down = pulled onto the device). The brackets disambiguate the marker from
// a real leading character and cluster the two states together in the sorted list.
//
// getDisplayName (NextUI common/utils.c:314) strips only TRAILING "(...)"/"[...]" groups
// and trailing extensions — a LEADING "[^] "/"[v] " survives intact (the leading "[" is
// at index 0, where its strip loop breaks). Verified against that source.
//
// RESOLUTION: the marker is a DISPLAY/state artifact on the filename only. The catalog
// index is keyed by the unmarked canonical basename, and ResolveRomID strips the leading
// marker before lookup, so a marked ROM still resolves to the right RomM rom_id and its
// server saves round-trip unchanged. The LOCAL save filename keeps the marked name
// (the emulator derives it from the on-disk ROM name) — consistent locally, and migrated
// in lockstep by ReconcileMarkedPresence whenever the marker flips.
package platform

import (
	"os"
	"path/filepath"
	"strings"

	"lodor/config"
	"lodor/romm"
)

const (
	// MarkerCloud prefixes a 0-byte stub (game available in the cloud, not downloaded).
	MarkerCloud = "[^] "
	// MarkerOnDevice prefixes a real, downloaded ROM file present on the device. It is
	// always the SAME string, so a downloaded game's on-disk name (and therefore its
	// save filename) is STABLE across delete/redownload.
	MarkerOnDevice = "[v] "
)

// allMarkers is every leading state marker the engine writes, so StripLeadingMarker can
// reverse whichever one an on-disk name carries.
var allMarkers = []string{MarkerCloud, MarkerOnDevice}

// StripLeadingMarker removes a leading cloud/on-device state marker from a ROM base name
// (or full filename), returning the canonical, server-matched name. A name with no
// marker is returned unchanged. This is the inverse the resolver applies so a marked
// on-disk ROM reverses to the same rom_id as its unmarked catalog key.
func StripLeadingMarker(name string) string {
	for _, m := range allMarkers {
		if strings.HasPrefix(name, m) {
			return name[len(m):]
		}
	}
	return name
}

// HasLeadingMarker reports whether name begins with any engine state marker.
func HasLeadingMarker(name string) bool {
	for _, m := range allMarkers {
		if strings.HasPrefix(name, m) {
			return true
		}
	}
	return false
}

// ReconcileMarkedPresence ensures the ROM whose canonical (unmarked) on-disk path is
// `unmarked` has exactly ONE physical presence under the correct leading state marker:
//
//	a real (non-zero) file      -> MarkerOnDevice  "[v] "
//	a 0-byte stub, or absent     -> MarkerCloud     "[^] "
//
// It migrates the game — and its saves and cover — from any OTHER known variant (the
// opposite marker, or a legacy unmarked name left by a pre-marker mirror) so nothing
// orphans across the rename. It only ever inspects/touches THIS ROM's three candidate
// paths (dev, cloud, legacy) under one directory; it never globs the folder for guesses.
//
// This makes the marker DETERMINISTIC BY STATE and self-healing: the first marked mirror
// over an older unmarked deployment converts every stub ([^]) and every already-downloaded
// game ([v]) in place, and a later refresh promotes a game that was filled by
// fetch-on-launch (which must keep the cloud name in place, since a NextUI pre-launch hook
// cannot redirect the launch) up to its on-device name, carrying its first save with it.
//
// Returns the final on-disk path and whether a brand-new cloud stub was CREATED (vs the
// ROM already having some presence, possibly after a migration).
func ReconcileMarkedPresence(cfg *config.Config, rom romm.Rom, unmarked string) (final string, didCreate bool) {
	if unmarked == "" {
		return "", false
	}
	dir := filepath.Dir(unmarked)
	canonBase := StripLeadingMarker(filepath.Base(unmarked)) // defensive: unmarked is already canonical
	cloud := filepath.Join(dir, MarkerCloud+canonBase)
	dev := filepath.Join(dir, MarkerOnDevice+canonBase)
	legacy := filepath.Join(dir, canonBase)

	// Find the existing variant (priority: on-device, then cloud, then legacy unmarked)
	// and whether it holds real bytes.
	var src string
	var srcReal bool
	for _, c := range []string{dev, cloud, legacy} {
		if fi, err := os.Stat(c); err == nil {
			src = c
			srcReal = fi.Size() > 0
			break
		}
	}

	desired := cloud
	if srcReal {
		desired = dev
	}

	if src == "" {
		// Nothing on disk yet -> create the cloud stub.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", false
		}
		f, err := os.Create(cloud)
		if err != nil {
			return "", false
		}
		_ = f.Close()
		return cloud, true
	}
	if src == desired {
		return desired, false
	}
	migrateMarkedGame(rom, src, desired)
	return desired, false
}

// migrateMarkedGame renames a game from on-disk name src to dst, carrying its saves and
// cover so the rename never orphans them. Best-effort: a single failed rename is ignored
// so one locked artifact can't abort the migration. The ROM file is moved LAST.
func migrateMarkedGame(rom romm.Rom, src, dst string) {
	srcBase := filepath.Base(src)
	dstBase := filepath.Base(dst)
	if srcBase == dstBase {
		return
	}
	// 1. Saves: every save artifact named "<srcBase>.<...>" in the slug's save folders.
	//    The MinUI/minarch save name is "<full ROM filename>.sav" (and save states share
	//    the same "<full ROM filename>." prefix), so anchoring on srcBase+"." migrates
	//    battery saves AND states while never touching a different game.
	if rom.PlatformFsSlug != "" {
		for _, folder := range EmulatorFoldersForFSSlug(rom.PlatformFsSlug) {
			renameSiblings(filepath.Join(SavesDir(), folder), srcBase, dstBase)
		}
	}
	// 2. Cover: NextUI keeps box art at "<rom dir>/.media/<stem>.<img ext>". Rename the
	//    exact stem matches only (a prefix match could catch a different game whose stem
	//    has a dot), across the common image extensions.
	srcStem := strings.TrimSuffix(srcBase, filepath.Ext(srcBase))
	dstStem := strings.TrimSuffix(dstBase, filepath.Ext(dstBase))
	media := filepath.Join(filepath.Dir(src), ".media")
	for _, ext := range []string{".png", ".jpg", ".jpeg"} {
		s := filepath.Join(media, srcStem+ext)
		if _, err := os.Stat(s); err == nil {
			_ = os.Rename(s, filepath.Join(media, dstStem+ext))
		}
	}
	// 3. The ROM file itself, last.
	_ = os.Rename(src, dst)
}

// renameSiblings renames every regular file in dir whose name is "<oldPrefix>.<rest>"
// to "<newPrefix>.<rest>". The trailing "." anchor means only artifacts belonging to
// this exact base are touched (never a different game that merely shares a name prefix).
// A missing dir or no matches is a silent no-op.
func renameSiblings(dir, oldPrefix, newPrefix string) {
	if oldPrefix == "" || oldPrefix == newPrefix {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	anchor := oldPrefix + "."
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, anchor) {
			continue
		}
		newName := newPrefix + name[len(oldPrefix):]
		_ = os.Rename(filepath.Join(dir, name), filepath.Join(dir, newName))
	}
}
