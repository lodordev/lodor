package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/cover"
	"lodor/fsutil"
	"lodor/platform"
)

// --place-frontend-media (box art + clean titles in host-frontend grids).
//
// ES-DE and Cocoon BOTH consume the same ES-DE data layout: covers resolve by matching
// the ROM's on-disk FILENAME to `downloaded_media/<system>/covers/<basename>.png` (they
// ignore gamelist <image> — verified live on ES-DE 3.4.1; Cocoon is ES-DE-media-
// compatible by design), and both read a centralized `gamelists/<system>/gamelist.xml`
// `<name>` as the display title. So one emitter serves both frontends; the app passes
// each detected frontend's data dir (LODOR_ESDE_DIR, LODOR_COCOON_DIR).
//
// This mode does two things per target frontend:
//
//  1. Covers, STATE-AWARE. Copy each owned ROM's .media cover into the frontend's
//     covers tree, keyed to the on-disk basename. A ✓ on-device game gets the normal
//     cover; a ✘ cloud stub gets a DIMMED cover (cover.Dim) — the download-state cue
//     that replaces the marker once it's stripped from the title. Covers are keyed to
//     the marked name, so the ✘→✓ flip self-heals: the normal cover re-places under
//     the new name and the dimmed one prunes as an orphan.
//
//  2. Clean-title gamelists. Merge-write `gamelists/<system>/gamelist.xml` with
//     `<name>` = marker-stripped and `<path>` = ./<marked-rom> (no <image> — art comes
//     from downloaded_media). This removes the ✘/✓ marker from the visible grid title
//     while keeping it on disk for the launcher/manifest. A user's own scraped entries
//     in the file survive verbatim (mergeGamelist).
//
// Ownership of covers is tracked in the manifest (ManifestFrontendMedia) so ONLY covers
// this mode wrote are pruned; a user's own scraped media is never touched (ROM ownership
// = the marker prefix; user ROMs carry none). Gamelists are merge-preserving and never
// pruned by this mode (they may hold the user's own entries). Offline, filesystem-only.
//
// RESULT frontend_media targets=<N> placed=<M> gamelists=<K> pruned=<P>

// frontendTarget is one host frontend's data directory (holding downloaded_media/ and
// gamelists/). name is for logging + manifest context only; ownership keys on dest path.
type frontendTarget struct {
	name    string
	dataDir string
}

func (t frontendTarget) coversDir(system string) string {
	return filepath.Join(t.dataDir, "downloaded_media", system, "covers")
}

func (t frontendTarget) gamelistPath(system string) string {
	return filepath.Join(t.dataDir, "gamelists", system, "gamelist.xml")
}

// frontendTargets resolves every host-frontend data dir the app told us about (or the
// ES-DE default when its tree exists). Empty slice = nothing installed here → clean no-op.
func frontendTargets() []frontendTarget {
	var ts []frontendTarget
	if d := esdeDataDir(); d != "" {
		ts = append(ts, frontendTarget{"esde", d})
	}
	if d := os.Getenv("LODOR_COCOON_DIR"); d != "" && isDir(d) {
		ts = append(ts, frontendTarget{"cocoon", d})
	}
	return ts
}

// esdeDataDir returns the ES-DE data root or "". Honors LODOR_ESDE_DIR, the legacy
// LODOR_ESDE_MEDIA_DIR (a downloaded_media path → its parent), else the on-disk default.
func esdeDataDir() string {
	if v := os.Getenv("LODOR_ESDE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("LODOR_ESDE_MEDIA_DIR"); v != "" {
		return filepath.Dir(v) // .../ES-DE/downloaded_media -> .../ES-DE
	}
	def := filepath.Join(platform.BasePath(), "ES-DE")
	if isDir(def) {
		return def
	}
	return ""
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// ownedGame is one Lodor-owned (marker-prefixed) ROM discovered on disk.
type ownedGame struct {
	system   string // ROM subfolder name (ES-DE/Cocoon system id)
	base     string // on-disk basename WITH marker + ext, e.g. "✓ Zelda (USA).z64"
	stem     string // base minus ext, e.g. "✓ Zelda (USA)"
	isStub   bool   // ✘ cloud stub (not downloaded) → dimmed cover
	coverSrc string // .media/<stem>.png if it exists, else ""
}

func runPlaceFrontendMedia() {
	targets := frontendTargets()
	if len(targets) == 0 {
		fmt.Println("RESULT frontend_media targets=0 placed=0 gamelists=0 pruned=0")
		fmt.Fprintln(os.Stderr, "FRONTEND-MEDIA: no host frontend detected (set LODOR_ESDE_DIR / LODOR_COCOON_DIR) — nothing to do")
		os.Exit(0)
	}
	man := platform.LoadManifest()
	games := scanOwnedGames(filepath.Clean(platform.RomsDir()))
	placed, gamelists, pruned := reconcileFrontendMedia(games, targets, man)

	if err := man.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "FRONTEND-MEDIA WARN: manifest save: %v\n", err)
	}
	fmt.Printf("RESULT frontend_media targets=%d placed=%d gamelists=%d pruned=%d\n",
		len(targets), placed, gamelists, pruned)
	os.Exit(0)
}

