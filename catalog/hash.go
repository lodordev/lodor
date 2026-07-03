package catalog

import (
	"path/filepath"
	"strings"

	"lodor/config"
	"lodor/platform"
)

// RomHashForPath — the playtime session-key resolver (task #146). Reverses a
// local ROM path to its rom_id (the same resolution ResolveRomID does) and
// returns the server-recorded content md5 the mirror stored in the index's
// by_id_hash. ok=false when the path doesn't resolve, the index predates #146
// (no by_id_hash), or RomM doesn't know the hash — the caller then keys the
// session by TAG/basename instead. Offline and sub-second: local index only.
func RomHashForPath(cfg *config.Config, romPath string) (string, bool) {
	if cfg == nil || romPath == "" {
		return "", false
	}
	id, ok := ResolveRomID(cfg, romPath)
	if !ok || id == 0 {
		return "", false
	}
	slug, sok := slugForRomPath(cfg, romPath)
	if !sok {
		return "", false
	}
	idx, err := loadIndex(IndexPath(cfg))
	if err != nil {
		return "", false
	}
	pi, pok := idx.Platforms[slug]
	if !pok || pi.ByIDHash == nil {
		return "", false
	}
	h := pi.ByIDHash[id]
	if h == "" {
		return "", false
	}
	return strings.ToLower(h), true
}

// CanonicalRomBasename returns the device-independent basename for a local ROM
// path: the on-disk name with the leading cloud/on-device state marker and the
// " (RomM)" coexist disambiguator stripped — the SAME normalization uploads use
// (sync/canonical.go), so every device derives the same playtime fallback key
// and rom_basename column for the same game.
func CanonicalRomBasename(romPath string) string {
	base := platform.StripLeadingMarker(filepath.Base(romPath))
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if platform.HasRomMTag(stem) {
		stem = platform.StripRomMTag(stem)
	}
	return stem + ext
}
