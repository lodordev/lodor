//go:build spruce

// spruceOS platform variant — the no-fork integration for the Miyoo A30 (Allwinner
// A33, ARMv7 / armhf) and the wider spruce fleet. Built with `-tags spruce`. Pinned
// against spruceUI/spruceOS `main` ground truth (spruce/scripts/helperFunctions.sh,
// spruce/scripts/emu/standard_launch.sh, RetroArch/.config/retroarch/retroarch.cfg,
// and the shipped Emu/<TAG>/config.json set), read live 2026-07-13.
//
// spruceOS is a MinUI-family firmware, but its on-card layout is OnionOS-shaped, so this
// variant is a near-clone of platform_onion.go with spruce's real paths substituted:
//   - ROMs live in /mnt/SDCARD/Roms/<TAG>/ where <TAG> is spruce's fixed system folder
//     code (GBA, FC, PS, MD …) — the Emu/<TAG>/ folder names shipped in the repo. NOT
//     MinUI's "<Display> (<TAG>)" folder. So the mirror folder IS the bare tag.
//   - It runs stock RetroArch. retroarch.cfg (verified from the shipped config) sets
//     savefile_directory=/mnt/SDCARD/Saves/saves with sort_savefiles_enable=true, so
//     battery saves land at /mnt/SDCARD/Saves/saves/<CoreName>/<rom-basename>.<ext>.
//     States: savestate_directory=/mnt/SDCARD/Saves/states, sort_savestates_enable=true
//     → /mnt/SDCARD/Saves/states/<CoreName>/. The folder is the RetroArch CORE folder
//     name (spruce cores sort by the core's own libretro name), NOT the system/slug —
//     and a system can be launched by several cores, so the save directory is
//     CORE-DRIVEN, exactly like OnionOS/muOS.
//   - The on-disk save filename is the ROM basename WITHOUT its extension + the save
//     extension ("Game (USA).srm") — the RetroArch rule.
//   - BIOS is a single flat directory (system_directory=/mnt/SDCARD/BIOS).
//
// HOW THE CORE IS KNOWN: the Lodor launch wrapper (App/Lodor/lodor_launch.sh, repointed
// into each Emu/<TAG>/config.json's `launch` field) reads the system's config.json
// (default_emulator / the device-scoped Emulator*.selected option) to learn the exact
// core for the launch and exports its RetroArch core-folder name as LODOR_SAVE_SUBDIR
// before invoking the engine, so save read/write lands exactly where RetroArch will
// look. For the context-free daemon path (--push-pending, no env) we DISCOVER saves by
// scanning every existing core folder and matching by ROM basename — immune to spruce
// reshuffling core names between releases.
//
// All card roots are env-overridable (BASE_PATH / ROMS_DIR / SAVES_DIR / BIOS_DIR) so
// the off-hardware sandbox can relocate the tree without a device. The pak working dir
// is resolved via PakDir() (LODOR_PAK_DIR), exactly like the !onion default. CGO-free,
// stdlib only.
package platform

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/config"
	"lodor/romm"
)

// spruceRomTags maps a RomM filesystem slug to the spruce Roms/ system folder TAG.
// These are spruce's fixed folder codes (the shipped Emu/<TAG>/ folder names, read from
// spruceUI/spruceOS `main`). A slug absent here is SKIPPED during mapping generation (no
// folder invented), and HasEmuPak further gates on the Emu/<TAG> folder actually
// existing on the card so we never stub a system the A30 can't launch. Extend by reading
// the card's real Emu/ folder names.
var spruceRomTags = map[string]string{
	"gba":                  "GBA",
	"gbc":                  "GBC",
	"gb":                   "GB",
	"snes":                 "SFC",
	"sfam":                 "SFC",
	"nes":                  "FC",
	"famicom":              "FC",
	"fds":                  "FDS",
	"genesis":              "MD",
	"sms":                  "MS",
	"mastersystem":         "MS",
	"gamegear":             "GG",
	"segacd":               "SEGACD",
	"sega32":               "THIRTYTWOX",
	"sega32x":              "THIRTYTWOX",
	"sg1000":               "SEGASGONE",
	"virtualboy":           "VB",
	"pokemon-mini":         "POKE",
	"pokemini":             "POKE",
	"tg16":                 "PCE",
	"pcengine":             "PCE",
	"turbografx-cd":        "PCECD",
	"supergrafx":           "SGFX",
	"neogeoaes":            "NEOGEO",
	"neogeomvs":            "NEOGEO",
	"neo-geo-cd":           "NEOCD",
	"neo-geo-pocket":       "NGP",
	"neo-geo-pocket-color": "NGP",
	"wonderswan":           "WS",
	"wonderswan-color":     "WSC",
	"lynx":                 "LYNX",
	"atarilynx":            "LYNX",
	"atari2600":            "ATARI",
	"atari5200":            "FIFTYTWOHUNDRED",
	"atari7800":            "EIGHTHUNDRED",
	"psx":                  "PS",
	"n64":                  "N64",
	"arcade":               "ARCADE",
	"fbneo":                "FBNEO",
	"msx":                  "MSX",
	"scummvm":              "SCUMMVM",
	"pico-8":               "PICO8",
	"doom":                 "DOOM",
	"amiga":                "AMIGA",
	"dos":                  "DOS",
	"ports":                "PORTS",
	"colecovision":         "COLECO",
	"c64":                  "COMMODORE",
	"acpc":                 "CPC",
	"dc":                   "DC",
	"nds":                  "NDS",
	"psp":                  "PSP",
	"fairchild-channel-f":  "FAIRCHILD",
	"arduboy":              "ARDUBOY",
}

