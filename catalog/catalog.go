// Package catalog mirrors the RomM library onto the SD card as 0-byte stub files
// and builds the JSON index that replaces grout's sqlite cache (BLUEPRINT §4, §5).
//
// MirrorCatalog walks every mapped platform live (no warm cache), stubs each
// not-yet-downloaded single-file ROM into its Roms/<System>/ folder, and writes a
// catalog-index.json keying both the canonical local basename and the full fs_name
// to each rom_id. ResolveRomID reverses a local ROM path back to its rom_id using
// that index — the clean replacement for the sqlite FS lookups.
//
// CGO-free, stdlib only.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lodor/config"
	"lodor/cover"
	"lodor/platform"
	"lodor/romm"
)

// saveExts are extensions that mark a file as a SAVE rather than a game. Such files
// must never be stubbed into Roms/ (BLUEPRINT §4 — ".state" included).
var saveExts = map[string]bool{
	".srm": true, ".sav": true, ".rtc": true, ".state": true, ".dsv": true,
	".mcr": true, ".mcd": true, ".brm": true, ".eep": true, ".sra": true,
	".fla": true, ".mpk": true, ".nv": true,
}

func isSaveExt(p string) bool { return saveExts[strings.ToLower(filepath.Ext(p))] }

// romClient is the subset of *romm.Client this package needs, kept as an interface
// so the mirror is testable without a live server.
type romClient interface {
	GetRoms(query romm.GetRomsQuery) (romm.PaginatedRoms, error)
	GetCollections() ([]romm.Collection, error)
	DownloadCover(coverPath string) ([]byte, error)
}

// platformIndex holds the two lookup tables for one platform.
type platformIndex struct {
	ByBasename map[string]int `json:"by_basename"`
	ByFsname   map[string]int `json:"by_fsname"`
}

// index is the on-disk catalog-index.json shape (BLUEPRINT §5).
type index struct {
	Version   int                      `json:"version"`
	Platforms map[string]platformIndex `json:"platforms"`
}

// Reporter receives honest, real-count progress from a long mirror so the caller can
// surface it to the launcher's side-channels (/tmp/dl-progress + /tmp/romm-phase).
// Both callbacks are best-effort and may be nil; the mirror's RESULT counts and error
// are the real gate, never the progress. A nil Reporter (or nil fields) disables
// emission entirely — the catalog package stays decoupled from the /tmp writers and
// fully testable without them (BLUEPRINT §8/§10).
//
//   - Phase(label): a live one-line human label, e.g. "Mirroring Game Boy (3/12)".
//   - Percent(pct): an integer 0..100 overall completion (real, monotonic-ish, coarse).
type Reporter struct {
	Phase   func(label string)
	Percent func(pct int)
}

// phase/percent are nil-safe shims so the mirror body can report unconditionally.
func (r *Reporter) phase(label string) {
	if r != nil && r.Phase != nil {
		r.Phase(label)
	}
}

func (r *Reporter) percent(pct int) {
	if r != nil && r.Percent != nil {
		r.Percent(pct)
	}
}

// platformDisplay picks a short human label for a platform's live phase line: its
// RomM name (custom_name preferred, then name), falling back to the fs_slug. Never a
// host/token — only the platform's own display fields.
func platformDisplay(p romm.Platform) string {
	if s := strings.TrimSpace(p.CustomName); s != "" {
		return s
	}
	if s := strings.TrimSpace(p.Name); s != "" {
		return s
	}
	return p.FsSlug
}

// IndexPath returns the absolute path of catalog-index.json:
// <SDCARD>/Tools/<PLATFORM>/RomM Sync.pak/catalog-index.json, honoring the
// SDCARD_PATH and PLATFORM environment variables (defaults /mnt/SDCARD, miyoomini).
func IndexPath(cfg *config.Config) string {
	sd := os.Getenv("SDCARD_PATH")
	if sd == "" {
		sd = "/mnt/SDCARD"
	}
	plat := os.Getenv("PLATFORM")
	if plat == "" {
		plat = "miyoomini"
	}
	return filepath.Join(sd, "Tools", plat, "RomM Sync.pak", "catalog-index.json")
}

