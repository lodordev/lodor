//go:build onion

// OnionOS (OnionUI) platform variant — the no-fork integration for the Miyoo Mini
// Plus (MY354, SigmaStar SSD202D). Built with `-tags onion`. Pinned against OnionOS
// v4.3.1-1 ground truth (onionui.github.io/docs + OnionUI/Onion source, 2026-06-27).
//
// Why OnionOS differs from MinUI (the !onion default in platform.go):
//   - ROMs live in /mnt/SDCARD/Roms/<TAG>/ where <TAG> is OnionOS's fixed system
//     folder code (GBA, SFC, FC, MD, PS …) — NOT MinUI's "<Display> (<TAG>)" folder.
//     So the mirror folder for a platform IS the bare tag (see MirrorFolderName).
//   - It runs stock RetroArch. Battery saves live at
//     /mnt/SDCARD/Saves/CurrentProfile/saves/<CoreDisplayName>/<rom-basename>.<ext>
//     (sort_savefiles_enable=true → per-core subfolders). The folder is the libretro
//     core DISPLAY name (e.g. "mGBA", "Snes9x"), NOT the system/slug — and a system
//     can be launched by several cores, so the save directory is CORE-DRIVEN, exactly
//     like muOS.
//   - The on-disk save filename is the ROM basename WITHOUT its extension + the save
//     extension ("Game (USA).srm") — the RetroArch rule, NOT MinUI's full-name+".sav".
//   - BIOS is a single flat directory (/mnt/SDCARD/BIOS — capital, unlike MinUI's Bios).
//
// HOW THE CORE IS KNOWN: the launch wrap (the per-system Emu/<TAG>/launch.sh shim)
// knows the exact core for the launch and exports its display name as
// LODOR_SAVE_SUBDIR before invoking the engine, so save read/write lands exactly where
// RetroArch will look. For the context-free daemon path (--push-pending, no env) we
// DISCOVER saves by scanning every existing core folder and matching by ROM basename —
// immune to OnionOS reshuffling core names between releases.
//
// All card roots are env-overridable (BASE_PATH / ROMS_DIR / SAVES_DIR / BIOS_DIR) so
// the off-hardware sandbox can relocate the tree without a device. The pak working dir
// is resolved via PakDir() (LODOR_PAK_DIR), exactly like the !onion default — there is
// no LODOR_DATA_DIR. CGO-free, stdlib only.
package platform

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/config"
	"lodor/romm"
)

// onionRomTags maps a RomM filesystem slug to the OnionOS Roms/ system folder TAG.
// These are OnionOS's fixed, documented folder codes (onionui.github.io/docs/emulators/
// folders) — they differ from MinUI's tags (OnionOS uses MS not SMS, ATARI not A2600,
// THIRTYTWOX not 32X, PS for PlayStation). A slug absent here is SKIPPED during mapping
// generation (no folder invented), and HasEmuPak further gates on the Emu/<TAG> folder
// actually existing on the card so we never stub a system the Mini Plus can't launch
// (NDS/PSP have folders on stronger devices but not the MMP). Extend by reading the
// card's real Emu/ folder names.
var onionRomTags = map[string]string{
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
	"atari7800":            "SEVENTYEIGHTHUNDRED",
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
}

// onionDefaultCore maps a slug to the libretro core DISPLAY name of the system's
// default core on OnionOS, used ONLY as a save-directory fallback when LODOR_SAVE_SUBDIR
// is unset (the daemon path, not the shim). Each value is a RetroArch core display name
// (the Saves/CurrentProfile/saves/<name>/ folder). A slug absent here yields no fallback
// save dir (we never blind-write to a guessed folder). The shim always sets the env, so
// this is a safety net, not the primary path. Verify against the card's actual core set.
var onionDefaultCore = map[string]string{
	"gb":           "Gambatte",
	"gbc":          "Gambatte",
	"gba":          "mGBA",
	"snes":         "Snes9x",
	"sfam":         "Snes9x",
	"nes":          "FCEUmm",
	"famicom":      "FCEUmm",
	"fds":          "FCEUmm",
	"genesis":      "Genesis Plus GX",
	"gamegear":     "Genesis Plus GX",
	"sms":          "Genesis Plus GX",
	"mastersystem": "Genesis Plus GX",
	"segacd":       "Genesis Plus GX",
	"sega32":       "PicoDrive",
	"sega32x":      "PicoDrive",
	"lynx":         "Handy",
	"atarilynx":    "Handy",
	"psx":          "PCSX-ReARMed",
	"n64":          "Mupen64Plus-Next",
	"tg16":         "Beetle PCE Fast",
	"pcengine":     "Beetle PCE Fast",
	"wonderswan":   "Beetle Cygne",
	"wonderswan-color": "Beetle Cygne",
	"virtualboy":   "Beetle VB",
	"neo-geo-pocket":       "Beetle NeoPop",
	"neo-geo-pocket-color": "Beetle NeoPop",
}

