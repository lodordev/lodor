package platform

import "lodor/romm"

// Let the RomM client fall back to a platform's canonical slug when its library
// folder name (fs_slug) is one the engine does not know.
func init() {
	romm.KnownFsSlug = func(s string) bool { _, ok := PrimaryTag(s); return ok }
}
