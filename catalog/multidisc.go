// Multi-disc completeness (lodor#7, disc-1-first): under the hybrid design the
// launch path downloads only the first missing disc — so "downloaded but
// incomplete" is a first-class, VALID on-card state. The on-card .m3u lists ONLY
// discs with real bytes (the shipped LodorOS launcher parses the playlist
// pre-launch and refuses to launch while any listed disc is missing/0-byte), so
// the FULL set lives in the mirror manifest's canonical disc list; a legacy
// full-list .m3u (no manifest list yet) censuses by its own refs. This file is
// the offline (filesystem + manifest only) view of that state: what the
// --check-rom gate, the --fetch-discs modes, and the daemons' --prefetch-discs
// enumeration all key off. No network, no device.
package catalog

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/platform"
)

// IncompleteM3U describes one downloaded (non-stub) multi-disc game whose on-card
// disc set is incomplete: the .m3u is real, but the canonical set (manifest disc
// list; legacy fallback: the m3u's own refs) has a disc absent or 0-byte on card.
type IncompleteM3U struct {
	Path    string // absolute on-card .m3u path
	Total   int    // discs the playlist references
	Present int    // referenced discs with real bytes on the card
}

// discPresent reports whether one referenced disc has real bytes on the card
// (absent and 0-byte stub both read "not present" — same predicate the hooks'
// busybox `[ -s ]` scan uses, so shell and engine always agree).
func discPresent(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

// M3UCompleteness counts a real .m3u's referenced discs: total listed and how many
// hold real bytes. total==0 means the playlist references nothing usable (broken).
func M3UCompleteness(m3uPath string) (total, present int) {
	refs := M3UDiscRefs(m3uPath)
	for _, r := range refs {
		if discPresent(r) {
			present++
		}
	}
	return len(refs), present
}

// CanonicalDiscRefs resolves the manifest-recorded canonical disc list (the
// m3u-relative "<Game>/<disc>" lines SetDiscs stored) to absolute on-card paths.
// Same defensive line rules as M3UDiscRefs (no absolute lines, no ".."), so a
// corrupt manifest can never point a census — or a sweep — outside the game's
// folder. nil when the entry has no recorded list (single-file or legacy record).
func CanonicalDiscRefs(man *platform.Manifest, m3uPath string) []string {
	if man == nil {
		return nil
	}
	e, ok := man.Entry(m3uPath)
	if !ok || len(e.Discs) == 0 {
		return nil
	}
	dir := filepath.Dir(m3uPath)
	var out []string
	for _, line := range e.Discs {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if filepath.IsAbs(line) || strings.Contains(line, "..") {
			continue
		}
		out = append(out, filepath.Join(dir, line))
	}
	return out
}

// RomDiscCompleteness is the manifest-first completeness census (lodor#7,
// local-only .m3u): the on-card playlist lists only present discs — always
// "complete" by its own refs — so the FULL set comes from the manifest's
// canonical disc list. A record without one (legacy card, pre-local-only) falls
// back to the .m3u's own refs, which on such a card ARE the full list (stubs
// included). total==0 = nothing usable referenced anywhere (broken).
func RomDiscCompleteness(man *platform.Manifest, m3uPath string) (total, present int) {
	refs := CanonicalDiscRefs(man, m3uPath)
	if refs == nil {
		return M3UCompleteness(m3uPath)
	}
	for _, r := range refs {
		if discPresent(r) {
			present++
		}
	}
	return len(refs), present
}

// IncompleteMultiDiscDownloads walks the mirror manifest for DOWNLOADED .m3u games
// (a 0-byte .m3u is still a stub — the launch path owns those) whose referenced
// disc set is incomplete. Offline by design: manifest + filesystem only, so the
// daemons can ask "is there prefetch work?" without touching the radio. Results
// are path-sorted for deterministic RESULT lines and logs.
func IncompleteMultiDiscDownloads() []IncompleteM3U {
	man := platform.LoadManifest()
	sd := sdcardRoot()
	var out []IncompleteM3U
	for rel, e := range man.Entries {
		if e.Kind != platform.ManifestDownload || !strings.EqualFold(filepath.Ext(rel), ".m3u") {
			continue
		}
		abs := filepath.Join(sd, rel) // manifest keys are SDCARD-relative (leading "/" kept) — same join uninstall uses
		fi, err := os.Stat(abs)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			continue // gone, or still a 0-byte stub (not this pass's job)
		}
		total, present := RomDiscCompleteness(man, abs)
		if total == 0 || present >= total {
			continue // broken playlist (nothing referenced) or already complete
		}
		out = append(out, IncompleteM3U{Path: abs, Total: total, Present: present})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
