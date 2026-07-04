//go:build muos

// muOS (MustardOS) platform variant — the no-fork port for Allwinner H700 Anbernic
// devices (RG34XX). Built with `-tags muos`. Pinned against muOS 2601 "Jacaranda"
// (RG34XX-H 2601.1) ground truth read from the shipped image, 2026-06-25; re-based onto
// the current engine interface (marker/A1/A2/manifest era) 2026-07-03.
//
// Why muOS differs from MinUI (the !onion && !muos default in platform.go):
//   - ROMs live in /mnt/mmc/ROMS/<System>/ where <System> is the muOS catalogue name
//     (e.g. "Sony PlayStation"), matched to an emulator via the system's assign config
//     (<share>/info/assign/<System>/). NOT MinUI's "<Display> (<TAG>)" folder — so the
//     "tag" on muOS IS the catalogue folder name, and MirrorFolderName returns it bare.
//   - It runs stock RetroArch. Battery saves live at
//     /run/muos/storage/save/file/<CoreDisplayName>/<rom-basename>.<ext>
//     (sort_savefiles_enable=true, by_content=false → per-corename subfolders). The
//     folder is the libretro corename (e.g. "PCSX-ReARMed"), NOT the system or the
//     slug — and a system can be launched by several cores, so the save directory is
//     CORE-DRIVEN, exactly like OnionOS.
//   - The on-disk save filename is the ROM basename WITHOUT its extension + the save
//     extension ("Game (USA).srm") — the RetroArch rule, NOT MinUI's full-name+".sav".
//   - BIOS is a single flat directory (/run/muos/storage/bios).
//
// HOW THE CORE IS KNOWN: the launch override shim knows the exact core for every launch
// and resolves its corename from the card's own libretro .info files
// (<share>/emulator/retroarch/info/<core>.info → `corename`). It exports that as
// LODOR_SAVE_SUBDIR before invoking the engine, so save read/write lands exactly where
// RetroArch will look. For the context-free daemon path (--push-pending, no env) we
// DISCOVER saves by scanning every existing core folder under the save dir and matching
// by ROM basename — immune to muOS reshuffling corenames between releases.
//
// SANDBOX RELOCATION: all card roots are env-overridable (ROMS_DIR / SAVES_DIR /
// BIOS_DIR); additionally, a set BASE_PATH relocates the WHOLE tree to the generic
// <BASE_PATH>/{Roms,Saves,Bios} layout — the same names the engine's shared card-walk
// code (catalog/migrate) joins under sdcardRoot(), so the off-hardware sandbox and the
// tag-free test suite exercise one consistent tree without a device. On hardware no env
// is needed: BASE_PATH is unset and the muOS-true defaults below apply. The pak working
// dir is resolved via PakDir() (LODOR_PAK_DIR), exactly like the other variants.
// CGO-free, stdlib only.
package platform

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/config"
	"lodor/romm"
)

