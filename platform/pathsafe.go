// Path-safety guards (SECURITY, 2026-07-10 audit). Every string the engine joins
// under the SD card that originates from the RomM server (file_name, fs_name_no_ext,
// platform_fs_slug) or from a co-installed app's config.json (directory_mappings
// relative_path) is HOSTILE input. A server that returns
// file_name:"../../../../.system/<plat>/bin/lodor-sync" — or a config with
// relative_path:"../../../data/local/tmp" — must not let a download escape the card
// and overwrite an executable that runs at next boot/sync. Hash verification is NOT a
// defence: the server supplies the hash, and an empty hash bypasses the check.
//
// Two independent layers, applied at every server-name -> on-disk-path join:
//  1. safePathComponent — a per-component "belt": a single, literal path segment with
//     no separators, no traversal, no absolutes.
//  2. containedUnder     — a "suspenders" containment assertion on the FINAL computed
//     destination: it must resolve strictly inside its intended base directory.
//
// This file carries NO build tag on purpose: it compiles into EVERY CFW variant
// (default MinUI, muos, onion, knulli, lodorandroid) so the guards can never be
// silently dropped from a lane.
package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"lodor/romm"
)

// safePathComponent reports whether s is a single, safe on-disk path COMPONENT: a
// non-empty name that is neither "." nor ".." , contains no path separator (/ or \),
// equals its own filepath.Base (so no directory prefix survived), and is a valid
// single path element per io/fs. It deliberately ALLOWS ordinary filenames the real
// download paths depend on — spaces, apostrophes, parentheses, unicode, dots, "+" —
// and rejects ONLY traversal / separators / absolute-ness. It is the per-component
// belt applied to every server-supplied name before it is joined under the card.
func safePathComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\") {
		return false
	}
	if filepath.Base(s) != s {
		return false
	}
	// fs.ValidPath rejects any path with a "." or ".." element, an empty element, a
	// leading/trailing slash, or a NUL byte. For a single component, combined with the
	// separator check above, a lone valid filename passes and traversal is refused.
	return fs.ValidPath(s)
}

// SafeName is the exported form of safePathComponent for callers outside the platform
// package (the download loop's per-disc re-check).
func SafeName(s string) bool { return safePathComponent(s) }

// containedUnder reports whether dest, once cleaned, resolves strictly inside baseDir
// (or IS baseDir). It is the final-destination containment assertion — the suspenders
// to safePathComponent's belt — catching any traversal that slips through component
// checks (e.g. a multi-segment mapped folder). Both paths are cleaned; the relative
// path from base to dest must not be ".." nor begin with "../". filepath.IsLocal on
// that relative path is the go1.25 restatement of the same rule and is used as a
// belt-and-suspenders second gate.
func containedUnder(baseDir, dest string) bool {
	if baseDir == "" || dest == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(baseDir), filepath.Clean(dest))
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	// rel == "." means dest IS the base dir (allowed as a container); any other rel
	// must be a local (non-escaping, non-absolute) path.
	if rel != "." && !filepath.IsLocal(rel) {
		return false
	}
	return true
}

// PathWithinRoms reports whether dest resolves strictly inside RomsDir() (the ROM
// download tree). It is the exported containment "suspenders" the download paths call
// on each final destination (single-file dest, multi-disc discDest, the .m3u) before
// os.Create / os.Rename, so even a traversal that somehow slips past the per-component
// belt cannot land bytes outside Roms/. A dest == RomsDir() itself would be a bug for a
// file write, so an exact match is treated as NOT a valid file destination.
func PathWithinRoms(dest string) bool {
	base := RomsDir()
	if filepath.Clean(dest) == filepath.Clean(base) {
		return false
	}
	return containedUnder(base, dest)
}

// isSafeRelFolder reports whether folder is a safe RELATIVE folder to place under
// RomsDir: it must not be absolute, must contain no ".." segment (checked on the
// forward-slash-normalised form so a Windows-style "..\x" is also caught), and its
// cleaned join under RomsDir() must not escape RomsDir(). A hostile
// directory_mappings relative_path such as "../../../../data/local/tmp" is rejected;
// a legitimate nested layout like "Game Boy Advance (GBA)" or "Sony/PlayStation" is
// accepted. Callers drop a poisoned value and fall through to the canonical path.
func isSafeRelFolder(folder string) bool {
	if folder == "" {
		return false
	}
	if filepath.IsAbs(folder) {
		return false
	}
	// Split on BOTH separators regardless of host OS: config.json can be authored on any
	// platform, and on Linux/Android filepath treats "\" as a literal char, so a
	// "..\windows" segment would otherwise slip through ToSlash unchanged. Normalise the
	// backslash to a slash first so every ".." segment is caught.
	norm := strings.ReplaceAll(filepath.ToSlash(folder), "\\", "/")
	for _, seg := range strings.Split(norm, "/") {
		if seg == ".." {
			return false
		}
	}
	base := RomsDir()
	return containedUnder(base, filepath.Join(base, folder))
}

// ValidateRomNames reports whether every server-supplied name in rom that the engine
// will join under the card is a safe single path component. It is the centralised belt
// applied right after GetRom (single-file / stub / archive paths) and again per-disc
// in the multi-disc path. It checks:
//   - PlatformFsSlug     (used as a ROM-folder name when no mapping exists)
//   - FsNameNoExt        (the multi-disc per-game subfolder + m3u stem)
//   - every Files[i].FileName (each disc / the single file's on-disk name)
//
// Empty PlatformFsSlug / FsNameNoExt are tolerated where the caller already treats
// them as "no path" (LocalRomPath returns "" for an empty slug); only NON-empty
// values must be safe. A nil/empty Files slice is fine (nothing to write).
func ValidateRomNames(rom romm.Rom) bool {
	if rom.PlatformFsSlug != "" && !safePathComponent(rom.PlatformFsSlug) {
		return false
	}
	// FsNameNoExt is the multi-disc subfolder + m3u stem; it must be a single safe
	// component. Single-file ROMs derive their basename from Files[0].FileName instead,
	// but an unsafe FsNameNoExt is never legitimate, so reject it whenever present.
	if rom.FsNameNoExt != "" && !safePathComponent(rom.FsNameNoExt) {
		return false
	}
	for _, f := range rom.Files {
		if f.FileName != "" && !safePathComponent(f.FileName) {
			return false
		}
	}
	return true
}
