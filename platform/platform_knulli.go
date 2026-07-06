//go:build knulli

// Knulli platform variant — the no-fork port for Batocera-derived Knulli CFW
// (Anbernic H700 boards and friends). Built with `-tags knulli`. Layout facts are
// Batocera's, which Knulli inherits unchanged (task #186 context, verified
// 2026-07-06): ROMs in /userdata/roms/<system-slug>/ (short lower-case Batocera
// slugs: gba, snes, megadrive, psx …), RetroArch battery saves in
// /userdata/saves/<system-slug>/<rom-stem>.srm (per-SYSTEM directories — Knulli
// ships sort-by-core DISABLED upstream, unlike OnionOS/muOS whose save dirs are
// core-driven), BIOS in the single flat /userdata/bios.
//
// Why Knulli differs from MinUI (the default build in platform.go):
//   - The Roms/ folder name IS the Batocera system slug — EmulationStation binds a
//     folder to a system purely by that name (es_systems), so MirrorFolderName
//     returns the bare slug and CanonicalMirrorFolder heals any drifted mapping
//     back to it (same policy as muOS, whose folder is fixed by info/assign).
//   - Saves are RetroArch-style: "<rom stem>.<ext>" ("Game (USA).srm"), NOT
//     MinUI's "<full filename>.sav". The save DIRECTORY is the system slug —
//     deterministic, no core discovery needed (sort_savefiles_by_content/core are
//     off upstream). LODOR_SAVE_SUBDIR, when exported by a launch shim, still
//     pins the subfolder explicitly and wins (cross-CFW contract).
//   - Display polish (clean marker-less names, box art, favorites) comes from the
//     per-folder gamelist.xml EmulationStation reads — emitted by lodor-sync's
//     --write-gamelists (cmd/lodor-sync/gamelist.go), NOT from the filenames. The
//     ✘/✓ state markers stay baked in the on-disk filenames (engine machinery
//     unchanged; HostShowsStateNatively is hard-false here) while the gamelist
//     <name> shows the stripped title.
//
// SANDBOX RELOCATION: all roots are env-overridable (ROMS_DIR / SAVES_DIR /
// BIOS_DIR); a set BASE_PATH relocates the WHOLE tree to the generic
// <BASE_PATH>/{Roms,Saves,Bios} layout — the same names the engine's shared
// card-walk code joins under sdcardRoot(), so the off-hardware sandbox and the
// tag-free test suite exercise one consistent tree without a device. On hardware
// no env is needed: BASE_PATH is unset and the Batocera-true defaults below
// apply (the integration exports SDCARD_PATH=/userdata so manifest/index
// relative paths stay consistent). CGO-free, stdlib only.
package platform

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/config"
	"lodor/romm"
)

