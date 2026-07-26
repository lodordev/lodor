//go:build spruce

package platform

// HostShowsStateNatively is always false on the spruceOS build: spruce uses its stock
// PyUI launcher (never forked — the whole point of the no-fork port), which cannot dim,
// so the engine keeps the ✘/✓ filename markers.
func HostShowsStateNatively() bool { return false }