// onionEmuFolder overrides the Emu/ folder name for the few systems where OnionOS's
// Emu/<folder> differs from the Roms/<TAG> folder. Ground truth from a live MMP install
// (2026-06-27): Roms/PS games are launched by Emu/PSX. Default (absent here) = the TAG.
// The HasEmuPak gate must check the EMU folder, not the Roms tag, or it'd skip PlayStation.
var onionEmuFolder = map[string]string{
	"PS": "PSX",
}

// saveSubdirEnv is set by the launch wrap to the core display name RetroArch will use
// for THIS launch. When present it pins the save read/write to the exact folder; when
// absent the daemon falls back to glob-all discovery + the onionDefaultCore table.
const saveSubdirEnv = "LODOR_SAVE_SUBDIR"

// envOr returns the value of environment variable key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BasePath returns the OnionOS card root: BASE_PATH if set, else /mnt/SDCARD.
func BasePath() string { return envOr("BASE_PATH", "/mnt/SDCARD") }

// RomsDir returns the OnionOS ROMs root: ROMS_DIR if set, else <BasePath>/Roms.
func RomsDir() string { return envOr("ROMS_DIR", filepath.Join(BasePath(), "Roms")) }

// BiosDir returns the OnionOS BIOS dir: BIOS_DIR if set, else <BasePath>/BIOS
// (capital BIOS — OnionOS, unlike MinUI's Bios).
func BiosDir() string { return envOr("BIOS_DIR", filepath.Join(BasePath(), "BIOS")) }

// SavesDir returns the OnionOS RetroArch savefile root: SAVES_DIR if set, else
// <BasePath>/Saves/CurrentProfile/saves (with sort_savefiles_enable=true → per-core
// subfolders below it).
func SavesDir() string {
	return envOr("SAVES_DIR", filepath.Join(BasePath(), "Saves", "CurrentProfile", "saves"))
}

// EmulatorFoldersForFSSlug returns the save folders to SCAN for a slug. When the shim
// has pinned the launch core (LODOR_SAVE_SUBDIR set) we scan only that folder; otherwise
// (the daemon's --push-pending) we scan EVERY existing core folder under SavesDir so a
// changed save is found wherever its core wrote it, regardless of which core launched it.
// The slug is intentionally not used to narrow the daemon scan — matching is by ROM
// basename in the caller.
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

