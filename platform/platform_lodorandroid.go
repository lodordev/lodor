//go:build android || lodorandroid

// Android platform variant — the helper-app lane (the Lodor app running UNDER an
// existing launcher frontend: ES-DE first, Daijisho/Pegasus later). Built with
// `-tags android`. Unlike every CFW lane there is no card and no launch shim: the
// Kotlin app (integrations/android) is the "shell" — it execs this engine from the
// APK's native-lib dir, exports the env below on every invocation, and plays the
// role lodor-hook.sh plays on Knulli.
//
// Layout facts (v1 = RetroArch-only, decided 2026-07-07):
//   - ROMs/stubs live in the shared-storage folder the FRONTEND scans (user-chosen
//     at onboarding; conventionally /storage/emulated/0/ROMs/<system>/). Folder
//     names are ES-DE system names — ES-DE binds folder→system by name, exactly
//     like Batocera/Knulli, and the two slug sets are near-identical.
//   - Saves: RetroArch Android defaults to shared storage
//     /storage/emulated/0/RetroArch/saves with sorting DISABLED → FLAT directory
//     ("Game (USA).srm" next to every other system's saves). The user can enable
//     sort-by-content-dir or sort-by-core in RA; the app detects the layout at
//     onboarding and pins it per-launch via LODOR_SAVE_SUBDIR (the cross-CFW
//     contract) — "." pins the flat root. Discovery (EmulatorFoldersForFSSlug)
//     scans root + first-level subdirs (muOS superset pattern) so pending pushes
//     find saves regardless of the user's RA sort setting.
//   - BIOS: RetroArch's system dir, flat (/storage/emulated/0/RetroArch/system).
//   - config.json/ledgers/queues: the app's PRIVATE filesDir (the bearer token
//     must never sit in world-readable shared storage) — the app sets
//     LODOR_PAK_DIR and the process CWD there.
//
// Standalone emulators (Dolphin, DuckStation…) keep saves in Android/data/<pkg>,
// unreachable without root — out of scope v1, and why this file is RA-shaped.
//
// SANDBOX RELOCATION: identical contract to the other lanes — ROMS_DIR/SAVES_DIR/
// BIOS_DIR override individual roots; a set BASE_PATH relocates the WHOLE tree to
// <BASE_PATH>/{Roms,Saves,Bios} so the off-hardware Panther suite exercises one
// consistent tree. On device the app exports the real paths and BASE_PATH stays
// unset. CGO-free, stdlib only.
package platform

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/config"
	"lodor/romm"
)

// androidRomFolders maps a RomM filesystem slug to the ES-DE system folder name.
//
// AUDITED against ES-DE's Android es_systems.xml (master, 2026-07-08): every value
// below is a literal <name> in the stock file. Divergences from Batocera:
// wonderswan/wonderswancolor, atarilynx, atarijaguar, sg-1000, arcade≠fbneo. A slug absent here is SKIPPED during mapping generation (no folder
// invented); HasEmuPak further gates on the folder actually existing (the app
// pre-creates folders only for systems the user enabled at onboarding).
var androidRomFolders = map[string]string{
	// --- Nintendo ---
	"gb":           "gb",
	"gbc":          "gbc",
	"gba":          "gba",
	"nes":          "nes",
	"famicom":      "nes", // RomM alias — same ES-DE folder
	"fds":          "fds",
	"snes":         "snes",
	"sfam":         "snes", // RomM alias (Super Famicom)
	"n64":          "n64",
	"nds":          "nds",
	"virtualboy":   "virtualboy",
	"pokemon-mini": "pokemini",
	"pokemini":     "pokemini",
	// --- Sony ---
	"psx": "psx",
	"psp": "psp",
	// --- Sega ---
	"genesis":      "megadrive",
	"sms":          "mastersystem",
	"mastersystem": "mastersystem",
	"gamegear":     "gamegear",
	"sg1000":       "sg-1000",
	"segacd":       "segacd",
	"sega32":       "sega32x",
	"sega32x":      "sega32x",
	"dc":           "dreamcast",
	"dreamcast":    "dreamcast",
	"saturn":       "saturn",
	// --- NEC ---
	"tg16":          "pcengine",
	"pcengine":      "pcengine",
	"turbografx-cd": "pcenginecd",
	"supergrafx":    "supergrafx",
	// --- Atari ---
	"atari2600":   "atari2600",
	"atari5200":   "atari5200",
	"atari7800":   "atari7800",
	"lynx":        "atarilynx",
	"atarilynx":   "atarilynx",
	"jaguar":      "atarijaguar",
	"atarijaguar": "atarijaguar",
	// --- SNK ---
	"neogeoaes":            "neogeo",
	"neogeomvs":            "neogeo",
	"neo-geo-cd":           "neogeocd",
	"neo-geo-pocket":       "ngp",
	"neo-geo-pocket-color": "ngpc",
	// --- Bandai --- (ES-DE divergence from Batocera: full words, not wswan/wswanc)
	"wonderswan":       "wonderswan",
	"wonderswan-color": "wonderswancolor",
	"wonderswancolor":  "wonderswancolor",
	"wswan":            "wonderswan",
	"wswanc":           "wonderswancolor",
	// --- Arcade --- (ES-DE has BOTH an "arcade" and an "fbneo" system folder,
	// unlike Batocera; map each RomM slug to its own)
	"arcade": "arcade",
	"fbneo":  "fbneo",
	// --- Other ---
	"colecovision": "colecovision",
	"scummvm":      "scummvm",
	"pico-8":       "pico8",
	"dos":          "dos",
	"ports":        "ports",
	"c64":          "c64",
}