// knulliRomFolders maps a RomM filesystem slug to the Batocera/Knulli roms/ system
// folder slug (the es_systems folder name). MOSTLY IDENTITY — Batocera's own slugs
// are short and lower-case like RomM's — with every divergence called out below.
// A slug absent here is SKIPPED during mapping generation (no folder invented) —
// honest, same as MinUI skipping an unknown tag; HasEmuPak further gates on the
// roms/<slug> folder actually existing on the card (Knulli pre-creates a folder
// for every system the build supports). Aliases cover RomM instances that slug
// the same system differently (fbneo/arcade, sega32/sega32x, mastersystem/sms,
// atarilynx/lynx, tg16/pcengine, …) — each maps to the one Batocera name.
var knulliRomFolders = map[string]string{
	// --- Nintendo (identity: Batocera uses the same short slugs) ---
	"gb":           "gb",
	"gbc":          "gbc",
	"gba":          "gba",
	"nes":          "nes",
	"famicom":      "nes", // RomM alias — same Batocera folder
	"fds":          "fds",
	"snes":         "snes",
	"sfam":         "snes", // RomM alias (Super Famicom) — same Batocera folder
	"n64":          "n64",
	"nds":          "nds",
	"virtualboy":   "virtualboy",
	"pokemon-mini": "pokemini", // EXCEPTION: RomM hyphenates; Batocera folder is "pokemini"
	"pokemini":     "pokemini", // RomM alias slug — identity
	// --- Sony (identity) ---
	"psx": "psx",
	"psp": "psp",
	// --- Sega ---
	"genesis":      "megadrive", // EXCEPTION: Batocera names the system "megadrive", NOT "genesis"
	"sms":          "mastersystem", // EXCEPTION: RomM short slug → Batocera's long "mastersystem"
	"mastersystem": "mastersystem", // RomM alias — identity with the Batocera folder
	"gamegear":     "gamegear",
	"sg1000":       "sg1000",
	"segacd":       "segacd",
	"sega32":       "sega32x", // EXCEPTION: RomM's "sega32" → Batocera "sega32x"
	"sega32x":      "sega32x", // RomM alias — identity
	"dc":           "dreamcast", // EXCEPTION: RomM short slug → Batocera "dreamcast"
	"dreamcast":    "dreamcast", // RomM alias — identity
	"saturn":       "saturn",
	// --- NEC ---
	"tg16":          "pcengine",   // EXCEPTION: RomM's TurboGrafx-16 slug → Batocera "pcengine"
	"pcengine":      "pcengine",   // RomM alias — identity
	"turbografx-cd": "pcenginecd", // EXCEPTION: Batocera folder is "pcenginecd"
	"supergrafx":    "supergrafx",
	// --- Atari ---
	"atari2600":   "atari2600",
	"atari5200":   "atari5200",
	"atari7800":   "atari7800",
	"lynx":        "lynx",
	"atarilynx":   "lynx", // RomM alias — same Batocera folder
	"jaguar":      "jaguar",
	"atarijaguar": "jaguar", // RomM alias — same Batocera folder
	// --- SNK ---
	"neogeoaes":            "neogeo",   // EXCEPTION: both AES and MVS land in Batocera's one "neogeo"
	"neogeomvs":            "neogeo",   // EXCEPTION: see above
	"neo-geo-cd":           "neogeocd", // EXCEPTION: Batocera folder is "neogeocd" (no hyphens)
	"neo-geo-pocket":       "ngp",      // EXCEPTION: Batocera folder is "ngp"
	"neo-geo-pocket-color": "ngpc",     // EXCEPTION: Batocera folder is "ngpc" (separate from ngp)
	// --- Bandai ---
	"wonderswan":       "wswan",  // EXCEPTION: Batocera folder is "wswan"
	"wonderswan-color": "wswanc", // EXCEPTION: Batocera folder is "wswanc"
	"wonderswancolor":  "wswanc", // RomM alias — same Batocera folder
	"wswan":            "wswan",  // RomM alias — identity
	"wswanc":           "wswanc", // RomM alias — identity
	// --- Arcade ---
	"arcade": "fbneo", // EXCEPTION: no generic "arcade" folder on Batocera; FBNeo is the default arcade home
	"fbneo":  "fbneo", // RomM alias — identity
	// --- Other (identity, folder names confirmed against Batocera es_systems) ---
	"colecovision": "colecovision",
	"scummvm":      "scummvm",
	"pico-8":       "pico8", // EXCEPTION: RomM hyphenates; Batocera folder is "pico8"
	"dos":          "dos",
	"ports":        "ports",
	"c64":          "c64",
}

// saveSubdirEnv is set by a launch shim to pin the save subfolder for THIS launch
// (the cross-CFW contract shared with onion/muos). On Knulli it is normally UNSET:
// the save directory is the deterministic per-system slug (sort-by-core disabled
// upstream), so no shim/core discovery is needed — but an exported value still wins.
const saveSubdirEnv = "LODOR_SAVE_SUBDIR"

// envOr returns the value of environment variable key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BasePath returns the Knulli data root: BASE_PATH if set, else /userdata. Setting
// BASE_PATH relocates the whole engine tree to the generic <BASE_PATH>/{Roms,Saves,
// Bios} layout (sandbox/tests); on hardware it stays unset and the Batocera-true
// /userdata/{roms,saves,bios} defaults below apply.
func BasePath() string { return envOr("BASE_PATH", "/userdata") }

// RomsDir returns the Knulli ROMs root: ROMS_DIR if set; else the relocated generic
// <BASE_PATH>/Roms when BASE_PATH is set (sandbox); else the Batocera default
// /userdata/roms.
func RomsDir() string {
	if v := os.Getenv("ROMS_DIR"); v != "" {
		return v
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return filepath.Join(bp, "Roms")
	}
	return "/userdata/roms"
}

// BiosDir returns the Knulli BIOS dir: BIOS_DIR if set; else <BASE_PATH>/Bios when
// BASE_PATH is set (sandbox); else the Batocera default /userdata/bios (one flat
// directory — Batocera cores read system files from there).
func BiosDir() string {
	if v := os.Getenv("BIOS_DIR"); v != "" {
		return v
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return filepath.Join(bp, "Bios")
	}
	return "/userdata/bios"
}