// MirrorCatalog stubs every not-yet-downloaded single-file ROM of each mapped
// platform into its Roms/ folder and, while iterating, builds and atomically writes
// the catalog index to IndexPath. Returns the per-action counts. Multi-file ROMs are
// counted and skipped (v1). A ROM whose local path resolves to a save extension is
// skipped; an already-present file is counted as existing; otherwise a 0-byte stub
// is created.
func MirrorCatalog(client romClient, cfg *config.Config, rep *Reporter) (created, existing, skipped, multifile, covers int, err error) {
	idx := index{Version: 1, Platforms: map[string]platformIndex{}}
	coversOn := cfg.CoversEnabled()

	rep.percent(0)
	rep.phase("Reading library…")

	// Self-heal: first-run onboarding writes host/auth/device but no directory_mappings,
	// so getMappedPlatforms would return empty and we would stub nothing. When mappings
	// are absent, auto-generate them from the user's platforms (logged to stderr),
	// persist to config.json, and mutate cfg so the walk below sees them. Existing
	// mappings are left untouched. A generation/persist failure is a config/reachability
	// error (same class as the platforms fetch) -- surface it.
	if merr := ensureDirectoryMappings(client, cfg); merr != nil {
		return 0, 0, 0, 0, 0, merr
	}

	// Mirror only platforms the user has mapped a Roms folder for; others have
	// nowhere to put stubs. We resolve those platforms via the directory_mappings
	// keys directly so no /api/platforms call is needed: every mapping key is an
	// fs_slug, and GetRoms is filtered by platform id — but we don't have the id
	// without the platforms list, so fetch platforms once.
	platforms, perr := getMappedPlatforms(client, cfg)
	if perr != nil {
		return 0, 0, 0, 0, 0, perr
	}

	// Weight the overall percent by each platform's rom_count so the bar advances in
	// proportion to real work (a 2000-game platform moves it far more than a 12-game
	// one) — real-and-fine. If rom_count is unavailable everywhere (totalWork==0) we
	// fall back to a coarse platforms-processed mapping below; both are honest.
	totalWork := 0
	for _, p := range platforms {
		totalWork += p.RomCount
	}
	doneWork := 0
	nPlat := len(platforms)
	// coverTotal/coverDone drive the "Fetching cover N/M…" label only (best-effort,
	// real counts). coverTotal is the library size (sum of rom_count) so the
	// denominator matches what the user sees; coverDone counts attempted covers.
	coverTotal := totalWork
	coverDone := 0

	for pi2, p := range platforms {
		// Live label BEFORE the (potentially slow) GetRoms call, so the user sees which
		// platform is in flight rather than a frozen previous label.
		rep.phase(fmt.Sprintf("Mirroring %s (%d/%d)…", platformDisplay(p), pi2+1, nPlat))

		page, gerr := client.GetRoms(romm.GetRomsQuery{PlatformIDs: []int{p.ID}})
		if gerr != nil {
			// Skip this platform's stubs but keep going (parity with grout's WARN). Still
			// advance the bar by this platform's weight so a single unreachable platform
			// doesn't stall the percent.
			doneWork += p.RomCount
			rep.percent(mirrorPct(pi2+1, nPlat, doneWork, totalWork))
			continue
		}
		pi := idx.Platforms[p.FsSlug]
		if pi.ByBasename == nil {
			pi.ByBasename = map[string]int{}
			pi.ByFsname = map[string]int{}
		}
		for i := range page.Items {
			rom := page.Items[i]

			// Index every ROM (single- and multi-file) so resolution works for all.
			if rom.CanonicalLocalBasename() != "" {
				pi.ByBasename[rom.CanonicalLocalBasename()] = rom.ID
			}
			if rom.FsName != "" {
				pi.ByFsname[rom.FsName] = rom.ID
			}

			// Box art: fetch this rom's cover into Roms/<System>/.media/<name>.png
			// (NextUI convention) for the WHOLE library — stubs included — so the browser
			// shows art before a game is even downloaded. Graceful + non-fatal: a coverless
			// (unidentified) rom is skipped, an already-present cover is skipped, and any
			// fetch/decode error is counted but NEVER aborts the mirror. Progress flows
			// through the existing side-channels only. Multi-file roms are stubbed below as
			// .m3u; we fetch their cover too (romPath is the .m3u path).
			if coversOn {
				if cp := rom.CoverPath(); cp != "" {
					if rp := platform.LocalRomPath(cfg, rom); rp != "" && !isSaveExt(rp) {
						coverDone++
						if coverTotal > 0 {
							rep.phase(fmt.Sprintf("Fetching cover %d/%d…", coverDone, coverTotal))
						}
						if out, _ := cover.FetchAndSave(client, cp, rp); out == cover.OutcomeSaved {
							covers++
						}
					}
				}
			}

			// Multi-file (multi-disc) ROMs ARE stubbed now: LocalRomPath returns the
			// game's <FsNameNoExt>.m3u path, so a 0-byte .m3u stub is dropped exactly like
			// a single-file game's stub. Tapping it runs the engine's multi-disc download
			// (per-disc streamed + hash-verified), which replaces the stub with a real
			// playlist and fills the disc subfolder. We still count it in multifile for
			// visibility, but it is NO LONGER skipped — the old behavior left multi-disc
			// games invisible (no stub to tap). The broken bare-.m3u single-file import is
			// a different case, caught honestly at download time (isBareM3U).
			if rom.HasMultipleFiles {
				multifile++
				// fall through to the normal stub-create below using the .m3u path.
			}
			path := platform.LocalRomPath(cfg, rom)
			if path == "" {
				skipped++
				continue
			}
			if isSaveExt(path) {
				skipped++
				continue
			}
			if _, statErr := os.Stat(path); statErr == nil {
				existing++
				continue
			}
			if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
				skipped++
				continue
			}
			f, crErr := os.Create(path)
			if crErr != nil {
				skipped++
				continue
			}
			_ = f.Close()
			created++
		}
		idx.Platforms[p.FsSlug] = pi

		// Platform finished: advance the weighted bar and report the running stub count
		// (honest live totals — no fake spinner, per feedback_no_fake_ui_state).
		doneWork += p.RomCount
		rep.percent(mirrorPct(pi2+1, nPlat, doneWork, totalWork))
		rep.phase(fmt.Sprintf("Stubbing library… %d games", created))
	}

	rep.phase("Writing index…")
	if werr := writeIndexAtomic(IndexPath(cfg), idx); werr != nil {
		return created, existing, skipped, multifile, covers, werr
	}
	rep.percent(100)
	if coversOn {
		rep.phase(fmt.Sprintf("Library updated — %d new, %d covers", created, covers))
	} else {
		rep.phase(fmt.Sprintf("Library updated — %d new", created))
	}
	return created, existing, skipped, multifile, covers, nil
}

