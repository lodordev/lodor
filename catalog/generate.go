package catalog

// Auto-generation of directory_mappings (BLUEPRINT §4/§7 self-heal). First-run
// onboarding writes host/auth/device into config.json but never the per-platform
// directory_mappings the mirror needs — so with none present, --mirror-catalog
// stubbed nothing (getMappedPlatforms returns empty). This file lets the mirror
// self-heal: when mappings are empty it fetches the user's platforms, generates one
// mapping per platform the engine has a known MinUI tag for, persists them, and
// proceeds. CGO-free, stdlib only.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// platformLister is the platforms-fetch capability the generator needs, kept as an
// interface (matching getMappedPlatforms' type-assert) so generation stays testable
// without a live server.
type platformLister interface {
	GetPlatforms() ([]romm.Platform, error)
}

// GenerateDirectoryMappings builds a directory_mappings map from the user's RomM
// platforms: for each platform the engine has a known MinUI emulator tag for AND a
// launchable Emu pak installed, it emits fs_slug -> {slug: fs_slug, relative_path}
// where relative_path is mode-aware (mirrorFolderName): "<Display> (<TAG>)" in
// "own"/"merge", "<Display> RomM (<TAG>)" in "separate" so RomM folders never collide
// with the user's own "<Display> (<TAG>)". <TAG> is platform.PrimaryTag(fs_slug) and
// <Display> is the platform's RomM name (custom_name preferred, then name, falling back
// to fs_slug). Platforms with NO known tag, or no installed pak, are SKIPPED (no folder
// invented) and counted.
//
// MERGE additionally ADOPTS the user's existing folder (C1 §1, adopt-by-tag): a
// top-level Roms/ dir whose trailing "(TAG)" — or bare name — matches the platform's
// tag becomes the mapping target, so stubs land among the user's own games. Multiple
// candidates pick the one holding the most real (non-hidden, non-0-byte) files —
// the user's "real" folder — tie broken lexicographically; the un-adopted siblings
// still save-sync via slugForRomPath's tag fallback (adoption only decides where
// stubs LAND). No candidate ⇒ the clean "<Display> (<TAG>)" is created (no
// collision by definition). Adoptions are logged (MAPGEN adopt …, host-free).
// Returns the generated map plus generated/skipped counts; never logs or returns a
// secret.
func GenerateDirectoryMappings(client platformLister, mode string) (mappings map[string]config.DirMapping, generated, skipped int, err error) {
	platforms, perr := client.GetPlatforms()
	if perr != nil {
		return nil, 0, 0, perr
	}

	var romsDirs []string
	if mode == config.MirrorModeMerge {
		romsDirs = listRomsFolders()
	}

	mappings = map[string]config.DirMapping{}
	for _, p := range platforms {
		if p.FsSlug == "" {
			skipped++
			continue
		}
		tag, ok := platform.PrimaryTag(p.FsSlug)
		if !ok {
			// No known MinUI tag/save dir for this platform — don't invent a folder.
			skipped++
			continue
		}
		if !platform.HasEmuPak(tag) {
			// Known tag, but NO emulator pak installed on this device (e.g. DS/3DS/PSP on a
			// Mini Flip, or N64 on a Trimui/tg5040 that ships no N64 pak). Mapping it would
			// stub a library of games that can't launch — and a search would happily download
			// them. Skip it; a device that later adds the pak picks the platform up on the
			// next mapping generation.
			skipped++
			continue
		}
		display := sanitizeFolderName(platformDisplayName(p))
		rel := mirrorFolderName(display, tag, mode)
		if mode == config.MirrorModeMerge {
			if adopted, nFiles, nCand := adoptFolderForTag(romsDirs, tag); adopted != "" {
				rel = adopted
				fmt.Fprintf(os.Stderr, "MAPGEN adopt %s -> %s (%d files, %d candidates)\n",
					p.FsSlug, adopted, nFiles, nCand)
			}
		}
		mappings[p.FsSlug] = config.DirMapping{
			Slug:         p.FsSlug,
			RelativePath: rel,
		}
		generated++
	}
	return mappings, generated, skipped, nil
}

