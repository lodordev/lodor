// BIOS-requirement knowledge shared by every CFW build (no build tag): which systems
// have an emulator that CANNOT boot without firmware the user must supply themselves
// (BYOB — LodorOS never ships BIOS, per feedback_no_ship_bios), and the local check the
// launchers call BEFORE launch so a missing BIOS becomes an HONEST on-screen message
// instead of a silent black screen (build #158).
//
// CGO-free, stdlib only. The directory layout differs per CFW (Bios/<TAG>/ on MinUI,
// flat Bios/ on onion/muos) — CheckBIOS covers both by searching Bios/<TAG> AND the flat
// Bios root, plus any extra dirs the caller knows about (e.g. a vendor RetroArch
// system_directory the H700 shim reads).
package platform

import (
	"os"
	"path/filepath"
	"strings"
)

// BIOSRequirement describes a system whose shipped LodorOS core genuinely fails to boot
// without firmware. SystemName is the human label for the on-screen message. Slots is an
// AND-of-ORs: EVERY slot must be satisfied, and a slot is satisfied when ANY of its
// alternative filenames is present (region variants — a Sega CD needs ONE of the three
// regional BIOSes, not all three). Subdirs are per-system subdirectories some cores also
// look in UNDER a BIOS root (flycast probes a "dc/" subdir as well as the flat root),
// searched in addition to — never instead of — the root.
type BIOSRequirement struct {
	SystemName string
	Slots      [][]string
	Subdirs    []string
}

// requiredBIOS maps a MinUI emulator TAG to its mandatory BIOS. A TAG ABSENT from this
// map has NO BIOS requirement and is NEVER gated (CheckBIOS returns ok=true) — that is
// the safety default: we only ever gate a system we are confident cannot boot without
// firmware on the core LodorOS actually ships. Cores with working HLE BIOS (pcsx_rearmed
// for PS1, PPSSPP for PSP) are DELIBERATELY EXCLUDED so a game that would have played is
// never falsely blocked. Filenames are the libretro-standard system-file names — the
// same names RomM's firmware manifest / --download-bios write into Bios/<TAG>/.
var requiredBIOS = map[string]BIOSRequirement{
	// Dreamcast via flycast (H700 vendor-RA shim, and standalone flycast) — no HLE BIOS;
	// dc_boot.bin absent is a guaranteed black screen (the RG40XXV case that drove #158).
	"DC": {
		SystemName: "Dreamcast",
		Slots:      [][]string{{"dc_boot.bin"}, {"dc_flash.bin"}},
		Subdirs:    []string{"dc"},
	},
	// Sega CD via picodrive / genesis_plus_gx — no HLE; needs one regional BIOS.
	"SEGACD": {
		SystemName: "Sega CD",
		Slots:      [][]string{{"bios_CD_U.bin", "bios_CD_E.bin", "bios_CD_J.bin"}},
	},
	// Saturn via mednafen-saturn / yabause — needs one regional BIOS (yabasanshiro can HLE
	// if the user enables it; the default and the common case require the real BIOS).
	"SATURN": {
		SystemName: "Saturn",
		Slots:      [][]string{{"sega_101.bin", "mpr-17933.bin"}},
	},
}

// BIOSRequirementForTag returns the BIOS requirement for a MinUI TAG and whether the
// system requires any BIOS at all.
func BIOSRequirementForTag(tag string) (BIOSRequirement, bool) {
	r, ok := requiredBIOS[strings.ToUpper(strings.TrimSpace(tag))]
	return r, ok
}

// CheckBIOS reports whether every mandatory BIOS slot for a system TAG is satisfied by a
// non-empty file present in one of the candidate directories. extraDirs are additional
// BIOS search roots the caller knows about (e.g. a vendor RetroArch system_directory the
// H700 shim reads) — searched ALONGSIDE the engine's own Bios/<TAG>/ default, never
// instead of it. A system with no BIOS requirement returns ok=true, nil, "" (never
// gated). When something is missing, ok=false and missing lists one representative
// filename per unsatisfied slot (the FIRST alternative — the canonical name to fetch).
func CheckBIOS(tag string, extraDirs []string) (ok bool, missing []string, systemName string) {
	req, need := BIOSRequirementForTag(tag)
	if !need {
		return true, nil, ""
	}
	dirs := biosSearchDirs(strings.ToUpper(strings.TrimSpace(tag)), req, extraDirs)
	for _, slot := range req.Slots {
		if len(slot) == 0 {
			continue
		}
		if !biosSlotSatisfied(slot, dirs) {
			missing = append(missing, slot[0])
		}
	}
	return len(missing) == 0, missing, req.SystemName
}

// biosSearchDirs is the ordered, de-duplicated list of directories to look for a system's
// BIOS in: the engine's own Bios/<TAG>/ (where --download-bios writes and minarch's
// system_directory points, minarch.c), the flat Bios/ root (onion/muos layout, and cores
// that read the BIOS root directly), each extraDir, and for every one of those any
// per-system Subdir (flycast's dc/).
func biosSearchDirs(tag string, req BIOSRequirement, extraDirs []string) []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	roots := []string{filepath.Join(BiosDir(), tag), BiosDir()}
	roots = append(roots, extraDirs...)
	for _, r := range roots {
		add(r)
		for _, sub := range req.Subdirs {
			add(filepath.Join(r, sub))
		}
	}
	return dirs
}

// biosSlotSatisfied is true when any alternative filename for a slot exists non-empty in
// any candidate directory.
func biosSlotSatisfied(alts []string, dirs []string) bool {
	for _, name := range alts {
		for _, d := range dirs {
			if fi, err := os.Stat(filepath.Join(d, name)); err == nil && !fi.IsDir() && fi.Size() > 0 {
				return true
			}
		}
	}
	return false
}