// muosRomFolders maps a RomM filesystem slug to the muOS catalogue / ROM-folder name,
// taken from the RG34XX-H 2601.1 card's own info/assign/<System> directories (the
// authoritative names — they differ from the upstream rommapp/muos-app map, which is
// stale: the card uses "Nintendo NES - Famicom", "Sony PlayStation", "PC DOS"). A slug
// absent here is SKIPPED during mapping generation (no folder invented) — honest, same
// as MinUI skipping an unknown tag. Content packs add systems; extend then. Aliases
// (mastersystem, atarilynx, pcengine, sega32x, dreamcast, fbneo) cover RomM instances
// that slug the same system differently — each maps to an already-verified card name.
var muosRomFolders = map[string]string{
	"arcade":               "Arcade",
	"fbneo":                "Arcade", // RomM alias — same verified card folder
	"atari2600":            "Atari 2600",
	"lynx":                 "Atari Lynx",
	"atarilynx":            "Atari Lynx", // some RomM instances slug Lynx this way
	"cave-story":           "Cave Story",
	"amiga":                "Commodore Amiga",
	"c64":                  "Commodore C64",
	"doom":                 "Doom",
	"ports":                "External - Ports",
	"tg16":                 "NEC PC Engine",
	"pcengine":             "NEC PC Engine", // RomM alias
	"turbografx-cd":        "NEC PC Engine CD",
	"supergrafx":           "NEC PC Engine SuperGrafx",
	"gb":                   "Nintendo Game Boy",
	"gba":                  "Nintendo Game Boy Advance",
	"gbc":                  "Nintendo Game Boy Color",
	"n64":                  "Nintendo N64",
	"nes":                  "Nintendo NES - Famicom",
	"famicom":              "Nintendo NES - Famicom",
	"snes":                 "Nintendo SNES - SFC",
	"sfam":                 "Nintendo SNES - SFC",
	"dos":                  "PC DOS",
	"pico-8":               "PICO-8",
	"neogeoaes":            "SNK Neo Geo",
	"neogeomvs":            "SNK Neo Geo",
	"neo-geo-cd":           "SNK Neo Geo CD",
	"neo-geo-pocket":       "SNK Neo Geo Pocket - Color",
	"neo-geo-pocket-color": "SNK Neo Geo Pocket - Color",
	"scummvm":              "ScummVM",
	"sega32":               "Sega 32X",
	"sega32x":              "Sega 32X", // RomM alias
	"naomi":                "Sega Atomiswave Naomi",
	"dc":                   "Sega Dreamcast",
	"dreamcast":            "Sega Dreamcast", // RomM alias
	"gamegear":             "Sega Game Gear",
	"sms":                  "Sega Master System",
	"mastersystem":         "Sega Master System", // some RomM instances slug SMS this way
	"segacd":               "Sega Mega CD - Sega CD",
	"genesis":              "Sega Mega Drive - Genesis",
	"psx":                  "Sony PlayStation",
	"psp":                  "Sony PlayStation Portable",
}

// muosDefaultCore maps a slug to the libretro corename of the system's DEFAULT core,
// used ONLY as a save-directory fallback when LODOR_SAVE_SUBDIR is unset (i.e. not the
// shim path). Each value is a verified corename (matches the card's save folders). A
// slug absent here yields no fallback save dir (we never blind-write to a guessed
// folder). The shim always sets the env, so this is a safety net, not the primary path.
var muosDefaultCore = map[string]string{
	"gb":           "Gambatte",
	"gbc":          "Gambatte",
	"gba":          "mGBA",
	"snes":         "Snes9x",
	"sfam":         "Snes9x",
	"nes":          "FCEUmm",
	"famicom":      "FCEUmm",
	"genesis":      "Genesis Plus GX",
	"gamegear":     "Genesis Plus GX",
	"sms":          "Genesis Plus GX",
	"mastersystem": "Genesis Plus GX",
	"atarilynx":    "Handy",
	"lynx":         "Handy",
	"psx":          "PCSX-ReARMed",
	"n64":          "Mupen64Plus-Next",
	"psp":          "PPSSPP",
}

// saveSubdirEnv is set by the launch override shim to the corename RetroArch will use
// for THIS launch (resolved from the card's libretro .info). When present it pins the
// save read/write to the exact folder; when absent the daemon falls back to glob-all
// discovery + the muosDefaultCore table.
const saveSubdirEnv = "LODOR_SAVE_SUBDIR"

// envOr returns the value of environment variable key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BasePath returns the muOS card root: BASE_PATH if set, else /mnt/mmc. Setting
// BASE_PATH relocates the whole engine tree to the generic <BASE_PATH>/{Roms,Saves,
// Bios} layout (sandbox/tests); on hardware it stays unset and Saves/BIOS live under
// /run/muos/storage, NOT under BasePath — they have dedicated resolvers below.
func BasePath() string { return envOr("BASE_PATH", "/mnt/mmc") }

// RomsDir returns the muOS ROMs root: ROMS_DIR if set (the launch scripts pin it to the
// live rom mount — /mnt/mmc/ROMS or /mnt/sdcard/ROMS); else the relocated generic
// <BASE_PATH>/Roms when BASE_PATH is set; else the muOS default /mnt/mmc/ROMS.
func RomsDir() string {
	if v := os.Getenv("ROMS_DIR"); v != "" {
		return v
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return filepath.Join(bp, "Roms")
	}
	return "/mnt/mmc/ROMS"
}

// BiosDir returns the muOS BIOS dir: BIOS_DIR if set; else <BASE_PATH>/Bios when
// BASE_PATH is set (sandbox); else the muOS default /run/muos/storage/bios.
func BiosDir() string {
	if v := os.Getenv("BIOS_DIR"); v != "" {
		return v
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return filepath.Join(bp, "Bios")
	}
	return "/run/muos/storage/bios"
}

