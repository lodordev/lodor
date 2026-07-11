// Multi-disc per-game folder naming (lodor#7, hardware-confirmed UX bug 2026-07-11).
//
// The engine writes a multi-disc game's discs into a per-game subfolder beside the
// .m3u. When that folder is a plain (non-dot) name, MinUI-family launchers list it
// as a SECOND, browsable entry next to the launchable .m3u — and its disc files show
// with unreadable truncated names. MinUI's hide() (workspace/all/common/utils.c)
// hides any entry whose name starts with '.', so dot-prefixing the folder makes the
// game list show exactly ONE entry per game: the .m3u. Engine-side, overlay-
// shippable, no launcher rebuild.
//
// This file carries NO build tag on purpose (like pathsafe.go): every CFW variant's
// MultiDiscDir builds its folder name through DiscFolderName, so the on-card layout
// and the .m3u's relative lines can never drift apart between lanes.
package platform

// DiscFolderName returns the on-card folder name for a multi-disc game's per-game
// disc directory: the m3u stem DOT-PREFIXED (".Final Fantasy VII (USA)"). MinUI and
// NextUI hide dot entries in the game list; muOS/Knulli (EmulationStation skips
// hidden paths) and Android don't browse the folder either — their launch paths
// resolve discs via the .m3u's relative lines, written in lockstep with this name —
// so the dot folder is harmless-to-hidden there and keeps the card layout uniform
// across every lane.
//
// A stem ALREADY starting with '.' (".hack//…") is returned unchanged: it is
// already hidden, and double-dotting would form a ".." prefix that the defensive
// m3u/manifest line filters (M3UDiscLines/CanonicalDiscRefs skip any line
// containing "..") would rightly refuse to resolve.
func DiscFolderName(stem string) string {
	if stem == "" || stem[0] == '.' {
		return stem
	}
	return "." + stem
}
