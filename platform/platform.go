// Package platform re-expresses the miyoomini/MinUI save-directory data (BLUEPRINT
// §6) as our own and provides the path helpers the engine needs: where ROMs, BIOS,
// and saves live on the card, and how a RomM ROM maps to a concrete on-disk path.
//
// CFW = MINUI. CGO-free, stdlib only. No embedded JSON: the slug->emulator-folder
// map is a plain Go literal, owing nothing to grout's code.
package platform

import (
	"os"
	"path/filepath"
	"strings"

	"lodor/config"
	"lodor/romm"
)

// emulatorFolders maps a RomM filesystem slug to its MinUI emulator/save folder
// name(s). The first entry is the canonical save directory; discovery scans all of
// them. A slug mapped to an empty slice has no save directory (BLUEPRINT §6).
var emulatorFolders = map[string][]string{
	"3do":                        {},
	"3ds":                        {"3DS"},
	"acpc":                       {"CPC"},
	"amiga":                      {"PUAE"},
	"arcade":                     {"FBN"},
	"arduboy":                    {},
	"atari-st":                   {},
	"atari2600":                  {"A2600"},
	"atari5200":                  {"A5200"},
	"atari7800":                  {"A7800"},
	"c128":                       {"C128"},
	"c64":                        {"C64"},
	"cave-story":                 {},
	"cbm-ii":                     {},
	"chailove":                   {},
	"chip-8":                     {},
	"colecovision":               {"COLECO"},
	"cpet":                       {"PET"},
	"dc":                         {"DC"},
	"doom":                       {"PRBOOM"},
	"dos":                        {},
	"fairchild-channel-f":        {},
	"famicom":                    {"FC"},
	"fds":                        {"FDS"},
	"g-and-w":                    {},
	"galaksija":                  {},
	"gamegear":                   {"GG"},
	"gb":                         {"GB"},
	"gba":                        {"GBA", "MGBA"},
	"gbc":                        {"GBC"},
	"genesis":                    {"MD"},
	"intellivision":              {},
	"j2me":                       {},
	"jaguar":                     {"JAGUAR"},
	"karaoke":                    {},
	"lowres":                     {},
	"lua":                        {},
	"lynx":                       {"LYNX"},
	"media-player":               {},
	"mega-duck-slash-cougar-boy": {},
	"msx":                        {"MSX"},
	"n64":                        {"N64"},
	"naomi":                      {},
	"nds":                        {"NDS"},
	"neo-geo-cd":                 {},
	"neo-geo-pocket":             {"NGP"},
	"neo-geo-pocket-color":       {"NGPC"},
	"neogeoaes":                  {},
	"neogeomvs":                  {},
	"nes":                        {"FC"},
	"odyssey":                    {},
	"onscripter":                 {},
	"openbor":                    {},
	"pc-8000":                    {},
	"pc-9800-series":             {},
	"pc-fx":                      {},
	"philips-cd-i":               {},
	"pico":                       {},
	"pico-8":                     {"P8"},
	"pokemon-mini":               {"PKM"},
	"ports":                      {},
	"ps2":                        {"PS2"},
	"psp":                        {"PSP"},
	"psx":                        {"PS"},
	"quake":                      {},
	"rpg-maker":                  {},
	"saturn":                     {"SATURN"},
	"scummvm":                    {},
	"sega32":                     {"32X"},
	"segacd":                     {"SEGACD"},
	"sfam":                       {"SFC"},
	"sg1000":                     {"SG1000"},
	"sharp-x68000":               {},
	"sms":                        {"SMS"},
	"snes":                       {"SFC"},
	"supergrafx":                 {},
	"supervision":                {},
	"tg16":                       {"PCE"},
	"ti-83":                      {},
	"tic-80":                     {},
	"turbografx-cd":              {},
	"uzebox":                     {},
	"vectrex":                    {},
	"vemulator":                  {},
	"vic-20":                     {"VIC"},
	"vircon-32":                  {},
	"virtualboy":                 {"VB"},
	"wasm-4":                     {},
	"wolfenstein-3d":             {},
	"wonderswan":                 {"WS"},
	"wonderswan-color":           {"WSC"},
	"x1":                         {},
	"zx81":                       {},
	"zxs":                        {},
	// --- live-slug additions (2026-06-25): RomM fs_slugs that --mirror-catalog
	// SKIPPED for lack of a tag but the Mini Flip (SSD202D) can run. Tags match the
	// existing in-repo twin so saves + the eventual Emus/<TAG>.pak stay consistent.
	"atarilynx":       {"LYNX"},
	"fbneo":           {"FBN"},
	"mastersystem":    {"SMS"},
	"megaduck":        {"MEGADUCK"},
	"pcengine":        {"PCE"},
	"pokemini":        {"PKM"},
	"sega32x":         {"32X"},
	"wonderswancolor": {"WSC"},
	// --- console-coverage additions (2026-06-27): RomM fs_slugs a strong device
	// (e.g. tg5040 / Trimui Smart Pro) can run via an installed Emus/<TAG>.pak but
	// the engine previously had NO tag for, so --mirror-catalog skipped them even
	// with the pak present (the block-(a) gap). The HasEmuPak gate still decides,
	// per device, whether to actually stub; these just make the console mappable.
	"dreamcast":   {"DC"},     // RomM slug for Sega Dreamcast (flycast); engine had only "dc"
	"atarijaguar": {"JAGUAR"}, // RomM slug for Atari Jaguar (virtualjaguar)
}