// scanOwnedGames walks roms/<system>/ once and returns every Lodor-owned ROM (marker-
// prefixed; user ROMs carry no marker and are skipped).
func scanOwnedGames(romsRoot string) []ownedGame {
	var games []ownedGame
	sysEntries, err := os.ReadDir(romsRoot)
	if err != nil {
		return games
	}
	for _, sysDir := range sysEntries {
		if !sysDir.IsDir() || strings.HasPrefix(sysDir.Name(), ".") {
			continue
		}
		system := sysDir.Name()
		dir := filepath.Join(romsRoot, system)
		files, ferr := os.ReadDir(dir)
		if ferr != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			base := f.Name()
			if !platform.HasLeadingMarker(base) {
				continue // user ROM — not ours
			}
			romAbs := filepath.Join(dir, base)
			src := ""
			if cover.Exists(romAbs) {
				src = cover.MediaPath(romAbs)
			}
			games = append(games, ownedGame{
				system:   system,
				base:     base,
				stem:     base[:len(base)-len(filepath.Ext(base))],
				isStub:   platform.HasCloudMarker(base),
				coverSrc: src,
			})
		}
	}
	return games
}

func reconcileFrontendMedia(games []ownedGame, targets []frontendTarget, man *platform.Manifest) (placed, gamelists, pruned int) {
	wantCovers := map[string]struct{}{}
	for _, t := range targets {
		// 1. Covers (state-aware).
		for _, g := range games {
			if g.coverSrc == "" {
				continue
			}
			dest := filepath.Join(t.coversDir(g.system), g.stem+".png")
			wantCovers[dest] = struct{}{}
			if placeCover(g.coverSrc, dest, g.isStub) {
				placed++
			}
			man.Record(dest, platform.ManifestFrontendMedia, 0)
		}
		// 2. Clean-title gamelists, grouped by system.
		bySystem := map[string][]glEntry{}
		for _, g := range games {
			bySystem[g.system] = append(bySystem[g.system], glEntry{
				fsname: g.base,
				name:   platform.StripLeadingMarker(g.stem),
				// no image: art comes from downloaded_media, not the gamelist
			})
		}
		for system, entries := range bySystem {
			sort.Slice(entries, func(i, j int) bool { return entries[i].fsname < entries[j].fsname })
			if writeMergedGamelist(t.gamelistPath(system), entries, man) {
				gamelists++
			}
		}
	}
	// 3. Prune covers we previously wrote that are no longer wanted (✘→✓ flip, rename,
	//    removal). Manifest-tracked → user media is never touched.
	for path, e := range man.Entries {
		if e.Kind != platform.ManifestFrontendMedia {
			continue
		}
		if _, keep := wantCovers[path]; keep {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "FRONTEND-MEDIA WARN: prune %s: %v\n", path, err)
			continue
		}
		man.Forget(path)
		pruned++
	}
	return placed, gamelists, pruned
}

// placeCover writes src to dest (normal, or dimmed for a stub), skipping when dest is
// already byte-identical. Returns true when it wrote.
func placeCover(src, dest string, dim bool) bool {
	data, err := os.ReadFile(src)
	if err != nil {
		return false // source vanished between scan and copy — skip quietly
	}
	if dim {
		data = cover.Dim(data)
	}
	if cur, cerr := os.ReadFile(dest); cerr == nil && bytes.Equal(cur, data) {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "FRONTEND-MEDIA WARN: mkdir %s: %v\n", filepath.Dir(dest), err)
		return false
	}
	if err := fsutil.WriteFileAtomic(dest, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "FRONTEND-MEDIA WARN: write %s: %v\n", dest, err)
		return false
	}
	return true
}

// writeMergedGamelist merge-writes one centralized gamelist.xml (ES-DE/Cocoon layout):
// Lodor-owned entries get a clean <name> + ./<marked-rom> <path>; the user's own entries
// are preserved verbatim. Records ManifestGamelist (never pruned by this mode). Returns
// true when the file actually changed. Best-effort — a parse/write failure is a WARN.
func writeMergedGamelist(glPath string, entries []glEntry, man *platform.Manifest) bool {
	existing, rerr := os.ReadFile(glPath)
	if rerr != nil && !os.IsNotExist(rerr) {
		fmt.Fprintf(os.Stderr, "FRONTEND-MEDIA WARN %s: unreadable (%v) — skipped\n", glPath, rerr)
		return false
	}
	merged, merr := mergeGamelist(existing, entries)
	if merr != nil {
		fmt.Fprintf(os.Stderr, "FRONTEND-MEDIA WARN %s: %v — left untouched\n", glPath, merr)
		return false
	}
	if bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(merged)) {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(glPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "FRONTEND-MEDIA WARN: mkdir %s: %v\n", filepath.Dir(glPath), err)
		return false
	}
	if err := fsutil.WriteFileAtomic(glPath, merged, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "FRONTEND-MEDIA WARN %s: write failed (%v)\n", glPath, err)
		return false
	}
	man.Record(glPath, platform.ManifestGamelist, 0)
	return true
}
