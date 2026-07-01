//go:build onion

package platform

// HostShowsStateNatively is always false on the OnionOS build: OnionOS uses its stock
// launcher, which cannot dim, so the engine keeps the ✘/✓ filename markers.
func HostShowsStateNatively() bool { return false }