// spruceDefaultCore maps a slug to the RetroArch core FOLDER name of the system's
// default core on spruce (the sort_savefiles_enable per-core folder), used ONLY as a
// save-directory fallback when LODOR_SAVE_SUBDIR is unset (the daemon path, not the
// wrapper). Values are the libretro core names as spruce's Emu config.json
// `default_emulator` records them (fceumm, gpsp, pcsx_rearmed …) — spruce's RA sorts
// savefiles into a folder named for the running core, and on this fleet that folder is
// the core's own name. A slug absent here yields no fallback save dir (we never
// blind-write to a guessed folder). The wrapper always sets the env, so this is a safety
// net, not the primary path. Verify against the card's actual Saves/saves/<folder> set.
var spruceDefaultCore = map[string]string{
	"gb":                   "gambatte",
	"gbc":                  "gambatte",
	"gba":                  "mgba",
	"snes":                 "snes9x",
	"sfam":                 "snes9x",
	"nes":                  "fceumm",
	"famicom":              "fceumm",
	"fds":                  "fceumm",
	"genesis":              "genesis_plus_gx",
	"gamegear":             "genesis_plus_gx",
	"sms":                  "genesis_plus_gx",
	"mastersystem":         "genesis_plus_gx",
	"segacd":               "genesis_plus_gx",
	"sega32":               "picodrive",
	"sega32x":              "picodrive",
	"lynx":                 "handy",
	"atarilynx":            "handy",
	"psx":                  "pcsx_rearmed",
	"n64":                  "mupen64plus_next",
	"tg16":                 "mednafen_pce_fast",
	"pcengine":             "mednafen_pce_fast",
	"wonderswan":           "mednafen_wswan",
	"wonderswan-color":     "mednafen_wswan",
	"virtualboy":           "mednafen_vb",
	"neo-geo-pocket":       "mednafen_ngp",
	"neo-geo-pocket-color": "mednafen_ngp",
}

// spruceEmuFolder overrides the Emu/ folder name for the few systems where spruce's
// Emu/<folder> differs from the Roms/<TAG> folder. Default (absent here) = the TAG (on
// spruce the Roms tag and Emu folder are the same for the systems Lodor mirrors — the
// shipped config uses "PS" for both Emu/PS and Roms/PS). Kept for parity/extension.
var spruceEmuFolder = map[string]string{}

// saveSubdirEnv is set by the launch wrapper to the core folder name RetroArch will use
// for THIS launch. When present it pins the save read/write to the exact folder; when
// absent the daemon falls back to glob-all discovery + the spruceDefaultCore table.
const saveSubdirEnv = "LODOR_SAVE_SUBDIR"

// envOr returns the value of environment variable key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BasePath returns the spruce card root: BASE_PATH if set, else /mnt/SDCARD.
func BasePath() string { return envOr("BASE_PATH", "/mnt/SDCARD") }

// RomsDir returns the spruce ROMs root: ROMS_DIR if set, else <BasePath>/Roms.
func RomsDir() string { return envOr("ROMS_DIR", filepath.Join(BasePath(), "Roms")) }

// BiosDir returns the spruce BIOS dir: BIOS_DIR if set, else <BasePath>/BIOS
// (system_directory in the shipped retroarch.cfg).
func BiosDir() string { return envOr("BIOS_DIR", filepath.Join(BasePath(), "BIOS")) }

// SavesDir returns the spruce RetroArch savefile root: SAVES_DIR if set, else
// <BasePath>/Saves/saves (savefile_directory in the shipped retroarch.cfg, with
// sort_savefiles_enable=true → per-core subfolders below it).
func SavesDir() string {
	return envOr("SAVES_DIR", filepath.Join(BasePath(), "Saves", "saves"))
}

