//go:build android || lodorandroid

package platform

// HostShowsStateNatively is hard-TRUE on the Android build — but for the opposite
// reason to LodorOS (whose forked launcher dims stubs itself). Here the engine must
// stay MARKER-LESS because filename markers actively break the host: ES-DE keys its
// per-game metadata (scraped art, play counts, favorites) on the ROM filename, so a
// ✘→✓ reconcile rename would orphan that metadata and force rescans mid-session.
// Marker-less is the LodorOS-proven engine mode: stubs carry canonical server names,
// a download fills bytes IN PLACE (no rename, the frontend never needs a rescan),
// and save filenames stay canonical. The Lodor app itself is the sync-state display
// surface (queue/status screens) — the launcher shows plain names, which is correct.
// --reconcile degrades to a harmless no-op; the app still calls it for ledger parity.
func HostShowsStateNatively() bool { return true }