// mirrorPct maps mirror progress to an integer 0..100. When per-platform rom_count is
// available (totalWork>0) it weights by ROMs processed — fine and real. Otherwise it
// falls back to the coarse-but-real platforms-processed ratio. It never reports 100
// until the caller's explicit final percent(100), so the bar can't claim "done" while
// the index write is still pending; it caps the in-loop value at 99.
func mirrorPct(platformsDone, platformsTotal, doneWork, totalWork int) int {
	var pct int
	switch {
	case totalWork > 0:
		pct = doneWork * 100 / totalWork
	case platformsTotal > 0:
		pct = platformsDone * 100 / platformsTotal
	default:
		pct = 0
	}
	if pct > 99 {
		pct = 99
	}
	if pct < 0 {
		pct = 0
	}
	return pct
}

// MirrorCollections writes one Collections/<sanitized>.txt per RomM collection,
// listing the SDCARD-relative path of each member ROM that is actually present on
// the card. Empty collections are skipped. Returns written, empty, and total counts
// (total = number of collections fetched). BLUEPRINT §4.
func MirrorCollections(client romClient, cfg *config.Config, rep *Reporter) (written, empty, total int, err error) {
	rep.percent(0)
	rep.phase("Reading collections…")

	collections, cerr := client.GetCollections()
	if cerr != nil {
		return 0, 0, 0, cerr
	}
	total = len(collections)

	sdRoot := sdcardRoot()

	platforms, perr := getMappedPlatforms(client, cfg)
	if perr != nil {
		return 0, 0, total, perr
	}

	// rom_id -> SDCARD-relative path, over mapped platforms only, for ROMs present on card.
	// This indexing pass walks the same per-platform pages as the catalog mirror, so we
	// drive the bar 0..50 across it, then 50..100 across the collection writes below.
	nPlat := len(platforms)
	idPath := map[int]string{}
	for pi2, p := range platforms {
		rep.phase(fmt.Sprintf("Indexing %s (%d/%d)…", platformDisplay(p), pi2+1, nPlat))
		if nPlat > 0 {
			rep.percent((pi2 + 1) * 50 / nPlat)
		}
		page, gerr := client.GetRoms(romm.GetRomsQuery{PlatformIDs: []int{p.ID}})
		if gerr != nil {
			continue
		}
		for i := range page.Items {
			abs := platform.LocalRomPath(cfg, page.Items[i])
			if abs == "" {
				continue
			}
			if _, statErr := os.Stat(abs); statErr != nil {
				continue // only list ROMs whose file (real or stub) exists on card
			}
			idPath[page.Items[i].ID] = strings.TrimPrefix(abs, sdRoot)
		}
	}

	colDir := filepath.Join(platform.RomsDir(), "..", "Collections")
	if mkErr := os.MkdirAll(colDir, 0o755); mkErr != nil {
		return 0, 0, total, mkErr
	}

	for ci, col := range collections {
		rep.phase(fmt.Sprintf("Building collections (%d/%d)…", ci+1, total))
		if total > 0 {
			rep.percent(50 + (ci+1)*50/total)
		}
		var lines []string
		for _, rid := range col.RomIDs {
			if rel, ok := idPath[rid]; ok {
				lines = append(lines, rel)
			}
		}
		if len(lines) == 0 {
			empty++
			continue
		}
		name := sanitizeCollectionName(col.Name)
		if name == "" {
			continue
		}
		if werr := os.WriteFile(filepath.Join(colDir, name+".txt"),
			[]byte(strings.Join(lines, "\n")+"\n"), 0o644); werr != nil {
			return written, empty, total, werr
		}
		written++
	}
	rep.percent(100)
	rep.phase(fmt.Sprintf("Collections updated — %d", written))
	return written, empty, total, nil
}