// BasePath returns the SD-card root: BASE_PATH if set, otherwise the first of
// /mnt/SDCARD, /mnt/sdcard, /mnt/mmc that exists, defaulting to /mnt/SDCARD.
func BasePath() string {
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		return bp
	}
	for _, candidate := range []string{"/mnt/SDCARD", "/mnt/sdcard", "/mnt/mmc"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "/mnt/SDCARD"
}

// PakDir returns the absolute working directory of the host's Lodor pak — the directory
// the pak's launch scripts cd into before invoking the engine, and where pak-local state
// (pending-saves.txt, catalog-index.json, the flashback staging/cache) lives.
//
// The engine is host-agnostic and MUST NOT know the host's pak name: LodorOS names the
// pak "Lodor.pak", NextUI/my355 ship it as a Tool pak with whatever name the host gives
// it. So the path is supplied at runtime via LODOR_PAK_DIR (an absolute path, exported by
// the pak's launch.sh and bin/* scripts). Fallback: the current working directory, which
// those same scripts already cd into before calling lodor-sync. Last resort: ".".
func PakDir() string {
	if d := strings.TrimSpace(os.Getenv("LODOR_PAK_DIR")); d != "" {
		return d
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// RomsDir returns <BasePath>/Roms.
func RomsDir() string { return filepath.Join(BasePath(), "Roms") }

// BiosDir returns <BasePath>/Bios.
func BiosDir() string { return filepath.Join(BasePath(), "Bios") }

// SavesDir returns <BasePath>/Saves.
func SavesDir() string { return filepath.Join(BasePath(), "Saves") }

// EmulatorFoldersForFSSlug returns the MinUI emulator/save folder names for a RomM
// filesystem slug. An unknown slug or one with no save directory returns nil/empty.
func EmulatorFoldersForFSSlug(slug string) []string {
	return emulatorFolders[slug]
}

// SaveFileName returns the MinUI/minarch on-disk save filename for a ROM: the full
// ROM filename (extension included) suffixed with ".sav" — e.g. SaveFileName(
// "Game (USA).gba", "srm") == "Game (USA).gba.sav". The saveExt argument is part of
// the cross-CFW signature and is ignored on MinUI, which always uses ".sav".
func SaveFileName(romFullFilename, saveExt string) string {
	return romFullFilename + ".sav"
}

// SaveDirectory returns the canonical save directory for a slug:
// <SavesDir>/<firstEmulatorFolder>. Slugs with no save directory return "".
func SaveDirectory(slug string) string {
	folders := EmulatorFoldersForFSSlug(slug)
	if len(folders) == 0 {
		return ""
	}
	return filepath.Join(SavesDir(), folders[0])
}

// BIOSFilePaths returns every candidate BIOS path for fileName on a platform: one
// per emulator tag (<BiosDir>/<TAG>/<fileName>) when the slug has tags, otherwise a
// single <BiosDir>/<fileName>.
func BIOSFilePaths(fileName, slug string) []string {
	tags := EmulatorFoldersForFSSlug(slug)
	if len(tags) > 0 {
		paths := make([]string, 0, len(tags))
		base := filepath.Base(fileName)
		for _, tag := range tags {
			paths = append(paths, filepath.Join(BiosDir(), tag, base))
		}
		return paths
	}
	return []string{filepath.Join(BiosDir(), fileName)}
}

// platformRomDirectory returns the directory under RomsDir where a ROM with the
// given fs_slug lives. It mirrors grout's resolver: the configured
// directory_mappings[fs_slug].relative_path when set, otherwise the fs_slug folder
// name itself (grout's GetPlatformRomDirectory keeps relativePath non-empty, so the
// RomMFSSlugToCFW fallback is the literal slug). When no mapping exists at all we
// fall back to the ROM's platform display name if available, else the fs_slug.
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
	// No mapping: prefer the platform display name (the MinUI "Name (TAG)" folder)
	// when present, otherwise the fs_slug.
	if displayName != "" {
		folder = displayName
	}
	return filepath.Join(RomsDir(), folder)
}

// romMDisambiguator is the marker appended to a RomM stub's basename in non-"own"
// mirror modes. NextUI/MinUI key a save file off the ROM's on-disk basename
// (Saves/<TAG>/<basename>.sav), so two same-named games in different folders that bind
// the same TAG would share — and corrupt — one save. Appending this marker gives the
// RomM copy its own save namespace. NextUI's getDisplayName strips trailing "(...)"
// groups, so the on-screen game name is unchanged.
const romMDisambiguator = " (RomM)"

// LocalBasename returns the extension-less on-disk basename a ROM occupies under the
// active mirror mode. In "own" mode it is rom.CanonicalLocalBasename() — byte-identical
// to the server's name. In "separate"/"merge" modes it appends romMDisambiguator so a
// RomM stub's save (and on-disk file) can never collide with a user's own same-named
// game in a different folder that binds the same TAG. This is the single source of
// truth shared by LocalRomPath and the catalog index keys, so a stub written here
// resolves back to its rom_id by the same name.
func LocalBasename(cfg *config.Config, rom romm.Rom) string {
	base := rom.CanonicalLocalBasename()
	if base == "" || cfg.ResolvedMirrorMode() == config.MirrorModeOwn {
		return base
	}
	return base + romMDisambiguator
}

// LocalRomPath returns the absolute on-disk path a ROM occupies under RomsDir:
// <RomsDir>/<mapped folder>/<basename><ext> for single-file ROMs, or
// <RomsDir>/<mapped folder>/<basename>.m3u for multi-file ROMs, where <basename> is the
// mode-aware LocalBasename (disambiguated in non-"own" modes). Byte-identical to grout's
// Rom.GetLocalPath (BLUEPRINT §4) in "own" mode. Returns "" when the ROM has no platform
// slug or no resolvable file.
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
		return filepath.Join(romDir, base+filepath.Ext(rom.Files[0].FileName))
	}
	return ""
}