// SavesDir returns the muOS RetroArch savefile root: SAVES_DIR if set; else
// <BASE_PATH>/Saves when BASE_PATH is set (sandbox); else the muOS default
// /run/muos/storage/save/file (savefile_directory from the card's
// retroarch.default.cfg, with sort_savefiles_enable=true → per-corename subfolders).
func SavesDir() string {
	if v := os.Getenv("SAVES_DIR"); v != "" {
		return v
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return filepath.Join(bp, "Saves")
	}
	return "/run/muos/storage/save/file"
}

// EmulatorFoldersForFSSlug returns the save folders to SCAN for a slug. When the shim
// has pinned the launch core (LODOR_SAVE_SUBDIR set) we scan only that folder; otherwise
// (the daemon's --push-pending) we scan EVERY existing core folder under SavesDir so a
// changed save is found wherever its core wrote it, regardless of which core launched it
// or how muOS has renamed cores between releases. The slug is intentionally not used to
// narrow the daemon scan — matching is by ROM basename in the caller.
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

// SaveFileName returns the muOS/RetroArch on-disk save filename: the ROM basename with
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
// shim-pinned LODOR_SAVE_SUBDIR wins (the core that will actually read the save); else
// the system's default-core folder; else "" (no save directory — never blind-write).
func SaveDirectory(slug string) string {
	if sub := os.Getenv(saveSubdirEnv); sub != "" {
		return filepath.Join(SavesDir(), sub)
	}
	if core, ok := muosDefaultCore[slug]; ok {
		return filepath.Join(SavesDir(), core)
	}
	return ""
}

// BIOSFilePaths returns the muOS BIOS destination(s) for a file: a single flat
// <BiosDir>/<base>. muOS keeps BIOS in one directory (unlike MinUI's per-tag subdirs).
func BIOSFilePaths(fileName, slug string) []string {
	return []string{filepath.Join(BiosDir(), filepath.Base(fileName))}
}

// PrimaryTag returns the muOS ROMs/ system folder — the catalogue name — for a RomM
// fs_slug (used to build directory_mappings and the mirror folder), and ok=false when
// the slug isn't a known muOS default system (caller SKIPs it, inventing no folder).
// On muOS the "tag" IS the catalogue folder name ("Sony PlayStation"): muOS binds a
// ROMS/ subfolder to an emulator by that name via info/assign, so stubs land in a
// folder muOS will recognise.
func PrimaryTag(fsSlug string) (tag string, ok bool) {
	if f, ok := muosRomFolders[fsSlug]; ok {
		return f, true
	}
	return "", false
}

