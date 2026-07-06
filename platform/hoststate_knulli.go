//go:build knulli

package platform

// HostShowsStateNatively is always false on the Knulli build: Knulli runs stock
// EmulationStation (never forked — the whole point of the port), which cannot dim
// stubs by file size, so the engine keeps the ✘/✓ filename markers. The CLEAN
// display name comes from the per-folder gamelist.xml the engine emits
// (--write-gamelists): the gamelist <name> is the marker-stripped title, so the
// marker shows sync state in the raw file listing while ES renders a clean library.
func HostShowsStateNatively() bool { return false }