// SavesDir returns the Knulli RetroArch savefile root: SAVES_DIR if set; else
// <BASE_PATH>/Saves when BASE_PATH is set (sandbox); else the Batocera default
// /userdata/saves (per-SYSTEM subfolders below it — Knulli ships RetroArch's
// sort-by-core disabled, so the subfolder is the system slug, not a corename).
func SavesDir() string {
	if v := os.Getenv("SAVES_DIR"); v != "" {
		return v
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return filepath.Join(bp, "Saves")
	}
	return "/userdata/saves"
}

// EmulatorFoldersForFSSlug returns the save folders to SCAN for a slug. A
// shim-pinned LODOR_SAVE_SUBDIR wins (cross-CFW contract); otherwise the one
// deterministic per-system folder — the Batocera slug — since Knulli's save sort
// is by system, never by core. An unknown slug scans nothing (no blind guesses).
func EmulatorFoldersForFSSlug(slug string) []string {
	if sub := os.Getenv(saveSubdirEnv); sub != "" {
		return []string{sub}
	}
	if f, ok := knulliRomFolders[slug]; ok {
		return []string{f}
	}
	return nil
}

// SaveFileName returns the Knulli/RetroArch on-disk save filename: the ROM basename
// with its extension stripped, plus the real save extension — SaveFileName("Game
// (USA).gba", "srm") == "Game (USA).srm". An empty saveExt yields just the stripped
// basename. Identical to the onion/muos RetroArch rule; the saveExt still flows
// from the server save's own extension (the shared save-ext machinery).
func SaveFileName(romFullFilename, saveExt string) string {
	base := strings.TrimSuffix(romFullFilename, filepath.Ext(romFullFilename))
	ext := strings.TrimPrefix(saveExt, ".")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

// SaveDirectory returns the canonical save directory for a write (pull/restore):
// a shim-pinned LODOR_SAVE_SUBDIR wins; else <SavesDir>/<batocera-slug> — the
// deterministic per-system folder RetroArch reads on Knulli; else "" for an
// unknown slug (no save directory — never blind-write).
func SaveDirectory(slug string) string {
	if sub := os.Getenv(saveSubdirEnv); sub != "" {
		return filepath.Join(SavesDir(), sub)
	}
	if f, ok := knulliRomFolders[slug]; ok {
		return filepath.Join(SavesDir(), f)
	}
	return ""
}

// BIOSFilePaths returns the Knulli BIOS destination(s) for a file: a single flat
// <BiosDir>/<base>. Batocera keeps system files in one directory (unlike MinUI's
// per-tag subdirs).
func BIOSFilePaths(fileName, slug string) []string {
	return []string{filepath.Join(BiosDir(), filepath.Base(fileName))}
}

// PrimaryTag returns the Knulli roms/ system folder — the Batocera slug — for a
// RomM fs_slug (used to build directory_mappings and the mirror folder), and
// ok=false when the slug isn't a known Batocera system (caller SKIPs it, inventing
// no folder). On Knulli the "tag" IS the folder slug ("megadrive"): EmulationStation
// binds a roms/ subfolder to a system purely by that name, so stubs land in a
// folder ES will recognise.
func PrimaryTag(fsSlug string) (tag string, ok bool) {
	if f, ok := knulliRomFolders[fsSlug]; ok {
		return f, true
	}
	return "", false
}

// FsSlugForTag reverses knulliRomFolders: given a Batocera folder slug, return a
// RomM fs_slug that maps to it. Several slugs alias one folder (nes/famicom,
// snes/sfam, lynx/atarilynx, arcade/fbneo …), so we return the lexicographically
// first match for a STABLE, deterministic result (the same rule as onion/muos).
// This also powers catalog's folder-name-native resolve (#182): the folder name
// IS the tag on Knulli, so a path under roms/<slug>/ reverses with no mapping.
func FsSlugForTag(tag string) (string, bool) {
	if tag == "" {
		return "", false
	}
	matches := make([]string, 0, 2)
	for slug, t := range knulliRomFolders {
		if t == tag {
			matches = append(matches, slug)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	sort.Strings(matches)
	return matches[0], true
}

// HasEmuPak reports whether Knulli can actually LAUNCH this system on THIS device —
// i.e. the roms/<slug> folder exists. Batocera/Knulli pre-creates a roms folder for
// every system the build supports (and only those), so folder existence is the
// per-device capability signal — the Knulli analog of MinUI's Emus/<TAG>.pak gate.
// It stops us stubbing a library the device can't play. A sandbox with no roms tree
// can bypass via LODOR_SKIP_EMU_GATE=1.
func HasEmuPak(tag string) bool {
	if tag == "" {
		return false
	}
	if os.Getenv("LODOR_SKIP_EMU_GATE") == "1" {
		return true
	}
	if st, err := os.Stat(filepath.Join(RomsDir(), tag)); err == nil && st.IsDir() {
		return true
	}
	return false
}

// PakDir resolves the app working directory (where config.json, catalog-index.json,
// mirror-manifest.json and the pending queue live): LODOR_PAK_DIR if exported by the
// app scripts, else the process CWD (the scripts cd into the app/data dir before
// invoking the engine), last resort ".". CFW-agnostic — identical to the other
// variants; duplicated because platform.go is excluded under -tags knulli.
func PakDir() string {
	if d := strings.TrimSpace(os.Getenv("LODOR_PAK_DIR")); d != "" {
		return d
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// MirrorFolderName builds the roms/ folder a platform's RomM games are mirrored
// into. On Knulli this is the bare Batocera slug (e.g. "megadrive") regardless of
// mirror mode — EmulationStation binds a roms/ subfolder to a system purely by that
// name, so there is no "<Display> (<TAG>)" form and no per-mode folder split. Mode
// separation from a user's own same-named games is handled instead by
// LocalBasename's " (RomM)" filename disambiguator, not by the folder. display is
// unused on Knulli — the folder must be exactly the slug ES recognises.
func MirrorFolderName(display, tag, mode string) string {
	return tag
}

// saveArtifactAnchors returns the filename prefixes (anchored with a trailing ".")
// that identify THIS game's save/state artifacts in a save folder. Knulli runs
// stock RetroArch, which derives save/state names from the ROM basename with its
// extension STRIPPED ("Game (USA).srm", "Game (USA).state1") — so the anchor is
// the stem, not MinUI's full-filename form. The full-filename anchor is kept too
// (harmless when no such file exists) so an artifact staged under the MinUI rule
// still migrates.
func saveArtifactAnchors(romBase string) []string {
	stem := strings.TrimSuffix(romBase, filepath.Ext(romBase))
	if stem == "" || stem == romBase {
		return []string{romBase}
	}
	return []string{stem, romBase}
}

// CanonicalMirrorFolder returns the ONLY roms/ folder name Knulli will recognise
// for a slug — the Batocera system slug es_systems binds. ES resolves a system
// PURELY by this folder name, so a directory_mappings relative_path that differs
// (a MinUI "Sega Mega Drive (MD)" carried across CFWs, an onion bare "MD", or a
// hand-edit) is invisible to the launcher and must be HEALED.
// catalog.ensureDirectoryMappings calls this to snap such a mapping back to the
// slug. Returns "" for a slug Knulli does not map so the caller leaves it.
func CanonicalMirrorFolder(fsSlug string) string {
	if f, ok := knulliRomFolders[fsSlug]; ok {
		return f
	}
	return ""
}

// ---------------------------------------------------------------------------
// CFW-agnostic helpers (identical to the default in platform.go; duplicated here
// because that file is excluded under -tags knulli). Keep in sync at foldback.
// ---------------------------------------------------------------------------

// platformRomDirectory returns the directory under RomsDir where a ROM with the given
// fs_slug lives: directory_mappings[fs_slug].relative_path when set, else the fs_slug
// folder, else the platform display name, else the fs_slug.
func platformRomDirectory(cfg *config.Config, fsSlug, displayName string) string {
	folder := fsSlug
	if cfg != nil {
		if m, ok := cfg.DirectoryMappings[fsSlug]; ok {
			if m.RelativePath != "" {
				folder = m.RelativePath
			} else {
				folder = fsSlug
			}
			return filepath.Join(RomsDir(), folder)
		}
	}
	// No mapping: prefer the authoritative Batocera slug so a stub lands in a folder
	// ES will recognise; else the display name; else the fs_slug.
	if f, ok := knulliRomFolders[fsSlug]; ok {
		folder = f
	} else if displayName != "" {
		folder = displayName
	}
	return filepath.Join(RomsDir(), folder)
}

// archiveRawExt maps a RomM fs_slug to the raw ROM extension its standalone emulator
// needs when the server stores the game inside a .7z the emulator cannot open. The
// engine extracts the .7z to this extension on download. NDS/DraStic is the case:
// DraStic reads raw .nds (and .zip) but NOT .7z. Kept for parity with the default
// build (Batocera's melonDS/DraStic hosts read raw .nds fine).
var archiveRawExt = map[string]string{
	"nds": ".nds",
}

// ArchiveExtractTargetForRom reports whether a ROM is stored in a .7z that must be
// extracted to a raw file on download, and the raw extension that file takes. Only .7z
// triggers it — .zip is left alone for emulators that read it natively. Mirrors the
// default definition exactly so cmd/lodor-sync (shared) resolves the same on all builds.
func ArchiveExtractTargetForRom(rom romm.Rom) (targetExt string, needsExtract bool) {
	if len(rom.Files) == 0 {
		return "", false
	}
	if !strings.EqualFold(filepath.Ext(rom.Files[0].FileName), ".7z") {
		return "", false
	}
	if e, ok := archiveRawExt[rom.PlatformFsSlug]; ok {
		return e, true
	}
	return "", false
}

// onDiskExt is the extension the local stub/file takes: the extracted raw extension
// when the server stores the game in an extract-on-download .7z, else the server
// file's own extension. Keeps the stub EmulationStation sees (and the file the
// emulator opens) a raw ROM rather than an unopenable .7z.
func onDiskExt(rom romm.Rom) string {
	if t, ok := ArchiveExtractTargetForRom(rom); ok {
		return t
	}
	if len(rom.Files) > 0 {
		return filepath.Ext(rom.Files[0].FileName)
	}
	return ""
}

// romMDisambiguator is the marker appended to a RomM stub's basename in "separate"
// mirror mode so a RomM stub's save (and on-disk file) can never collide with a user's
// own same-named game. Merge mode is canonical BY DESIGN (C1 §2, the "RomM-first"
// 2026-06-28 cut) — the leading ✘/✓ state markers keep a Lodor stub/download from ever
// being byte-equal to the user's own filename (Knulli keeps markers: stock ES launcher,
// HostShowsStateNatively is hard-false; the gamelist <name> hides them on screen).
const romMDisambiguator = " (RomM)"

// LocalBasename returns the extension-less on-disk basename a ROM occupies under the
// active mirror mode. In "own" AND "merge" modes it is rom.CanonicalLocalBasename() —
// byte-identical to the server's name (merge-canonical is what makes dedup-by-index-
// adoption free; see the default variant's comment). In "separate" mode it appends
// romMDisambiguator. Single source of truth shared by LocalRomPath and the catalog
// index keys. SAME semantics as the current default and muos builds.
func LocalBasename(cfg *config.Config, rom romm.Rom) string {
	base := rom.CanonicalLocalBasename()
	if base == "" || cfg.ResolvedMirrorMode() != config.MirrorModeSeparate {
		return base
	}
	return base + romMDisambiguator
}

// LocalRomPath returns the absolute on-disk path a ROM occupies under RomsDir:
// <RomsDir>/<mapped folder>/<basename><ext> for single-file ROMs, or
// <RomsDir>/<mapped folder>/<basename>.m3u for multi-file ROMs. Returns "" when the ROM
// has no platform slug or no resolvable file.
func LocalRomPath(cfg *config.Config, rom romm.Rom) string {
	if rom.PlatformFsSlug == "" {
		return ""
	}
	romDir := platformRomDirectory(cfg, rom.PlatformFsSlug, rom.PlatformDisplayName)
	base := LocalBasename(cfg, rom)
	if rom.HasMultipleFiles {
		return filepath.Join(romDir, base+".m3u")
	}
	if len(rom.Files) > 0 {
		return filepath.Join(romDir, base+onDiskExt(rom))
	}
	return ""
}

// MultiDiscDir returns the per-game subfolder a multi-file ROM's discs are written into:
// <RomsDir>/<mapped folder>/<FsNameNoExt>/. Returns "" when the ROM has no platform slug.
func MultiDiscDir(cfg *config.Config, rom romm.Rom) string {
	if rom.PlatformFsSlug == "" {
		return ""
	}
	romDir := platformRomDirectory(cfg, rom.PlatformFsSlug, rom.PlatformDisplayName)
	return filepath.Join(romDir, rom.FsNameNoExt)
}