// FsSlugForTag reverses muosRomFolders: given a muOS catalogue folder name, return a
// RomM fs_slug that maps to it. Several slugs alias one catalogue name (nes/famicom,
// snes/sfam, sms/mastersystem, lynx/atarilynx …), so we return the lexicographically
// first match for a STABLE, deterministic result (the same rule as the onion variant).
func FsSlugForTag(tag string) (string, bool) {
	if tag == "" {
		return "", false
	}
	matches := make([]string, 0, 2)
	for slug, t := range muosRomFolders {
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

// muosAssignDir returns the root of muOS's system→launcher assign config:
// MUOS_ASSIGN_DIR if set (sandbox), else <share>/info/assign with the share dir from
// MUOS_SHARE_DIR (the card's own script/var/func.sh exports it; /opt/muos/share on
// 2601 "Jacaranda").
func muosAssignDir() string {
	if v := os.Getenv("MUOS_ASSIGN_DIR"); v != "" {
		return v
	}
	return filepath.Join(envOr("MUOS_SHARE_DIR", "/opt/muos/share"), "info", "assign")
}

// HasEmuPak reports whether muOS can actually LAUNCH this system on THIS card — i.e.
// the assign config directory for the catalogue name exists (<assign>/<System>/ holds
// global.ini + per-launcher .ini; muOS resolves every launch through it). This is the
// muOS analog of MinUI's Emus/<TAG>.pak gate: it stops us stubbing a library the device
// can't play. A sandbox with no assign tree can bypass via LODOR_SKIP_EMU_GATE=1.
func HasEmuPak(tag string) bool {
	if tag == "" {
		return false
	}
	if os.Getenv("LODOR_SKIP_EMU_GATE") == "1" {
		return true
	}
	if st, err := os.Stat(filepath.Join(muosAssignDir(), tag)); err == nil && st.IsDir() {
		return true
	}
	return false
}

// PakDir resolves the app working directory (where config.json, catalog-index.json,
// mirror-manifest.json and the pending queue live): LODOR_PAK_DIR if exported by the
// app scripts, else the process CWD (the scripts cd into the app/data dir before
// invoking the engine), last resort ".". CFW-agnostic — identical to the other
// variants; duplicated because platform.go is excluded under -tags muos.
func PakDir() string {
	if d := strings.TrimSpace(os.Getenv("LODOR_PAK_DIR")); d != "" {
		return d
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// MirrorFolderName builds the ROMS/ folder a platform's RomM games are mirrored into.
// On muOS this is the bare catalogue name (e.g. "Sony PlayStation") regardless of
// mirror mode — muOS binds a ROMS/ subfolder to an emulator purely by that name, so
// there is no "<Display> (<TAG>)" form and no per-mode folder split. Mode separation
// from a user's own same-named games is handled instead by LocalBasename's " (RomM)"
// filename disambiguator, not by the folder. display is unused on muOS — the folder
// must be exactly the catalogue name muOS recognises.
func MirrorFolderName(display, tag, mode string) string {
	return tag
}

// saveArtifactAnchors returns the filename prefixes (anchored with a trailing ".")
// that identify THIS game's save/state artifacts in a save folder. muOS runs stock
// RetroArch, which derives save/state names from the ROM basename with its extension
// STRIPPED ("Game (USA).srm", "Game (USA).state1") — so the anchor is the stem, not
// MinUI's full-filename form. The full-filename anchor is kept too (harmless when no
// such file exists) so an artifact staged under the MinUI rule still migrates.
func saveArtifactAnchors(romBase string) []string {
	stem := strings.TrimSuffix(romBase, filepath.Ext(romBase))
	if stem == "" || stem == romBase {
		return []string{romBase}
	}
	return []string{stem, romBase}
}

// ---------------------------------------------------------------------------
// CFW-agnostic helpers (identical to the default in platform.go; duplicated here
// because that file is excluded under -tags muos). Keep in sync at foldback.
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
	// No mapping: prefer the authoritative catalogue name so a stub lands in a folder
	// muOS will recognise; else the display name; else the fs_slug.
	if f, ok := muosRomFolders[fsSlug]; ok {
		folder = f
	} else if displayName != "" {
		folder = displayName
	}
	return filepath.Join(RomsDir(), folder)
}

// archiveRawExt maps a RomM fs_slug to the raw ROM extension its standalone emulator
// needs when the server stores the game inside a .7z the emulator cannot open. The
// engine extracts the .7z to this extension on download (via the bundled 7zz in the
// launch wrap). NDS/DraStic is the case: DraStic reads raw .nds (and .zip) but NOT .7z.
// Kept for parity with the default build and any content-pack muOS host that adds NDS.
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
// file's own extension. Keeps the stub the muOS launcher sees (and the file the
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
// being byte-equal to the user's own filename (muOS keeps markers: stock launcher,
// HostShowsStateNatively is hard-false).
const romMDisambiguator = " (RomM)"

// LocalBasename returns the extension-less on-disk basename a ROM occupies under the
// active mirror mode. In "own" AND "merge" modes it is rom.CanonicalLocalBasename() —
// byte-identical to the server's name (merge-canonical is what makes dedup-by-index-
// adoption free; see the default variant's comment). In "separate" mode it appends
// romMDisambiguator. Single source of truth shared by LocalRomPath and the catalog
// index keys. SAME semantics as the current default (!onion && !muos) build.
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

// CanonicalMirrorFolder returns the ONLY Roms/ folder name muOS will recognise for a
// slug — the catalogue name info/assign binds to an emulator. muOS resolves a system
// PURELY by this folder name, so a directory_mappings relative_path that differs (an
// onion bare-TAG "GG", a MinUI "Sega Game Gear (GG)" left in a config.json carried
// across CFWs, or a hand-edit) is unlaunchable and must be HEALED. catalog.ensure-
// DirectoryMappings calls this to snap such a mapping back to the catalogue name.
// Returns "" for a slug muOS does not map (content-pack system) so the caller leaves it.
func CanonicalMirrorFolder(fsSlug string) string {
	if f, ok := muosRomFolders[fsSlug]; ok {
		return f
	}
	return ""
}
