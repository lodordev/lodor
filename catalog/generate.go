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
// platforms: for each platform the engine has a known MinUI emulator tag for, it
// emits fs_slug -> {slug: fs_slug, relative_path: "<Display Name> (<TAG>)"} where
// <TAG> is platform.PrimaryTag(fs_slug) and <Display Name> is the platform's RomM
// name (custom_name preferred, then name, falling back to fs_slug). Platforms with
// NO known tag are SKIPPED (no folder invented) and counted. Returns the generated
// map plus generated/skipped counts; never logs or returns a secret.
func GenerateDirectoryMappings(client platformLister) (mappings map[string]config.DirMapping, generated, skipped int, err error) {
	platforms, perr := client.GetPlatforms()
	if perr != nil {
		return nil, 0, 0, perr
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
			// Mini Flip). Mapping it would stub a library of games that can't launch — and a
			// search would happily download them. Skip it; a device that later adds the pak
			// picks the platform up on the next mapping generation.
			skipped++
			continue
		}
		display := sanitizeFolderName(platformDisplayName(p))
		mappings[p.FsSlug] = config.DirMapping{
			Slug:         p.FsSlug,
			RelativePath: fmt.Sprintf("%s (%s)", display, tag),
		}
		generated++
	}
	return mappings, generated, skipped, nil
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

	mappings, generated, skipped, gerr := GenerateDirectoryMappings(lister)
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
