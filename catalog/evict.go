// Evict — "Delete from card" (task #125, the Game Manager's delete action).
//
// The mirror image of the download: a game's real bytes are removed from the card
// and its 0-byte cloud stub is re-created in place, so it stays in the browsable
// library and re-downloads on the next launch (LodorOS Y-menu semantics). SAVES
// ARE NEVER DELETED: the ✓→✘ marker rename carries this game's saves and cover in
// lockstep via the same ReconcileMarkedPresence the mirror/reconcile paths use, so
// progress survives the evict and rides the next download. Offline by design —
// filesystem + local index only, no host, no network, no device_id.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// EvictToStub removes ONE downloaded ROM's bytes and leaves its 0-byte stub under
// the correct state name. Returns (evicted, reason): reason is a short machine
// token for the RESULT line when evicted=false —
//
//	"missing"           — no file at romPath
//	"stub"              — already a 0-byte stub (nothing to delete)
//	"resolve"           — romPath is not under a managed platform folder; we
//	                      refuse to touch files the library doesn't own
//	"not-lodor-managed" — the mirror-owned manifest does not claim this file as a
//	                      Lodor download (C1 design audit V3: in merge mode an
//	                      ADOPTED user file resolves — that's the feature — so
//	                      resolution alone must never authorize a truncate; the
//	                      user's own ROM is refused untouched). Also the honest
//	                      answer on a manifest-less/corrupt card: ownership is
//	                      unknowable, so nothing is destroyed — the next library
//	                      Refresh re-records real Lodor downloads and evict works
//	                      again.
//	"truncate"          — the filesystem write failed (card error)
//
// Multi-disc: a real .m3u's referenced disc files ARE the bytes — they are
// deleted too (and their per-game folder removed once empty), then the .m3u
// itself becomes the 0-byte stub, exactly the shape the mirror writes.
func EvictToStub(cfg *config.Config, romPath string) (evicted bool, reason string) {
	if cfg == nil || romPath == "" {
		return false, "missing"
	}
	fi, err := os.Stat(romPath)
	if err != nil {
		return false, "missing"
	}
	if fi.IsDir() || fi.Size() == 0 {
		return false, "stub"
	}
	slug, ok := slugForRomPath(cfg, romPath)
	if !ok {
		return false, "resolve"
	}

	// V3 gate: only a manifest-owned DOWNLOAD may be truncated. Real files are
	// never triple-gate re-claimable (that gate is for 0-byte stubs only), so an
	// unmanifested real file — the user's own ROM in merge mode, or anything on a
	// card whose manifest is lost — is refused byte-identical.
	man := platform.LoadManifest()
	if !man.OwnsKind(romPath, platform.ManifestDownload) {
		return false, "not-lodor-managed"
	}

	if strings.EqualFold(filepath.Ext(romPath), ".m3u") {
		evictDiscFiles(romPath)
	}

	if terr := os.Truncate(romPath, 0); terr != nil {
		return false, "truncate"
	}

	// Flip the on-disk name back to the cloud state, carrying saves + cover with
	// the rename (never orphaning them). LodorOS keeps canonical names (its forked
	// launcher dims by file size), so it reconciles to the unmarked name instead.
	dir := filepath.Dir(romPath)
	canonBase := platform.StripLeadingMarker(filepath.Base(romPath))
	unmarked := filepath.Join(dir, canonBase)
	rom := romm.Rom{PlatformFsSlug: slug} // only the slug is read (save-folder lookup)
	var final string
	if platform.HostShowsStateNatively() {
		final, _ = platform.ReconcileCanonicalPresence(cfg, rom, unmarked)
	} else {
		final, _ = platform.ReconcileMarkedPresence(cfg, rom, unmarked)
	}
	if final != "" && filepath.Base(final) != filepath.Base(romPath) {
		updateIndexByID(cfg, slug, final) // best-effort; next Refresh rebuilds anyway
	}
	// Manifest: the download is now a stub (possibly under the flipped ✘ name).
	if final != "" {
		man.RenamePath(romPath, final)
		man.Record(final, platform.ManifestStub, 0)
	} else {
		man.Record(romPath, platform.ManifestStub, 0)
	}
	if merr := man.Save(); merr != nil {
		fmt.Fprintf(os.Stderr, "MANIFEST save failed after evict: %v\n", merr)
	}
	return true, ""
}

// evictDiscFiles deletes the disc files a real .m3u references (the engine's own
// multi-disc download writes "<FsNameNoExt>/<disc>" lines resolved relative to
// the m3u's directory). Defensive: absolute lines or any ".." component are
// skipped so a hand-edited m3u can never delete outside the game's folder. The
// per-game disc folder is removed only when the deletions left it empty
// (os.Remove on a non-empty dir fails, silently). Best-effort throughout.
func evictDiscFiles(m3uPath string) {
	data, err := os.ReadFile(m3uPath)
	if err != nil {
		return
	}
	dir := filepath.Dir(m3uPath)
	discDirs := map[string]bool{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if filepath.IsAbs(line) || strings.Contains(line, "..") {
			continue
		}
		p := filepath.Join(dir, line)
		_ = os.Remove(p)
		if d := filepath.Dir(p); d != dir {
			discDirs[d] = true
		}
	}
	for d := range discDirs {
		_ = os.Remove(d)
	}
}
