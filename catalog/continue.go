// The cross-device "Continue" collection (task #37).
//
// NextUI CANNOT launch a ROM from a "Continue" folder under Roms/: getEmuName
// (workspace/all/common/utils.c:352) derives the emulator tag from the FIRST path
// component under Roms/ only — it truncates the path at the first '/' after
// "Roms/" and reads the trailing "(TAG)" from THAT folder name. So both a flat
// "Roms/Continue/game.gba" and a nested "Roms/Continue/Game Boy Advance (GBA)/
// game.gba" resolve the emulator to the literal folder "Continue" and fail. The
// only Roms/-side layout that launches is one root folder PER TAG ("0) Continue
// (GBA)", "0) Continue (SFC)", …) — root clutter, and a second on-card presence
// per game (double download, duplicated bytes).
//
// A NextUI COLLECTION is the correct vehicle: Collections/<name>.txt lines are
// SDCARD-relative paths to the game's REAL platform-folder file (nextui.c
// getCollection), so launching a Continue entry is byte-identical to launching
// from the platform folder — same HOOK_ROM_PATH into the fetch/restore hooks,
// same emulator resolution, no duplicate presence. Collection file order is
// PRESERVED by the browser (nextui.c:244 — collections are "not alphabetized"),
// so newest-first is expressed directly. The name "0) Continue" sorts first in
// the Collections list and NextUI's trimSortingMeta strips the "0) " for display,
// showing just "Continue".
//
// CGO-free, stdlib only.
package catalog

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

const (
	// continueCollectionName is the on-card collection filename stem. A RomM
	// collection with the same (sanitized) name is overwritten by this writer —
	// the Continue mirror runs last and wins.
	continueCollectionName = "0) Continue"
	// continueCap bounds the Continue list (task #37: "N most-recent", capped).
	continueCap = 12
)

// saveLister is the optional client capability the Continue mirror needs. It is
// asserted at runtime (like getMappedPlatforms' GetPlatforms) so the romClient
// interface — and every existing test fake — stays unchanged; a client without
// it simply skips the Continue mirror.
type saveLister interface {
	GetSaves(q romm.SaveQuery) ([]romm.Save, error)
}

// ContinueList builds the cross-device continue list: the N most-recently-played
// games ACROSS DEVICES (RomM server saves, newest first), deduped per game,
// resolved to their on-card SDCARD-relative paths via idPath. Entries whose
// on-card file is missing are skipped; ghost saves (byte-less server records,
// #63) never drive recency. Shared by the full collections mirror and the light
// --sync-continue mode (task #133) — same list, two delivery cadences.
//
// Every failure path returns an empty list, never an error: Continue is a
// convenience layer, never a reason to fail the pass that asked for it.
func ContinueList(client romClient, cfg *config.Config, idPath map[int]string) []string {
	sl, ok := client.(saveLister)
	if !ok {
		return nil
	}
	platforms, perr := getMappedPlatforms(client, cfg)
	if perr != nil {
		return nil // unreachable / no platform capability — quietly no Continue this run
	}

	// Newest save per ROM across every mapped platform (cross-device: server saves
	// carry every device's syncs; UpdatedAt is the last play that pushed).
	newest := map[int]time.Time{}
	for _, p := range platforms {
		saves, gerr := sl.GetSaves(romm.SaveQuery{PlatformID: p.ID})
		if gerr != nil {
			continue
		}
		for _, s := range saves {
			// Ghost (#63): a byte-less record isn't a playable session. Mirrors
			// sync.IsGhostSave — inlined because sync imports catalog (no cycle).
			if s.FileSizeBytes <= 0 || s.RomID == 0 {
				continue
			}
			if t, seen := newest[s.RomID]; !seen || s.UpdatedAt.After(t) {
				newest[s.RomID] = s.UpdatedAt
			}
		}
	}
	if len(newest) == 0 {
		return nil
	}

	type ent struct {
		id int
		t  time.Time
	}
	order := make([]ent, 0, len(newest))
	for id, t := range newest {
		order = append(order, ent{id, t})
	}
	sort.Slice(order, func(i, j int) bool {
		if !order[i].t.Equal(order[j].t) {
			return order[i].t.After(order[j].t) // newest first
		}
		return order[i].id < order[j].id // deterministic tie-break
	})

	sd := sdcardRoot()
	var lines []string
	for _, e := range order {
		rel, found := idPath[e.id]
		if !found || rel == "" {
			continue // not mirrored on this card (unmapped / unlaunchable platform)
		}
		onCard, cok := resolveOnCardRel(sd, rel)
		if !cok {
			continue // stale index entry — never list a path the browser can't open
		}
		lines = append(lines, onCard)
		if len(lines) == continueCap {
			break
		}
	}
	return lines
}