// saveSubdirEnv is set by the app to pin the save subfolder for THIS launch (the
// cross-CFW contract shared with onion/muos/knulli). On Android the app ALWAYS pins
// it (it detected RA's sort layout at onboarding and knows the core it is about to
// launch): "." = RA's default flat root, a system folder name = sort-by-content-dir,
// a core display name = sort-by-core. Unset (bare engine invocation, sandbox) falls
// back to the flat default below.
const saveSubdirEnv = "LODOR_SAVE_SUBDIR"

// envOr returns the value of environment variable key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BasePath returns the Android shared-storage root: BASE_PATH if set (sandbox
// relocation), else /storage/emulated/0. On device the app exports the real
// ROMS_DIR/SAVES_DIR/BIOS_DIR individually, so this default is rarely load-bearing.
func BasePath() string { return envOr("BASE_PATH", "/storage/emulated/0") }

// RomsDir returns the ROMs root the frontend scans: ROMS_DIR if set (the app always
// sets it — the user chose the folder at onboarding); else the relocated generic
// <BASE_PATH>/Roms when BASE_PATH is set (sandbox); else the conventional
// /storage/emulated/0/ROMs.
func RomsDir() string {
	if v := os.Getenv("ROMS_DIR"); v != "" {
		return v
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return filepath.Join(bp, "Roms")
	}
	return "/storage/emulated/0/ROMs"
}

// BiosDir returns the BIOS dir: BIOS_DIR if set; else <BASE_PATH>/Bios when BASE_PATH
// is set (sandbox); else RetroArch Android's flat system dir.
func BiosDir() string {
	if v := os.Getenv("BIOS_DIR"); v != "" {
		return v
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return filepath.Join(bp, "Bios")
	}
	return "/storage/emulated/0/RetroArch/system"
}

// SavesDir returns the RetroArch savefile root: SAVES_DIR if set; else <BASE_PATH>/
// Saves when BASE_PATH is set (sandbox); else RetroArch Android's shared-storage
// default. RA ships sorting OFF → saves sit FLAT in this directory unless the user
// enabled a sort mode (then the app pins the subfolder via LODOR_SAVE_SUBDIR).
func SavesDir() string {
	if v := os.Getenv("SAVES_DIR"); v != "" {
		return v
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return filepath.Join(bp, "Saves")
	}
	return "/storage/emulated/0/RetroArch/saves"
}

