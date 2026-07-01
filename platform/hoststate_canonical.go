package platform

import (
	"os"
	"path/filepath"

	"lodor/config"
	"lodor/romm"
)

// ReconcileCanonicalPresence is the no-marker counterpart of ReconcileMarkedPresence,
// used on hosts whose launcher shows sync state natively (HostShowsStateNatively). The
// ROM keeps its CANONICAL RomM name with no ✘/✓ prefix. It ensures the game has exactly
// ONE on-disk presence at the unmarked path, migrating from any leftover MARKED variant
// (a card mirrored by an older marker-stamping build) so its saves and cover follow the
// rename — that migration is what de-markers an upgraded card on the next library refresh.
// Returns the final on-disk path and whether a brand-new 0-byte stub was created.
func ReconcileCanonicalPresence(cfg *config.Config, rom romm.Rom, unmarked string) (final string, didCreate bool) {
	if unmarked == "" {
		return "", false
	}
	dir := filepath.Dir(unmarked)
	canonBase := StripLeadingMarker(filepath.Base(unmarked))
	canonical := filepath.Join(dir, canonBase)

	// Existing presence priority: the canonical name first (already clean — keep it),
	// then any marked variant (on-device, cloud, legacy) to migrate down to canonical.
	candidates := []string{
		canonical,
		filepath.Join(dir, MarkerOnDevice+canonBase),
		filepath.Join(dir, MarkerCloud+canonBase),
	}
	for _, m := range legacyMarkers {
		candidates = append(candidates, filepath.Join(dir, m+canonBase))
	}
	var src string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			src = c
			break
		}
	}

	if src == "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", false
		}
		f, err := os.Create(canonical)
		if err != nil {
			return "", false
		}
		_ = f.Close()
		return canonical, true
	}
	if src == canonical {
		return canonical, false
	}
	// A marked variant exists -> strip the marker, carrying saves + cover in lockstep.
	migrateMarkedGame(rom, src, canonical)
	return canonical, false
}