// resolveOnCardRel returns the SDCARD-relative path that is on-card TRUE for rel: rel
// itself when it exists, else the same basename under a different download-state
// marker ("✓ "/"✘ "/bare/legacy — preferring on-device). The index can lag the card
// between syncs: the post-launch hook renames ✘→✓ the moment a download-on-launch
// game exits, so an index-derived path written verbatim may name a file that no
// longer exists — exactly how the Smart Pro 2026-07-03 Continue head/collection ended
// up pointing at "✘ …" while the card held "✓ …" (task #135). Every Continue surface
// (collection, head file, root label) must emit what the CARD says, not what the
// index remembers. ok=false when no variant exists.
func resolveOnCardRel(sd, rel string) (string, bool) {
	if _, err := os.Stat(filepath.Join(sd, rel)); err == nil {
		return rel, true
	}
	dir := filepath.Dir(rel)
	base := platform.StripLeadingMarker(filepath.Base(rel))
	for _, m := range []string{platform.MarkerOnDevice, platform.MarkerCloud, "", "[v] ", "[^] "} {
		cand := filepath.Join(dir, m+base)
		if cand == rel {
			continue // already tried verbatim
		}
		if _, err := os.Stat(filepath.Join(sd, cand)); err == nil {
			return cand, true
		}
	}
	return "", false
}

// LoadIndexIDPath loads the rom_id -> SDCARD-relative-path map from the LOCAL
// catalog-index.json. An absent/unreadable index returns an empty map — callers
// treat that as "no library mirrored yet" and degrade quietly. Near-instant: no
// network, one small file read.
func LoadIndexIDPath(cfg *config.Config) map[int]string {
	idPath := map[int]string{}
	if idx, lerr := loadIndex(IndexPath(cfg)); lerr == nil {
		for _, pidx := range idx.Platforms {
			for id, rel := range pidx.ByID {
				idPath[id] = rel
			}
		}
	}
	return idPath
}

// writeContinueFile writes Collections/"0) Continue.txt" from lines (temp +
// atomic rename — feedback: non-atomic card writes have zeroed files before).
// lines empty => nothing is written and any existing file is LEFT ALONE (a
// transient-empty feed must not erase a good list; the full mirror's prune is
// the one place a stale Continue is removed). Returns entries written.
func writeContinueFile(colDir string, lines []string) int {
	if len(lines) == 0 {
		return 0
	}
	if mkErr := os.MkdirAll(colDir, 0o755); mkErr != nil {
		return 0
	}
	name := sanitizeCollectionName(continueCollectionName) + ".txt"
	final := filepath.Join(colDir, name)
	tmp := final + ".tmp"
	if werr := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); werr != nil {
		return 0
	}
	if rerr := os.Rename(tmp, final); rerr != nil {
		_ = os.Remove(tmp)
		return 0
	}
	return len(lines)
}

// ContinueDir is the on-card Collections directory the Continue file lives in.
func ContinueDir() string {
	return filepath.Join(platform.RomsDir(), "..", "Collections")
}

// SyncContinue is the LIGHT continue refresh (task #133, the fast half of "Sync
// now"): rebuild the Continue collection + merge the cross-device recents into
// the host's Recently Played, using ONLY the local index + per-platform save
// listings — no catalog mirror, no collections listing. Unlike the full mirror
// it never prunes sibling collections and an empty feed leaves the existing
// Continue file untouched. Returns entries written, recents merged, recents total.
func SyncContinue(client romClient, cfg *config.Config) (entries, merged, total int) {
	lines := ContinueList(client, cfg, LoadIndexIDPath(cfg))
	entries = writeContinueFile(ContinueDir(), lines)
	merged, total = MergeRecents(lines)
	deliverContinueHead(lines)
	return entries, merged, total
}

// deliverContinueHead is the shared tail of both Continue cadences (task #134):
// persist the head entry for the LODORCT resume dispatcher and refresh the
// dynamic root label. Best-effort, capability-gated, never affects the caller's
// counts; an empty feed changes nothing (same rule as writeContinueFile).
func deliverContinueHead(lines []string) {
	if len(lines) == 0 {
		return
	}
	WriteContinueHead(lines)
	UpdateContinueRootLabel(DisplayNameFor(lines[0]))
}