// ResolveRomID reverses a local ROM path back to its rom_id using the catalog
// index. It reverses directory_mappings (parent folder name -> fs_slug, matching
// relative_path first, then the slug) and then looks up the basename (no ext) in
// by_basename, falling back to the full base name in by_fsname. The index is loaded
// per call.
func ResolveRomID(cfg *config.Config, romPath string) (romID int, ok bool) {
	slug, sok := slugForRomPath(cfg, romPath)
	if !sok {
		return 0, false
	}

	idx, lerr := loadIndex(IndexPath(cfg))
	if lerr != nil {
		return 0, false
	}
	pi, pok := idx.Platforms[slug]
	if !pok {
		return 0, false
	}

	base := filepath.Base(romPath)
	nameNoExt := strings.TrimSuffix(base, filepath.Ext(base))
	if id, found := pi.ByBasename[nameNoExt]; found && id != 0 {
		return id, true
	}
	if id, found := pi.ByFsname[base]; found && id != 0 {
		return id, true
	}
	return 0, false
}

// slugForRomPath reverses directory_mappings: a ROM lives in
// Roms/<relative_path>/<file>, so the parent directory name identifies the
// platform. First pass matches the on-disk folder name (relative_path) — the most
// specific signal; second pass matches the slug itself. Returns the RomM fs_slug
// used by the index (the mapping's Slug override when set, else the map key). Pure
// logic ported from grout's slugForRomPath.
func slugForRomPath(cfg *config.Config, romPath string) (string, bool) {
	if cfg == nil {
		return "", false
	}
	dir := filepath.Base(filepath.Dir(romPath))
	for slug, m := range cfg.DirectoryMappings {
		if m.RelativePath == dir {
			if m.Slug != "" {
				return m.Slug, true
			}
			return slug, true
		}
	}
	for slug, m := range cfg.DirectoryMappings {
		if m.Slug == dir || slug == dir {
			if m.Slug != "" {
				return m.Slug, true
			}
			return slug, true
		}
	}
	return "", false
}

// sanitizeCollectionName makes a collection name safe as a filename, replacing the
// reserved set / \ : * ? " < > | with "-" and trimming surrounding space.
func sanitizeCollectionName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sdcardRoot() string {
	sd := os.Getenv("SDCARD_PATH")
	if sd == "" {
		sd = "/mnt/SDCARD"
	}
	return sd
}

// getMappedPlatforms returns the RomM platforms the user has a directory mapping
// for, fetching the platform list once to learn each fs_slug's id.
func getMappedPlatforms(client romClient, cfg *config.Config) ([]romm.Platform, error) {
	pc, ok := client.(interface {
		GetPlatforms() ([]romm.Platform, error)
	})
	if !ok {
		return nil, errNoPlatforms
	}
	all, err := pc.GetPlatforms()
	if err != nil {
		return nil, err
	}
	var out []romm.Platform
	for _, p := range all {
		if cfg != nil {
			if _, mapped := cfg.DirectoryMappings[p.FsSlug]; mapped {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// errNoPlatforms is returned when the client cannot list platforms.
var errNoPlatforms = errPlatforms("client does not support GetPlatforms")

type errPlatforms string

func (e errPlatforms) Error() string { return string(e) }

// writeIndexAtomic marshals idx and writes it to path via a temp file + rename so a
// reader never sees a partial index. The parent directory is created if missing.
func writeIndexAtomic(path string, idx index) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadIndex reads and parses catalog-index.json.
func loadIndex(path string) (index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return index{}, err
	}
	var idx index
	if err := json.Unmarshal(data, &idx); err != nil {
		return index{}, err
	}
	return idx, nil
}
