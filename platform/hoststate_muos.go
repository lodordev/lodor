//go:build muos

package platform

// HostShowsStateNatively is always false on the muOS build: muOS uses its stock
// launcher (never forked — the whole point of the port), which cannot dim, so the
// engine keeps the ✘/✓ filename markers.
func HostShowsStateNatively() bool { return false }