// mirrorContinue is the full-mirror delivery of ContinueList: writes the file,
// marks it kept for the caller's prune, and merges the recents. Returns the
// number of entries written; 0 means NO file was written (an empty feed leaves
// no stale "Continue" in the browser — the caller's prune removes any previous
// file, because 0-entry runs never mark it kept).
func mirrorContinue(client romClient, cfg *config.Config, idPath map[int]string, colDir string, kept map[string]bool) int {
	lines := ContinueList(client, cfg, idPath)
	n := writeContinueFile(colDir, lines)
	if n > 0 {
		kept[sanitizeCollectionName(continueCollectionName)+".txt"] = true
	}
	// Cross-device recents ride the same list (task #132): merge is best-effort
	// and never affects the mirror's counts.
	MergeRecents(lines)
	deliverContinueHead(lines)
	return n
}

// -----------------------------------------------------------------------------------
// Cross-device recents -> the host's native "Recently Played" (task #132).
//
// MinUI-family hosts (NextUI included) read SHARED_USERDATA/.minui/recent.txt at every
// menu init: one "<SDCARD-relative path>[\t<alias>]" per line, NEWEST FIRST, capped at
// 24 (nextui.c MAX_RECENTS), entries whose file is missing dropped on read. Recently
// Played is a native TOP-LEVEL root row — so merging the cross-device list here puts
// Continue at the root with ZERO launcher change and real, launchable paths (stubs
// download via the pre-launch hook exactly like any other launch).
//
// Merge contract (the safety that makes injection okay):
//   - existing lines keep their exact order and aliases — the DEVICE's own recency is
//     the primary truth; NextUI's addRecent/bump-to-top always wins for local plays.
//   - server-only entries are APPENDED BELOW the local ones, newest-first (recent.txt
//     carries no timestamps, so cross-position interleaving would be a guess — we
//     don't guess).
//   - dedupe by exact path: NextUI itself only dedupes in addRecent, NOT when reading
//     the file, so a lazy injector would create visible duplicate rows.
//   - cap at the host's MAX_RECENTS; local lines are never evicted in favor of
//     injected ones.
//   - atomic temp+rename write (FAT32 — feedback_lodor_fat32_atomic_writes). NextUI
//     rewrites this file from memory on menu init / game exit; our writers run in the
//     windows where the launcher is dead (boot.d, launch hooks, Tools paks), and a
//     lost merge simply re-lands on the next sync — eventual consistency, no torn file.
// -----------------------------------------------------------------------------------

// nextuiMaxRecents mirrors nextui.c MAX_RECENTS.
const nextuiMaxRecents = 24

// recentsPath returns the MinUI-family shared recents file, or "" when this card has
// no .minui shared state (not a MinUI-family host) — the capability gate that keeps
// this host-specific delivery from spraying files onto foreign layouts.
func recentsPath() string {
	dir := filepath.Join(sdcardRoot(), ".userdata", "shared", ".minui")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return filepath.Join(dir, "recent.txt")
}

// MergeRecents merges the cross-device continue list (SDCARD-relative paths, newest
// first) into the host's recent.txt per the contract above. Returns how many entries
// were newly added and the total lines written. (0,0) = nothing to do or no
// MinUI-family recents on this card.
func MergeRecents(lines []string) (merged, total int) {
	if len(lines) == 0 {
		return 0, 0
	}
	rp := recentsPath()
	if rp == "" {
		return 0, 0
	}

	var out []string
	seen := map[string]bool{}
	if data, err := os.ReadFile(rp); err == nil {
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimRight(raw, "\r")
			if line == "" {
				continue
			}
			path := line
			if i := strings.IndexByte(line, '\t'); i >= 0 {
				path = line[:i]
			}
			if strings.Contains(path, "(LODORGM)/") || strings.Contains(path, "(LODORCT)/") {
				continue // dispatcher dummy rows (B3): scrubbed by the pak, never re-preserved here
			}
			if seen[path] {
				continue // pre-existing duplicate — collapse it while we're here
			}
			seen[path] = true
			out = append(out, line) // alias and local order preserved verbatim
		}
	}
	for _, rel := range lines {
		if len(out) >= nextuiMaxRecents {
			break
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
		merged++
	}
	if merged == 0 {
		return 0, len(out) // nothing new — don't rewrite the file for no reason
	}
	if len(out) > nextuiMaxRecents {
		out = out[:nextuiMaxRecents]
	}

	tmp := rp + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, 0
	}
	if _, err = f.WriteString(strings.Join(out, "\n") + "\n"); err == nil {
		err = f.Sync() // FAT32: fsync before rename or a yank can zero the file
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, 0
	}
	if err := os.Rename(tmp, rp); err != nil {
		_ = os.Remove(tmp)
		return 0, 0
	}
	return merged, len(out)
}
