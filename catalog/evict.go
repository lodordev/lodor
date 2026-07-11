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
		// Stranded multi-disc bytes: an interrupted multi-disc download can leave the
		// .m3u a 0-byte stub while fetched discs sit in the per-game folder — bytes
		// the "stub" refusal would otherwise make unreclaimable forever. When the
		// manifest owns that folder (record-intent-then-act wrote it before any disc
		// landed), sweep it; the refusal token is unchanged (the .m3u IS a stub).
		if !fi.IsDir() && strings.EqualFold(filepath.Ext(romPath), ".m3u") {
			sweepStrandedDiscDir(romPath)
		}
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

// M3UDiscRefs returns the absolute on-card paths of the disc files a real .m3u
// references (the engine's own multi-disc download writes ".<FsNameNoExt>/<disc>"
// lines — dot-hidden folder, lodor#7 UX fix; legacy cards carry non-dot
// "<FsNameNoExt>/<disc>" lines — resolved relative to the m3u's directory, so BOTH
// shapes follow the playlist automatically). Defensive: blank/comment lines,
// absolute lines, and any ".." component are skipped, so a hand-edited m3u can never
// point a caller outside the game's folder. Filesystem-read only, never mutates.
// Shared by evict/uninstall (delete the referenced discs) and the disc-1-first
// completeness checks (--check-rom / --prefetch-discs, lodor#7).
func M3UDiscRefs(m3uPath string) []string {
	dir := filepath.Dir(m3uPath)
	var out []string
	for _, line := range M3UDiscLines(m3uPath) {
		out = append(out, filepath.Join(dir, line))
	}
	return out
}

// M3UDiscLines returns a real .m3u's disc lines RAW (m3u-relative, CRLF-trimmed),
// under the same defensive rules as M3UDiscRefs (blank/comment/absolute/".." lines
// skipped). This relative shape is what the manifest's canonical disc list stores —
// the legacy-migration seed reads the old full-list playlist through this.
func M3UDiscLines(m3uPath string) []string {
	data, err := os.ReadFile(m3uPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if filepath.IsAbs(line) || strings.Contains(line, "..") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// evictDiscFiles deletes a multi-disc game's LOCAL disc bytes. The .m3u is
// local-only now (lodor#7 — it lists just the discs with real bytes), so the
// playlist refs alone no longer cover the whole on-card set: beyond-budget 0-byte
// stubs and interrupted .tmp partials live in the per-game folder unlisted. Three
// legs, most-authoritative last:
//
//  1. the .m3u's own refs (works even on a manifest-less card);
//  2. the manifest's canonical disc list (covers discs delisted from a local-only
//     playlist but still holding bytes — e.g. after a hand-edit);
//  3. the manifest-OWNED per-game folder, removed WHOLE (sweepStrandedDiscDir's
//     gate: downloadMultiDiscCore records the folder before any disc lands —
//     record-intent-then-act), which is what actually catches stubs and partials.
//
// An unowned same-named folder (the user's own files in merge mode, or any
// manifest-less card) is never swept — legs 1-2 remove only referenced/recorded
// files and the folder falls only if empty. Best-effort throughout.
func evictDiscFiles(m3uPath string) {
	dir := filepath.Dir(m3uPath)
	discDirs := map[string]bool{}
	man := platform.LoadManifest()
	seen := map[string]bool{}
	for _, p := range append(M3UDiscRefs(m3uPath), CanonicalDiscRefs(man, m3uPath)...) {
		if seen[p] {
			continue
		}
		seen[p] = true
		_ = os.Remove(p)
		if d := filepath.Dir(p); d != dir {
			discDirs[d] = true
		}
	}
	for d := range discDirs {
		_ = os.Remove(d)
	}
	sweepStrandedDiscDir(m3uPath) // owned-folder sweep: stubs + .tmp partials
}

// sweepStrandedDiscDir reclaims fetched disc bytes stranded behind a 0-byte stub
// .m3u (an interrupted multi-disc download: the mirror stubs the playlist first,
// discs land in the per-game folder, and the full .m3u is only written at the end).
// A stub references nothing, so evictDiscFiles cannot find the bytes — instead the
// per-game folder is derived the way the download derives it (the playlist's
// marker-stripped stem beside it) and removed WHOLE, but ONLY when the mirror-owned
// manifest claims the folder (downloadMultiDiscCore records it before any disc
// lands — record-intent-then-act). An unowned same-named folder — the user's own
// files in merge mode, or any manifest-less card — is refused untouched, the same
// V3 gate the real-evict path applies. Best-effort: RemoveAll mirrors the download
// path's own failure cleanup for exactly this manifest-owned folder.
//
// BOTH layouts are swept: the dot-hidden folder the engine writes now
// (DiscFolderName, lodor#7 UX fix) and the legacy non-dot folder a pre-dot card may
// still carry (migrateLegacyM3U converges those, but an evict can arrive first) —
// each under its own ownership gate, so a user's same-named folder in either shape
// is still refused.
func sweepStrandedDiscDir(m3uPath string) {
	base := platform.StripLeadingMarker(filepath.Base(m3uPath))
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" {
		return
	}
	man := platform.LoadManifest()
	for _, name := range []string{platform.DiscFolderName(stem), stem} {
		discDir := filepath.Join(filepath.Dir(m3uPath), name)
		fi, err := os.Stat(discDir)
		if err != nil || !fi.IsDir() {
			continue
		}
		if !man.OwnsKind(discDir, platform.ManifestFolder) {
			continue
		}
		_ = os.RemoveAll(discDir)
	}
}