// listRomsFolders returns the visible top-level Roms/ directory names — the merge
// adoption candidates. Skips hidden names (leading "."), ".disabled"-suffixed dirs
// (NextUI's hide() convention) and the Lodor root-entry affordance folders.
func listRomsFolders() []string {
	entries, err := os.ReadDir(filepath.Join(sdcardRoot(), "Roms"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".disabled") {
			continue
		}
		if strings.HasSuffix(n, "(LODORGM)") || strings.HasSuffix(n, "(LODORCT)") {
			continue // launcher affordances, never library folders
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// adoptFolderForTag picks the user folder a platform's stubs should land in:
// candidates are dirs whose trailing "(TAG)" (case-insensitive) — or bare name —
// equals the platform tag. Multiple candidates: most real (non-hidden, non-0-byte,
// non-marked) files wins — the user's "real" folder; tie → lexicographic first
// (candidates arrive sorted). Returns ("",0,0) when no candidate exists.
func adoptFolderForTag(romsDirs []string, tag string) (folder string, files, candidates int) {
	best, bestFiles := "", -1
	for _, d := range romsDirs {
		dirTag := tagFromFolderName(d)
		if !strings.EqualFold(dirTag, tag) && !strings.EqualFold(d, tag) {
			continue
		}
		candidates++
		n := countRealFiles(filepath.Join(sdcardRoot(), "Roms", d))
		if n > bestFiles { // strict: ties keep the earlier (lexicographic) candidate
			best, bestFiles = d, n
		}
	}
	if best == "" {
		return "", 0, 0
	}
	return best, bestFiles, candidates
}

// countRealFiles counts a folder's top-level regular files that are non-hidden,
// non-0-byte and not Lodor-marked — the "user's real games" signal the adoption
// tiebreak uses.
func countRealFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || platform.HasLeadingMarker(e.Name()) {
			continue
		}
		if fi, ferr := e.Info(); ferr == nil && fi.Size() > 0 {
			n++
		}
	}
	return n
}

// mirrorFolderName builds the Roms/ folder a platform's RomM games are mirrored into.
// "own"/"merge": the clean "<Display> (<TAG>)" (merge only CREATES it when adoption
// found no user folder — no collision by definition). "separate": "<Display> RomM
// (<TAG>)" — NextUI's getEmuName binds off the LAST paren so this still launches
// <TAG>.pak, getDisplayName strips the trailing paren so it reads "<Display> RomM",
// and it can never collide with the user's own "<Display> (<TAG>)" folder (issue #68).
func mirrorFolderName(display, tag, mode string) string {
	// CFW-variant: MinUI builds "<Display> (<TAG>)"; OnionOS builds the bare <TAG>.
	return platform.MirrorFolderName(display, tag, mode)
}

// sanitizeFolderName makes a platform display name safe as a SINGLE flat folder
// component: the launcher expects Roms/<folder>/, so a name carrying a path
// separator (e.g. RomM's "Sega Mega Drive/Genesis") must not spawn a nested dir.
// Replaces the reserved set / \ : * ? " < > | with "-", collapses any runs of
// whitespace introduced, and trims — mirroring sanitizeCollectionName's intent.
func sanitizeFolderName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// platformDisplayName picks the human folder label for a platform: custom_name when
// set, else the RomM name, else the fs_slug as a last resort.
func platformDisplayName(p romm.Platform) string {
	if s := strings.TrimSpace(p.CustomName); s != "" {
		return s
	}
	if s := strings.TrimSpace(p.Name); s != "" {
		return s
	}
	return p.FsSlug
}

// ensureDirectoryMappings lazily auto-generates and persists directory_mappings when
// cfg has none, mutating cfg in place so the caller's mirror walk sees them. EXISTING
// mappings are never touched (respecting a user's hand-tuned set). Generation and
// persistence are logged to STDERR only — the §8 stdout MIRROR line stays clean. A
// generation/persist failure is non-fatal here only if it leaves cfg unmapped; we
// return the error so the caller decides (the mirror treats it as a hard error so
// the launcher reports "couldn't reach RomM" rather than silently stubbing nothing).
func ensureDirectoryMappings(client romClient, cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	if len(cfg.DirectoryMappings) > 0 {
		return nil // respect existing/hand-tuned mappings — never overwrite
	}

	lister, ok := client.(platformLister)
	if !ok {
		return errNoPlatforms
	}

	mode := cfg.ResolvedMirrorMode()
	mappings, generated, skipped, gerr := GenerateDirectoryMappings(lister, mode)
	if gerr != nil {
		return gerr
	}
	if generated == 0 {
		// Nothing to map (no platforms, or none with a known tag). Leave config as-is;
		// the mirror will simply produce created=0, which is the honest result.
		fmt.Fprintf(os.Stderr, "MAPGEN generated=0 skipped=%d (no mappable platforms)\n", skipped)
		return nil
	}

	if werr := config.WriteDirectoryMappings(mappings); werr != nil {
		return werr
	}
	cfg.DirectoryMappings = mappings

	fmt.Fprintf(os.Stderr, "MAPGEN generated=%d skipped=%d (auto-generated directory_mappings; persisted to config.json)\n",
		generated, skipped)
	// Log the slug->folder names (NO host/token) so the run is auditable.
	slugs := make([]string, 0, len(mappings))
	for s := range mappings {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	for _, s := range slugs {
		fmt.Fprintf(os.Stderr, "MAPGEN  %s -> %s\n", s, mappings[s].RelativePath)
	}
	return nil
}