// EmulatorFoldersForFSSlug returns the save folders to SCAN for a slug. An
// app-pinned LODOR_SAVE_SUBDIR wins; otherwise the superset: the flat root itself
// (".", RA's default) plus every first-level subdirectory (covers sort-by-content-dir
// and sort-by-core without knowing which the user picked — the muOS discovery
// pattern widened by the flat root).
func EmulatorFoldersForFSSlug(slug string) []string {
	if sub := os.Getenv(saveSubdirEnv); sub != "" {
		return []string{sub}
	}
	folders := []string{"."}
	entries, err := os.ReadDir(SavesDir())
	if err != nil {
		return folders
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			folders = append(folders, e.Name())
		}
	}
	return folders
}

// SaveFileName returns the RetroArch on-disk save filename: the ROM basename with its
// extension stripped, plus the real save extension — SaveFileName("Game (USA).gba",
// "srm") == "Game (USA).srm". Identical to the onion/muos/knulli RetroArch rule.
func SaveFileName(romFullFilename, saveExt string) string {
	base := strings.TrimSuffix(romFullFilename, filepath.Ext(romFullFilename))
	ext := strings.TrimPrefix(saveExt, ".")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

// SaveDirectory returns the canonical save directory for a write (pull/restore): an
// app-pinned LODOR_SAVE_SUBDIR wins ("." cleans to the flat root); else the flat
// SavesDir itself for any known slug — RA's shipped default. Unknown slug returns ""
// (no save directory — never blind-write).
func SaveDirectory(slug string) string {
	if sub := os.Getenv(saveSubdirEnv); sub != "" {
		return filepath.Join(SavesDir(), sub)
	}
	if _, ok := androidRomFolders[slug]; ok {
		return SavesDir()
	}
	return ""
}

// BIOSFilePaths returns the BIOS destination(s) for a file: a single flat
// <BiosDir>/<base> — RetroArch reads system files from one directory, like Batocera.
func BIOSFilePaths(fileName, slug string) []string {
	return []string{filepath.Join(BiosDir(), filepath.Base(fileName))}
}

// PrimaryTag returns the ES-DE system folder name for a RomM fs_slug (used to build
// directory_mappings and the mirror folder), and ok=false when the slug isn't a
// known ES-DE system (caller SKIPs it, inventing no folder). Like Knulli, the "tag"
// IS the folder name: ES-DE binds a ROMs/ subfolder to a system purely by that name.
func PrimaryTag(fsSlug string) (tag string, ok bool) {
	if f, ok := androidRomFolders[fsSlug]; ok {
		return f, true
	}
	return "", false
}

// FsSlugForTag reverses androidRomFolders: given an ES-DE folder name, return a RomM
// fs_slug that maps to it. Aliases collapse (nes/famicom, lynx/atarilynx …), so we
// return the lexicographically first match for a STABLE, deterministic result (the
// shared cross-lane rule).
func FsSlugForTag(tag string) (string, bool) {
	if tag == "" {
		return "", false
	}
	matches := make([]string, 0, 2)
	for slug, t := range androidRomFolders {
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

// HasEmuPak reports whether this device can actually LAUNCH the system — i.e. the
// ROMs/<folder> directory exists. The app pre-creates folders ONLY for systems the
// user enabled (and mapped to a core) at onboarding, so folder existence is the
// per-device capability signal — the Android analog of Knulli's pre-created roms
// folders. A sandbox with no roms tree can bypass via LODOR_SKIP_EMU_GATE=1.
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
// mirror-manifest.json and the pending queues live): LODOR_PAK_DIR if exported by
// the app (always, on device — it points into the app's private filesDir), else the
// process CWD, last resort ".". CFW-agnostic — identical to the other variants;
// duplicated because platform.go is excluded under -tags android.
func PakDir() string {
	if d := strings.TrimSpace(os.Getenv("LODOR_PAK_DIR")); d != "" {
		return d
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// MirrorFolderName builds the ROMs/ folder a platform's RomM games are mirrored
// into. Like Knulli this is the bare ES-DE system name regardless of mirror mode —
// the frontend binds folder→system purely by that name. Mode separation from a
// user's own same-named games is handled by LocalBasename's " (RomM)" filename
// disambiguator, not by the folder. display is unused.
func MirrorFolderName(display, tag, mode string) string {
	return tag
}

// saveArtifactAnchors returns the filename prefixes (anchored with a trailing ".")
// that identify THIS game's save/state artifacts in a save folder. RetroArch derives
// save/state names from the ROM basename with its extension STRIPPED ("Game
// (USA).srm", "Game (USA).state1") — so the anchor is the stem. The full-filename
// anchor is kept too (harmless when no such file exists) so an artifact staged under
// the MinUI rule still migrates.
func saveArtifactAnchors(romBase string) []string {
	stem := strings.TrimSuffix(romBase, filepath.Ext(romBase))
	if stem == "" || stem == romBase {
		return []string{romBase}
	}
	return []string{stem, romBase}
}

// CanonicalMirrorFolder returns the ONLY ROMs/ folder name the frontend will
// recognise for a slug — the ES-DE system name. A directory_mappings relative_path
// that differs (carried across from another CFW, or a hand-edit) is invisible to the
// frontend and must be HEALED; catalog.ensureDirectoryMappings calls this to snap
// such a mapping back. Returns "" for a slug Android does not map so the caller
// leaves it.
func CanonicalMirrorFolder(fsSlug string) string {
	if f, ok := androidRomFolders[fsSlug]; ok {
		return f
	}
	return ""
}

// ---------------------------------------------------------------------------
// CFW-agnostic helpers (identical to the default in platform.go; duplicated here
// because that file is excluded under -tags android). Keep in sync at foldback.
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
	// No mapping: prefer the authoritative ES-DE name so a stub lands in a folder
	// the frontend will recognise; else the display name; else the fs_slug.
	if f, ok := androidRomFolders[fsSlug]; ok {
		folder = f
	} else if displayName != "" {
		folder = displayName
	}
	return filepath.Join(RomsDir(), folder)
}

// archiveRawExt maps a RomM fs_slug to the raw ROM extension its emulator needs when
// the server stores the game inside a .7z the emulator cannot open. NDS is the case
// (RA's DraStic-adjacent cores and standalone DraStic read raw .nds, not .7z).
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
// file's own extension. Keeps the stub the frontend sees (and the file the emulator
// opens) a raw ROM rather than an unopenable .7z.
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
// mirror mode so a RomM stub's save (and on-disk file) can never collide with a
// user's own same-named game. NOTE the Android difference from Knulli: this lane is
// MARKER-LESS (HostShowsStateNatively is hard-true — see hoststate_android.go), so in
// merge mode a RomM file IS byte-named like a user's own; the catalog's dedup-by-
// index-adoption handles that identically to LodorOS.
const romMDisambiguator = " (RomM)"

// LocalBasename returns the extension-less on-disk basename a ROM occupies under the
// active mirror mode. In "own" AND "merge" modes it is rom.CanonicalLocalBasename() —
// byte-identical to the server's name. In "separate" mode it appends
// romMDisambiguator. Single source of truth shared by LocalRomPath and the catalog
// index keys. Same semantics as every other lane.
func LocalBasename(cfg *config.Config, rom romm.Rom) string {
	base := rom.CanonicalLocalBasename()
	if base == "" || cfg.ResolvedMirrorMode() != config.MirrorModeSeparate {
		return base
	}
	return base + romMDisambiguator
}

// LocalRomPath returns the absolute on-disk path a ROM occupies under RomsDir:
// <RomsDir>/<mapped folder>/<basename><ext> for single-file ROMs, or
// <RomsDir>/<mapped folder>/<basename>.m3u for multi-file ROMs. Returns "" when the
// ROM has no platform slug or no resolvable file.
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

// MultiDiscDir returns the per-game subfolder a multi-file ROM's discs are written
// into: <RomsDir>/<mapped folder>/<FsNameNoExt>/. Returns "" when the ROM has no
// platform slug.
func MultiDiscDir(cfg *config.Config, rom romm.Rom) string {
	if rom.PlatformFsSlug == "" {
		return ""
	}
	romDir := platformRomDirectory(cfg, rom.PlatformFsSlug, rom.PlatformDisplayName)
	return filepath.Join(romDir, rom.FsNameNoExt)
}
