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
	MarkerCloud = "✘ "
	// MarkerOnDevice prefixes a real, downloaded ROM file present on the device. It is
	// always the SAME string, so a downloaded game's on-disk name (and therefore its
	// save filename) is STABLE across delete/redownload.
	MarkerOnDevice = "✓ "
)

// legacyMarkers are the old ASCII markers earlier builds wrote ([^]/[v]). Kept so
// StripLeadingMarker/ReconcileMarkedPresence still recognize + migrate cards mirrored
// before the check/X switch (instead of duplicating the stub under a new marker).
var legacyMarkers = []string{"[^] ", "[v] "}

// allMarkers is every leading state marker the engine may encounter (current + legacy),
// so StripLeadingMarker can reverse whichever one an on-disk name carries.
var allMarkers = append([]string{MarkerCloud, MarkerOnDevice}, legacyMarkers...)

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

// HasCloudMarker reports whether name begins with the CLOUD (stub) marker —
// current "✘ " or legacy "[^] ". The manifest's ReclaimableStub triple gate keys
// off this: only a cloud-marked 0-byte file may ever be re-claimed as mirror-owned.
func HasCloudMarker(name string) bool {
	return strings.HasPrefix(name, MarkerCloud) || strings.HasPrefix(name, "[^] ")
}

// StripRomMTag removes the trailing coexist disambiguator (" (RomM)") from an
// EXTENSION-LESS stem, returning the canonical stem. A stem without the tag is
// returned unchanged. This is the second of the two device-local decorations the
// mirror bakes into an on-disk name (the first is the leading state marker); the
// resolver and the save-upload canonicalizer share this single definition so the
// strip set can never drift between them (task #135 / workstream A1).
func StripRomMTag(stem string) string {
	return strings.TrimSuffix(stem, romMDisambiguator)
}

// HasRomMTag reports whether an extension-less stem carries the coexist
// disambiguator suffix.
func HasRomMTag(stem string) bool {
	return strings.HasSuffix(stem, romMDisambiguator)
}

// RomMTag returns the coexist disambiguator suffix itself (" (RomM)") for callers
// that need to CONSTRUCT a tagged lookup key rather than strip one.
func RomMTag() string { return romMDisambiguator }

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
	return ReconcileMarkedPresenceGuarded(cfg, rom, unmarked, nil)
}

// ReconcileMarkedPresenceGuarded is ReconcileMarkedPresence with the merge-mode
// LEGACY-CANDIDATE guard (C1 design audit V4). The legacy candidate is a file at
// the bare unmarked canonical name — in "own"/"separate" modes that can only be a
// pre-marker Lodor deployment's own file, but in MERGE mode the user's own exact-
// named game sits at precisely that path, and adopting it would RENAME their ROM
// and their saves to "✓ <name>" without consent (live field bug, §0.2 of the
// design). When legacyOwned is non-nil (the catalog passes a manifest check in
// merge mode) an unowned legacy candidate is left COMPLETELY untouched and the
// reconcile returns ("", false): create-stub-or-nothing — the mirror's dedup
// should have adopted the file instead, and a duplicate marked stub beside the
// user's file would render as a duplicate row. nil legacyOwned = historical
// behavior, byte-identical for own/separate/LodorOS callers.
func ReconcileMarkedPresenceGuarded(cfg *config.Config, rom romm.Rom, unmarked string, legacyOwned func(path string) bool) (final string, didCreate bool) {
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
	candidates := []string{dev, cloud}
	for _, m := range legacyMarkers {
		candidates = append(candidates, filepath.Join(dir, m+canonBase))
	}
	candidates = append(candidates, legacy)
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil {
			src = c
			srcReal = fi.Size() > 0
			break
		}
	}

	// V4 guard (merge mode): the bare unmarked candidate that isn't manifest-owned
	// is the USER's file — never rename/adopt it, never stub beside it.
	if src == legacy && src != "" && legacyOwned != nil && !legacyOwned(src) {
		return "", false
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

// RenameSaveArtifacts renames every save/state artifact for a game whose on-disk
// basename changes from oldBase to newBase (battery saves AND states — everything
// prefixed "<base>."), across ALL of the slug's save folders. Exported for the
// coexist-layout migration (cmd/lodor-sync), which moves mirror-owned games between
// folder layouts and must carry their saves exactly like migrateMarkedGame does.
func RenameSaveArtifacts(slug, oldBase, newBase string) {
	if slug == "" {
		return
	}
	for _, folder := range EmulatorFoldersForFSSlug(slug) {
		renameSiblings(filepath.Join(SavesDir(), folder), oldBase, newBase)
	}
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