// EmulatorFoldersForFSSlug returns the save folders to SCAN for a slug. When the wrapper
// has pinned the launch core (LODOR_SAVE_SUBDIR set) we scan only that folder; otherwise
// (the daemon's --push-pending) we scan EVERY existing core folder under SavesDir so a
// changed save is found wherever its core wrote it, regardless of which core launched it.
func EmulatorFoldersForFSSlug(slug string) []string {
	if sub := os.Getenv(saveSubdirEnv); sub != "" {
		return []string{sub}
	}
	entries, err := os.ReadDir(SavesDir())
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// SaveFileName returns the spruce/RetroArch on-disk save filename: the ROM basename with
// its extension stripped, plus the real save extension — SaveFileName("Game (USA).gba",
// "srm") == "Game (USA).srm". An empty saveExt yields just the stripped basename.
func SaveFileName(romFullFilename, saveExt string) string {
	base := strings.TrimSuffix(romFullFilename, filepath.Ext(romFullFilename))
	ext := strings.TrimPrefix(saveExt, ".")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

// SaveDirectory returns the canonical save directory for a write (pull/restore). The
// wrapper-pinned LODOR_SAVE_SUBDIR wins (the core that will actually read the save); else
// the system's default-core folder; else "" (no save directory — never blind-write).
func SaveDirectory(slug string) string {
	if sub := os.Getenv(saveSubdirEnv); sub != "" {
		return filepath.Join(SavesDir(), sub)
	}
	if core, ok := spruceDefaultCore[slug]; ok {
		return filepath.Join(SavesDir(), core)
	}
	return ""
}

// BIOSFilePaths returns the spruce BIOS destination(s) for a file: a single flat
// <BiosDir>/<base>. spruce keeps BIOS in one directory (system_directory).
func BIOSFilePaths(fileName, slug string) []string {
	return []string{filepath.Join(BiosDir(), filepath.Base(fileName))}
}

// PrimaryTag returns the spruce Roms/ system TAG for a slug (used to build
// directory_mappings and the mirror folder), and ok=false when the slug isn't a known
// spruce system (caller SKIPs it).
func PrimaryTag(fsSlug string) (tag string, ok bool) {
	if t, ok := spruceRomTags[fsSlug]; ok {
		return t, true
	}
	return "", false
}

// FsSlugForTag reverses spruceRomTags: given a spruce Roms/ system TAG, return a RomM
// fs_slug that maps to it (lexicographically-first on aliases, for a STABLE result).
// On real spruce this path is effectively dead: the mirror writes BARE-tag folders, so
// catalog's tagFromFolderName yields "" and never calls this. Kept deterministic.
func FsSlugForTag(tag string) (string, bool) {
	if tag == "" {
		return "", false
	}
	matches := make([]string, 0, 2)
	for slug, t := range spruceRomTags {
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

// HasEmuPak reports whether spruce can actually LAUNCH this system on THIS card — i.e.
// the Emu/<TAG>/ folder exists (spruce ships emulators as Emu/<TAG>/config.json). This is
// the spruce analog of MinUI's Emus/<TAG>.pak gate: it stops us stubbing a library the
// A30 can't play. Honors BASE_PATH. A card whose Emu/ isn't populated yet (pre-install
// sandbox) can bypass via LODOR_SKIP_EMU_GATE=1.
func HasEmuPak(tag string) bool {
	if tag == "" {
		return false
	}
	if os.Getenv("LODOR_SKIP_EMU_GATE") == "1" {
		return true
	}
	emu := tag
	if f, ok := spruceEmuFolder[tag]; ok {
		emu = f
	}
	p := filepath.Join(BasePath(), "Emu", emu)
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return true
	}
	return false
}

// PakDir resolves the pak working directory (where config.json, catalog-index.json, and
// the pending queue live): LODOR_PAK_DIR if exported by the pak scripts, else the process
// CWD (the scripts cd into the pak/data dir before invoking the engine), last resort ".".
func PakDir() string {
	if d := strings.TrimSpace(os.Getenv("LODOR_PAK_DIR")); d != "" {
		return d
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// saveArtifactAnchors returns the filename prefixes (anchored with a trailing ".") that
// identify THIS game's save/state artifacts in a save folder. spruce runs stock
// RetroArch, which derives save/state names from the ROM basename with its extension
// STRIPPED ("Game (USA).srm", "Game (USA).state1") — so the anchor is the stem. The
// full-filename anchor is kept too (harmless when no such file exists) so an artifact
// staged under the MinUI rule still migrates.
func saveArtifactAnchors(romBase string) []string {
	stem := strings.TrimSuffix(romBase, filepath.Ext(romBase))
	if stem == "" || stem == romBase {
		return []string{romBase}
	}
	return []string{stem, romBase}
}

// MirrorFolderName builds the Roms/ folder a platform's RomM games are mirrored into.
// On spruce this is the bare system TAG (e.g. "GBA") regardless of mirror mode — spruce
// binds a Roms/ subfolder to an emulator purely by that fixed folder code, so there is
// no "<Display> (<TAG>)" form and no per-mode folder split. Mode separation from a
// user's own same-named games is handled by LocalBasename's " (RomM)" filename
// disambiguator, not by the folder.
func MirrorFolderName(display, tag, mode string) string {
	return tag
}

// ---------------------------------------------------------------------------
// CFW-agnostic helpers (identical to the !onion default in platform.go; duplicated here
// because that file is excluded under -tags spruce). Keep in sync at foldback.
// ---------------------------------------------------------------------------

// platformRomDirectory returns the directory under RomsDir where a ROM with the given
// fs_slug lives: directory_mappings[fs_slug].relative_path when set (and safe), else the
// fs_slug folder, else the platform display name, else the fs_slug.
func platformRomDirectory(cfg *config.Config, fsSlug, displayName string) string {
	folder := fsSlug
	if cfg != nil {
		if m, ok := cfg.DirectoryMappings[fsSlug]; ok {
			// SECURITY: relative_path comes from config.json, which a co-installed hostile
			// app can write. Only honour it when it is a safe relative folder under Roms/.
			if m.RelativePath != "" && isSafeRelFolder(m.RelativePath) {
				return filepath.Join(RomsDir(), m.RelativePath)
			}
			if m.RelativePath == "" {
				return filepath.Join(RomsDir(), fsSlug)
			}
			// Unsafe relative_path: fall through to canonical resolution below.
		}
	}
	if displayName != "" {
		folder = displayName
	}
	return filepath.Join(RomsDir(), folder)
}

// archiveRawExt maps a RomM fs_slug to the raw ROM extension its standalone emulator
// needs when the server stores the game inside a .7z the emulator cannot open. NDS/DraStic
// is the case: DraStic reads raw .nds (and .zip) but NOT .7z.
var archiveRawExt = map[string]string{
	"nds": ".nds",
}

// ArchiveExtractTargetForRom reports whether a ROM is stored in a .7z that must be
// extracted to a raw file on download, and the raw extension that file takes.
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

// onDiskExt is the extension the local stub/file takes: the extracted raw extension when
// the server stores the game in an extract-on-download .7z, else the server file's own
// extension.
func onDiskExt(rom romm.Rom) string {
	if t, ok := ArchiveExtractTargetForRom(rom); ok {
		return t
	}
	if len(rom.Files) > 0 {
		return filepath.Ext(rom.Files[0].FileName)
	}
	return ""
}

// romMDisambiguator is the marker appended to a RomM stub's basename in non-"own" mirror
// modes so a RomM stub's save (and on-disk file) can never collide with a user's own
// same-named game in a different folder that binds the same TAG.
const romMDisambiguator = " (RomM)"

// LocalBasename returns the extension-less on-disk basename a ROM occupies under the
// active mirror mode: rom.CanonicalLocalBasename() in "own" mode, else suffixed.
func LocalBasename(cfg *config.Config, rom romm.Rom) string {
	base := rom.CanonicalLocalBasename()
	if base == "" || cfg.ResolvedMirrorMode() == config.MirrorModeOwn {
		return base
	}
	return base + romMDisambiguator
}

// LocalRomPath returns the absolute on-disk path a ROM occupies under RomsDir.
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
// <RomsDir>/<mapped folder>/.<FsNameNoExt>/ — dot-hidden (lodor#7 UX fix).
func MultiDiscDir(cfg *config.Config, rom romm.Rom) string {
	if rom.PlatformFsSlug == "" {
		return ""
	}
	romDir := platformRomDirectory(cfg, rom.PlatformFsSlug, rom.PlatformDisplayName)
	return filepath.Join(romDir, DiscFolderName(rom.FsNameNoExt))
}

// CanonicalMirrorFolder returns "" on spruce: the bare-TAG folder spruce mirror already
// writes IS canonical here, so the mirror never heals it (catalog.healMirrorFolders is a
// no-op on the spruce build, exactly as on onion).
func CanonicalMirrorFolder(fsSlug string) string { return "" }