// SaveFileName returns the OnionOS/RetroArch on-disk save filename: the ROM basename
// with its extension stripped, plus the real save extension — SaveFileName("Game
// (USA).gba", "srm") == "Game (USA).srm". An empty saveExt yields just the stripped
// basename.
func SaveFileName(romFullFilename, saveExt string) string {
	base := strings.TrimSuffix(romFullFilename, filepath.Ext(romFullFilename))
	ext := strings.TrimPrefix(saveExt, ".")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

// SaveDirectory returns the canonical save directory for a write (pull/restore). The
// shim-pinned LODOR_SAVE_SUBDIR wins (the core that will actually read the save); else
// the system's default-core folder; else "" (no save directory — never blind-write).
func SaveDirectory(slug string) string {
	if sub := os.Getenv(saveSubdirEnv); sub != "" {
		return filepath.Join(SavesDir(), sub)
	}
	if core, ok := onionDefaultCore[slug]; ok {
		return filepath.Join(SavesDir(), core)
	}
	return ""
}

// BIOSFilePaths returns the OnionOS BIOS destination(s) for a file: a single flat
// <BiosDir>/<base>. OnionOS keeps BIOS in one directory (unlike MinUI's per-tag subdirs).
func BIOSFilePaths(fileName, slug string) []string {
	return []string{filepath.Join(BiosDir(), filepath.Base(fileName))}
}

// EmulatorFoldersForFSSlug already covers save-scan folders. PrimaryTag returns the
// OnionOS Roms/ system TAG for a slug (used to build directory_mappings and the mirror
// folder), and ok=false when the slug isn't a known OnionOS system (caller SKIPs it).
func PrimaryTag(fsSlug string) (tag string, ok bool) {
	if t, ok := onionRomTags[fsSlug]; ok {
		return t, true
	}
	return "", false
}

// FsSlugForTag reverses onionRomTags: given an OnionOS Roms/ system TAG, return a RomM
// fs_slug that maps to it. It exists because catalog.go (shared with the !onion build)
// calls platform.FsSlugForTag to resolve a capability-discovered platform folder back to
// its slug. Several slugs alias one OnionOS tag (snes/sfam->SFC, nes/famicom->FC), so we
// return the lexicographically-first match for a STABLE, deterministic result. On real
// OnionOS this path is effectively dead: the mirror writes BARE-tag folders, so catalog's
// tagFromFolderName (which extracts a trailing "(TAG)") yields "" and never calls this.
// Kept deterministic to avoid flaky download/save resolution if a MinUI-style "(TAG)"
// folder is ever present on an onion build.
func FsSlugForTag(tag string) (string, bool) {
	if tag == "" {
		return "", false
	}
	matches := make([]string, 0, 2)
	for slug, t := range onionRomTags {
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

// HasEmuPak reports whether OnionOS can actually LAUNCH this system on THIS card — i.e.
// the Emu/<TAG>/ folder exists (OnionOS ships emulators as Emu/<TAG>/launch.sh). This is
// the OnionOS analog of MinUI's Emus/<TAG>.pak gate: it stops us stubbing a library the
// Mini Plus can't play (e.g. a PSP/NDS folder code with no installed Emu on the MMP).
// Honors BASE_PATH. A card whose Emu/ isn't populated yet (pre-install sandbox) can
// bypass via LODOR_SKIP_EMU_GATE=1.
func HasEmuPak(tag string) bool {
	if tag == "" {
		return false
	}
	if os.Getenv("LODOR_SKIP_EMU_GATE") == "1" {
		return true
	}
	emu := tag
	if f, ok := onionEmuFolder[tag]; ok {
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
// CFW-agnostic — identical to the !onion default in platform.go; duplicated because that
// file is excluded under -tags onion.
func PakDir() string {
	if d := strings.TrimSpace(os.Getenv("LODOR_PAK_DIR")); d != "" {
		return d
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// saveArtifactAnchors returns the filename prefixes (anchored with a trailing ".")
// that identify THIS game's save/state artifacts in a save folder. OnionOS runs stock
// RetroArch, which derives save/state names from the ROM basename with its extension
// STRIPPED ("Game (USA).srm", "Game (USA).state1") — so the anchor is the stem, not
// MinUI's full-filename form (anchoring on the full basename missed every RetroArch
// save during a ✘/✓ marker flip, orphaning it under the old marker name). The
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
// On OnionOS this is the bare system TAG (e.g. "GBA") regardless of mirror mode —
// OnionOS binds a Roms/ subfolder to an emulator purely by that fixed folder code, so
// there is no "<Display> (<TAG>)" form and no per-mode folder split. Mode separation
// from a user's own same-named games is handled instead by LocalBasename's " (RomM)"
// filename disambiguator (the agnostic path below), not by the folder. display is
// unused on OnionOS — the folder must be exactly the tag OnionOS recognises.
func MirrorFolderName(display, tag, mode string) string {
	return tag
}

// ---------------------------------------------------------------------------
// CFW-agnostic helpers (identical to the !onion default in platform.go; duplicated
// here because that file is excluded under -tags onion). Keep in sync at foldback.
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
	if displayName != "" {
		folder = displayName
	}
	return filepath.Join(RomsDir(), folder)
}

// archiveRawExt maps a RomM fs_slug to the raw ROM extension its standalone emulator
// needs when the server stores the game inside a .7z the emulator cannot open. The
// engine extracts the .7z to this extension on download (via the bundled 7zz in the
// launch wrap). NDS/DraStic is the case: DraStic reads raw .nds (and .zip) but NOT .7z.
// On the Mini Plus (SSD202D) NDS is not launchable, so HasEmuPak gates it out and this
// never fires there; kept for parity with the !onion build and any stronger Onion host.
var archiveRawExt = map[string]string{
	"nds": ".nds",
}

// ArchiveExtractTargetForRom reports whether a ROM is stored in a .7z that must be
// extracted to a raw file on download, and the raw extension that file takes. Only .7z
// triggers it -- .zip is left alone for emulators that read it natively. Mirrors the
// !onion definition exactly so cmd/lodor-sync (shared) resolves the same on both builds.
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
// file's own extension. Keeps the stub the OnionOS launcher sees (and the file the
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

// romMDisambiguator is the marker appended to a RomM stub's basename in non-"own"
// mirror modes so a RomM stub's save (and on-disk file) can never collide with a user's
// own same-named game in a different folder that binds the same TAG.
const romMDisambiguator = " (RomM)"

// LocalBasename returns the extension-less on-disk basename a ROM occupies under the
// active mirror mode: rom.CanonicalLocalBasename() in "own" mode, else suffixed with
// romMDisambiguator. Single source of truth shared by LocalRomPath and the catalog
// index keys.
func LocalBasename(cfg *config.Config, rom romm.Rom) string {
	base := rom.CanonicalLocalBasename()
	if base == "" || cfg.ResolvedMirrorMode() == config.MirrorModeOwn {
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