// MultiDiscDir returns the per-game subfolder a multi-file ROM's discs are written
// into: <RomsDir>/<mapped folder>/<FsNameNoExt>/. The .m3u (from LocalRomPath) sits
// one level up, beside this folder, and references each disc as "<FsNameNoExt>/<disc
// filename>" — relative to the .m3u's own directory, which is exactly how the MinUI
// launcher's getFirstDisc and the emulator's m3u loader resolve disc paths. This
// mirrors RomM's own folder-per-game layout and keeps the system folder to one .m3u +
// one subfolder per multi-disc game. Returns "" when the ROM has no platform slug.
func MultiDiscDir(cfg *config.Config, rom romm.Rom) string {
	if rom.PlatformFsSlug == "" {
		return ""
	}
	romDir := platformRomDirectory(cfg, rom.PlatformFsSlug, rom.PlatformDisplayName)
	return filepath.Join(romDir, rom.FsNameNoExt)
}

// PrimaryTag returns the canonical MinUI emulator tag for a RomM filesystem slug —
// the first (canonical) entry of its emulatorFolders list, used to build the
// "<Display Name> (<TAG>)" ROM-folder name when auto-generating directory_mappings.
// Returns "" and ok=false for a slug with no known save/emulator folder (the caller
// must then SKIP it rather than invent a folder).
func PrimaryTag(fsSlug string) (tag string, ok bool) {
	folders := emulatorFolders[fsSlug]
	if len(folders) == 0 {
		return "", false
	}
	return folders[0], true
}

// HasEmuPak reports whether an emulator pak for the given MinUI tag is actually
// INSTALLED on this device (checked at the builtin then the user pak location). This is
// the difference between a platform the engine knows a TAG for and one the device can
// actually LAUNCH: a Mini Flip has an "NDS"/"3DS"/"PSP" tag but no matching .pak, so
// those games must not be mapped or stubbed (you'd download what you can't play). It
// adapts per device — a beefier MinUI handheld that ships an NDS.pak gets DS mapped.
// Honors SDCARD_PATH/PLATFORM (defaults /mnt/SDCARD, miyoomini), like the rest of the engine.
func HasEmuPak(tag string) bool {
	if tag == "" {
		return false
	}
	sd := os.Getenv("SDCARD_PATH")
	if sd == "" {
		sd = "/mnt/SDCARD"
	}
	plat := os.Getenv("PLATFORM")
	if plat == "" {
		plat = "miyoomini"
	}
	for _, p := range []string{
		filepath.Join(sd, ".system", plat, "paks", "Emus", tag+".pak"),
		filepath.Join(sd, "Emus", plat, tag+".pak"),
	} {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}
