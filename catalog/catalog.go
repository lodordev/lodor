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
	".fla": true, ".flash": true, ".mpk": true, ".nv": true,
}

func isSaveExt(p string) bool { return saveExts[strings.ToLower(filepath.Ext(p))] }

// nonGameExts are extensions for files RomM bundles ALONGSIDE games — manuals, videos,
// info/metadata, box art — that its API returns as standalone "rom" entries (the index
// is messy: images/manuals/videos folders get counted as roms). They must never be
// stubbed into Roms/ as launchable games. This is a DENYLIST (not an allowlist): every
// entry is an extension a real ROM NEVER uses, so a real game is never accidentally
// hidden. Saves are filtered separately by saveExts (they belong in Saves/, not Roms/).
var nonGameExts = map[string]bool{
	".txt": true, ".nfo": true, ".xml": true,
	".pdf": true,
	".mp4": true, ".avi": true, ".mkv": true, ".webm": true, ".m4v": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true,
}

// isNonGameAsset reports whether a mirror path is a non-game bundle member (manual /
// video / info / cover) that should be DROPPED from the library rather than stubbed.
// Matches by extension (nonGameExts) OR by RomM's bundle name conventions (-manual /
// -video suffix, or an "_info" metadata file) so an unexpected extension still can't
// masquerade as a game. Never matches a real ROM extension.
func isNonGameAsset(p string) bool {
	if nonGameExts[strings.ToLower(filepath.Ext(p))] {
		return true
	}
	base := strings.ToLower(filepath.Base(p))
	name := strings.TrimSuffix(base, filepath.Ext(base))
	return strings.HasSuffix(name, "-manual") || strings.HasSuffix(name, "-video") ||
		name == "_info" || base == "_info.txt"
}

// romClient is the subset of *romm.Client this package needs, kept as an interface
// so the mirror is testable without a live server.
type romClient interface {
	GetRoms(query romm.GetRomsQuery) (romm.PaginatedRoms, error)
	GetCollections() ([]romm.Collection, error)
	DownloadCover(coverPath string) ([]byte, error)
}

// platformIndex holds the lookup tables for one platform.
type platformIndex struct {
	ByBasename map[string]int    `json:"by_basename"`
	ByFsname   map[string]int    `json:"by_fsname"`
	// ByID maps rom_id -> SDCARD-relative path for every stubbed/present ROM, so
	// --mirror-collections can resolve collection members WITHOUT re-fetching the whole
	// library (that per-platform refetch was the multi-minute "collections hang").
	ByID map[int]string `json:"by_id,omitempty"`
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

// IndexPath returns the absolute path of catalog-index.json inside the host pak's
// working directory (platform.PakDir(), resolved from LODOR_PAK_DIR / the script CWD).
// The engine owns no host pak name.
func IndexPath(cfg *config.Config) string {
	return filepath.Join(platform.PakDir(), "catalog-index.json")
}

// MirrorCatalog stubs every not-yet-downloaded single-file ROM of each mapped
// platform into its Roms/ folder and, while iterating, builds and atomically writes
// the catalog index to IndexPath. Returns the per-action counts. Multi-file ROMs are
// counted and skipped (v1). A ROM whose local path resolves to a save extension is
// skipped; an already-present file is counted as existing; otherwise a 0-byte stub
// is created.
//
// MERGE (C1 §2 — dedup-by-index-adoption): a user's real file whose (marker-
// stripped, case-insensitive) filename exactly matches the ROM's canonical on-disk
// name or raw fs_name — in the adopted folder OR any same-tag sibling — is ADOPTED:
// NO stub is created (no duplicate row), the index by_id takes THEIR path (so
// Collections/Continue list their real file and save-sync attaches to it — the
// adoption magic moment), and the file itself is never renamed, marked, evicted or
// pruned (the manifest never claims it). A user's 0-byte file at a canonical name
// is skipped entirely (never adopted, never stubbed, never downloaded-over).
// Adopted files are returned in the adopted count (also counted as existing —
// the game IS on the card).
func MirrorCatalog(client romClient, cfg *config.Config, rep *Reporter, coverForce bool) (created, existing, skipped, multifile, covers, adopted int, err error) {
	idx := index{Version: 1, Platforms: map[string]platformIndex{}}
	coversOn := cfg.CoversEnabled()

	// Mirror-owned manifest (C2): every path this run CREATES is recorded so the
	// destructive paths (prune/evict/download-fill/uninstall) can gate on real
	// ownership instead of inference. One atomic save at the end of the run.
	man := platform.LoadManifest()
	// resolves is the catalog leg of the ReclaimableStub triple gate: does this
	// on-disk path reverse to a rom_id against the LOCAL index (the previous run's
	// — the new one isn't written yet)?
	resolves := func(p string) bool { _, ok := ResolveRomID(cfg, p); return ok }

	rep.percent(0)
	rep.phase("Reading library…")

	// Self-heal: first-run onboarding writes host/auth/device but no directory_mappings,
	// so getMappedPlatforms would return empty and we would stub nothing. When mappings
	// are absent, auto-generate them from the user's platforms (logged to stderr),
	// persist to config.json, and mutate cfg so the walk below sees them. Existing
	// mappings are left untouched. A generation/persist failure is a config/reachability
	// error (same class as the platforms fetch) -- surface it.
	if merr := ensureDirectoryMappings(client, cfg); merr != nil {
		return 0, 0, 0, 0, 0, 0, merr
	}

	// Mirror only platforms the user has mapped a Roms folder for; others have
	// nowhere to put stubs. We resolve those platforms via the directory_mappings
	// keys directly so no /api/platforms call is needed: every mapping key is an
	// fs_slug, and GetRoms is filtered by platform id — but we don't have the id
	// without the platforms list, so fetch platforms once.
	platforms, perr := getMappedPlatforms(client, cfg)
	if perr != nil {
		return 0, 0, 0, 0, 0, 0, perr
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

		// Don't stub platforms this device can't launch. A mapping can carry a known tag
		// (DS/3DS/PSP) for which NO emulator pak is installed on a Mini Flip — stubbing it
		// fills the library + search with un-launchable games (you could download what you
		// can't play). Skip it BEFORE the network fetch, and self-heal a stale mapping by
		// pruning any 0-byte stubs already on the card. A real download is left untouched.
		if tag, ok := platform.PrimaryTag(p.FsSlug); !ok || !platform.HasEmuPak(tag) {
			pruneUnplayableStubs(cfg, p, man, resolves)
			doneWork += p.RomCount
			rep.percent(mirrorPct(pi2+1, nPlat, doneWork, totalWork))
			continue
		}

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
			pi.ByID = map[int]string{}
		}

		// Manifest: record FOLDER-creation intent BEFORE any stub write (record-
		// intent-then-act: a crash leaves an over-complete manifest, the safe
		// direction — we may "own" a folder we failed to create; removing a
		// nonexistent path later is a no-op). Only a folder that does not exist yet
		// is ours to claim; an existing folder is the user's (or a prior run's own,
		// already recorded).
		if m, ok := cfg.DirectoryMappings[p.FsSlug]; ok && m.RelativePath != "" {
			dir := filepath.Join(sdcardRoot(), "Roms", m.RelativePath)
			if _, serr := os.Stat(dir); os.IsNotExist(serr) {
				man.Record(dir, platform.ManifestFolder, 0)
			}
		}

		// MERGE dedup scan (once per platform): the user's own files — unmarked, so
		// never Lodor's — across the adopted folder and every same-tag sibling,
		// keyed by lower-cased filename for the Tier-1 exact match.
		var userFiles map[string]adoptCandidate
		if !platform.HostShowsStateNatively() && cfg.ResolvedMirrorMode() == config.MirrorModeMerge {
			if tag, ok := platform.PrimaryTag(p.FsSlug); ok {
				adoptedDir := ""
				if m, mok := cfg.DirectoryMappings[p.FsSlug]; mok {
					adoptedDir = m.RelativePath
				}
				userFiles = scanUserFilesForTag(tag, adoptedDir)
			}
		}
		for i := range page.Items {
			rom := page.Items[i]

			// Index every ROM (single- and multi-file) so resolution works for all. Key
			// by the mode-aware on-disk basename (LocalBasename) — the SAME name the stub
			// is written under (disambiguated in non-"own" modes) — so ResolveRomID, which
			// reverses the on-disk path, finds the rom_id. ByFsname keeps the raw server
			// fs_name as a secondary key for any caller resolving by the original name.
			if lb := platform.LocalBasename(cfg, rom); lb != "" {
				pi.ByBasename[lb] = rom.ID
			}
			if rom.FsName != "" {
				pi.ByFsname[rom.FsName] = rom.ID
			}

			// Canonical (unmarked) on-disk path, then drop anything that isn't a
			// launchable game (a save misfiled in Roms/, or a manual/video/info/box-art
			// bundle member RomM returns as a standalone "rom").
			path := platform.LocalRomPath(cfg, rom)
			if path == "" || isSaveExt(path) || isNonGameAsset(path) {
				skipped++
				continue
			}
			// Multi-file (multi-disc) ROMs ARE stubbed: LocalRomPath returns the game's
			// <FsNameNoExt>.m3u path, so a 0-byte .m3u stub drops exactly like a single-file
			// game's. (Marker note: the multi-disc DOWNLOAD path still writes the canonical
			// .m3u — marker-on-m3u is a tracked follow-up; single-file fills in place.)
			if rom.HasMultipleFiles {
				multifile++
			}

			// MERGE Tier-1 dedup: the user's exact-named file wins — adopt, don't stub.
			if userFiles != nil {
				canonName := filepath.Base(path)
				uf, hit := userFiles[strings.ToLower(canonName)]
				if !hit && rom.FsName != "" {
					uf, hit = userFiles[strings.ToLower(rom.FsName)]
				}
				if hit {
					// Heal: a mirror-owned stub for this game (left by an earlier
					// layout/flip) is a redundant duplicate row now that the user's
					// file owns the name — remove it (owned + re-verified 0-byte only).
					for _, mk := range []string{platform.MarkerCloud, platform.MarkerOnDevice} {
						sp := filepath.Join(filepath.Dir(path), mk+canonName)
						if man.OwnsKind(sp, platform.ManifestStub) {
							if fi, serr := os.Stat(sp); serr == nil && !fi.IsDir() && fi.Size() == 0 {
								if os.Remove(sp) == nil {
									man.Forget(sp)
								}
							}
						}
					}
					if uf.size == 0 {
						// The user's own 0-byte file occupies the canonical name: never
						// adopt (nothing to attach to), never stub beside it, never
						// download over it (the V5 gate refuses it too). Skip + log.
						fmt.Fprintf(os.Stderr, "MIRROR skip %s: user 0-byte file occupies canonical name\n", canonName)
						skipped++
						continue
					}
					// Index-adopt: by_id -> THEIR path (Collections/Continue/GM list
					// their real file); by_basename/by_fsname already carry the
					// canonical keys above, so ResolveRomID on their path is free.
					pi.ByID[rom.ID] = strings.TrimPrefix(uf.abs, sdcardRoot())
					adopted++
					existing++
					// Box art for their file: additive-only (force NEVER applies here —
					// a --full refresh must not overwrite the user's own art; FetchAndSave
					// without force skips any existing cover).
					if coversOn {
						if cp := rom.CoverPath(); cp != "" {
							coverDone++
							if out, _ := cover.FetchAndSave(client, cp, uf.abs, false); out == cover.OutcomeSaved {
								covers++
								man.Record(cover.MediaPath(uf.abs), platform.ManifestCover, rom.ID)
							}
						}
					}
					continue
				}
			}

			// Reconcile to exactly one on-disk presence under the correct state marker:
			// a 0-byte stub becomes "[^] " (in the cloud), a real downloaded file becomes
			// "[v] " (on device), migrating saves + cover in lockstep if the marker flips.
			// finalPath is the actual MARKED name on the card; didCreate is true only for a
			// brand-new stub (preserving the created/existing contract counts).
			var finalPath string
			var didCreate bool
			if platform.HostShowsStateNatively() {
				// LodorOS forked launcher dims by file size -> keep canonical names, no markers.
				finalPath, didCreate = platform.ReconcileCanonicalPresence(cfg, rom, path)
			} else if cfg.ResolvedMirrorMode() == config.MirrorModeMerge {
				// MERGE: the V4 belt — a file at the bare canonical name that the
				// manifest doesn't own is the USER's; reconcile leaves it (and its
				// saves) untouched and creates nothing (dedup above should have
				// adopted it; a case-drift miss lands here and is skipped+logged).
				finalPath, didCreate = platform.ReconcileMarkedPresenceGuarded(cfg, rom, path, man.Owns)
				if finalPath == "" {
					fmt.Fprintf(os.Stderr, "MIRROR skip %s: user file occupies canonical name (not adopted — name/case drift)\n", filepath.Base(path))
				}
			} else {
				finalPath, didCreate = platform.ReconcileMarkedPresence(cfg, rom, path)
			}
			if finalPath == "" {
				skipped++
				continue
			}
			// Manifest: the reconciled presence is mirror-owned — a 0-byte stub as
			// "stub", real bytes as "download" (in own/separate modes the mirror
			// folders are the engine's by construction, so a real file at a mirror
			// name is a download/library file either way; in merge mode only files
			// at MARKED names ever reach here, and a ✓-named real file that
			// resolves is a Lodor download — user files never carry markers).
			if fi, serr := os.Stat(finalPath); serr == nil {
				kind := platform.ManifestStub
				if fi.Size() > 0 {
					kind = platform.ManifestDownload
				}
				man.Record(finalPath, kind, rom.ID)
			}
			// rom_id -> SDCARD-relative MARKED path so --mirror-collections lists the real
			// on-disk name the launcher can open.
			pi.ByID[rom.ID] = strings.TrimPrefix(finalPath, sdcardRoot())
			if didCreate {
				created++
			} else {
				existing++
			}

			// Box art: fetch this rom's cover into <rom dir>/.media/<marked stem>.png
			// (NextUI convention) ANCHORED at the actual on-disk name so the launcher finds
			// it. WHOLE-library (stubs included). Graceful + non-fatal: a coverless rom is
			// skipped, an already-present cover is skipped, any fetch/decode error is counted
			// but NEVER aborts the mirror; progress flows through the side-channels only.
			if coversOn {
				if cp := rom.CoverPath(); cp != "" {
					coverDone++
					if coverTotal > 0 {
						rep.phase(fmt.Sprintf("Fetching cover %d/%d…", coverDone, coverTotal))
					}
					if out, _ := cover.FetchAndSave(client, cp, finalPath, coverForce); out == cover.OutcomeSaved {
						covers++
						// Manifest: our box art is removable at uninstall (kind=cover).
						man.Record(cover.MediaPath(finalPath), platform.ManifestCover, rom.ID)
					}
				}
			}
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
		return created, existing, skipped, multifile, covers, adopted, werr
	}
	// Persist the manifest (one atomic, fsync'd write per run). Recorded under the
	// mode this pass mirrored in, so a later mode flip is detectable. A failed
	// manifest write is non-fatal for the mirror itself but degrades the NEXT run's
	// pruning toward "delete nothing" — the safe direction — so log it honestly.
	man.SetMode(cfg.ResolvedMirrorMode())
	if merr := man.Save(); merr != nil {
		fmt.Fprintf(os.Stderr, "MANIFEST save failed: %v (pruning degrades to no-op next run)\n", merr)
	}
	rep.percent(100)
	if coversOn {
		rep.phase(fmt.Sprintf("Library updated — %d new, %d covers", created, covers))
	} else {
		rep.phase(fmt.Sprintf("Library updated — %d new", created))
	}
	return created, existing, skipped, multifile, covers, adopted, nil
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
// (total = number of collections fetched), plus cont — the entry count of the
// cross-device "0) Continue" collection written in the same pass (see continue.go;
// 0 = empty feed, no Continue file). BLUEPRINT §4 + task #37.
func MirrorCollections(client romClient, cfg *config.Config, rep *Reporter) (written, empty, total, cont int, err error) {
	rep.percent(0)
	rep.phase("Reading collections…")

	collections, cerr := client.GetCollections()
	if cerr != nil {
		return 0, 0, 0, 0, cerr
	}
	total = len(collections)

	// rom_id -> SDCARD-relative path. FAST PATH: reuse the by_id map the preceding
	// --mirror-catalog already wrote to the index. This avoids re-fetching the WHOLE
	// library here (35 platforms / thousands of roms + an os.Stat each) — that refetch was
	// the multi-minute "Updating collections" hang. The index is local: near-instant.
	idPath := LoadIndexIDPath(cfg)
	// FALLBACK (rare): no usable index yet (collections run before any catalog mirror).
	// Do the live per-platform walk so collections still work — just slowly, this once.
	if len(idPath) == 0 {
		sdRoot := sdcardRoot()
		platforms, perr := getMappedPlatforms(client, cfg)
		if perr != nil {
			return 0, 0, total, 0, perr
		}
		nPlat := len(platforms)
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
	}

	colDir := ContinueDir()
	if mkErr := os.MkdirAll(colDir, 0o755); mkErr != nil {
		return 0, 0, total, 0, mkErr
	}

	// Ownership: the mirror-owned manifest (C2 — formalizes the STEP 0b
	// collections ledger into the ONE mechanism). A legacy card whose ownership
	// still lives in collections-owned.txt has those names imported once, so its
	// prune continuity survives the upgrade; the ledger file is retired after the
	// first successful manifest save.
	man := platform.LoadManifest()
	hasCollectionEntries := false
	for _, e := range man.Entries {
		if e.Kind == platform.ManifestCollection || e.Kind == platform.ManifestContinue {
			hasCollectionEntries = true
			break
		}
	}
	if !hasCollectionEntries {
		for name := range readOwnedCollections() {
			man.Record(filepath.Join(colDir, name), platform.ManifestCollection, 0)
		}
	}

	kept := map[string]bool{}
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
			return written, empty, total, 0, werr
		}
		kept[name+".txt"] = true
		man.Record(filepath.Join(colDir, name+".txt"), platform.ManifestCollection, 0)
		written++
	}

	// Cross-device "Continue" collection (task #37): written in the SAME pass, BEFORE
	// the prune, and marked kept — otherwise the prune below (correctly) removes any
	// collection file the server didn't produce. An empty feed writes nothing, so the
	// prune clears a stale Continue from a previous run. Runs after the collection
	// loop so a RomM collection that happens to share the name is overwritten.
	rep.phase("Updating Continue…")
	preContinue := map[string]bool{}
	for n := range kept {
		preContinue[n] = true
	}
	cont = mirrorContinue(client, cfg, idPath, colDir, kept)
	for n := range kept {
		if !preContinue[n] { // the Continue file mirrorContinue added this pass
			man.Record(filepath.Join(colDir, n), platform.ManifestContinue, 0)
		}
	}

	// Prune stale collection files — MANIFEST-SCOPED (C1 design audit V2; formalizes
	// STEP 0b). Collections/ belongs to the USER on NextUI: their curated .txt lists
	// plus map.txt, NextUI's native display-alias file. The pre-0b prune removed ANY
	// *.txt not written this pass — deleting user collections and map.txt on every
	// refresh. A file is only removable when the mirror-owned manifest claims it
	// (kind=collection/continue). No manifest = prune nothing (fail-safe: stale Lodor
	// collections may linger one refresh; user data is never at risk). map.txt is
	// hard-excluded regardless of what the manifest says.
	if entries, derr := os.ReadDir(colDir); derr == nil {
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".txt") || kept[n] {
				continue
			}
			fp := filepath.Join(colDir, n)
			if strings.EqualFold(n, "map.txt") ||
				(!man.OwnsKind(fp, platform.ManifestCollection) && !man.OwnsKind(fp, platform.ManifestContinue)) {
				continue // user data (or unknowable ownership) — never ours to delete
			}
			if os.Remove(fp) == nil {
				man.Forget(fp)
			}
		}
	}
	// Persist ownership = exactly what exists after this pass. Best-effort: a failed
	// manifest write leaves the OLD manifest (or none) — pruning degrades toward
	// "nothing", never toward "user files". The legacy STEP 0b ledger file is retired
	// only after its content is safely inside a saved manifest.
	if merr := man.Save(); merr == nil {
		_ = os.Remove(collectionsLedgerPath())
	} else {
		fmt.Fprintf(os.Stderr, "MANIFEST save failed: %v (collections pruning degrades to no-op next run)\n", merr)
	}
	rep.percent(100)
	rep.phase(fmt.Sprintf("Collections updated — %d", written))
	return written, empty, total, cont, nil
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

	// Strip any leading cloud/on-device state marker the launcher kept on the on-disk
	// filename: the index is keyed by the unmarked, server-matched canonical basename,
	// so "[^] Game (USA).gba" and "[v] Game (USA).gba" must both reverse to the same
	// rom_id as the bare "Game (USA)" key (BLUEPRINT §A). The local save path keeps the
	// marked name; only resolution-to-RomM strips it.
	base := platform.StripLeadingMarker(filepath.Base(romPath))
	ext := filepath.Ext(base)
	nameNoExt := strings.TrimSuffix(base, ext)

	// Primary lookups: the mode-aware index keys as the mirror wrote them.
	if id, found := pi.ByBasename[nameNoExt]; found && id != 0 {
		return id, true
	}
	if id, found := pi.ByFsname[base]; found && id != 0 {
		return id, true
	}
	// Multi-disc: the on-disk file is "<name>.m3u" but a multi-file ROM's fs_name is
	// the extension-less game FOLDER name, so the full-base ByFsname lookup above can
	// never hit — try the stem.
	if id, found := pi.ByFsname[nameNoExt]; found && id != 0 {
		return id, true
	}

	// CROSS-MODE DECORATION FALLBACKS (workstream A1). A coexist-mode flip re-keys the
	// index (own keys "Game"; separate/merge keys "Game (RomM)") but does NOT rename
	// files already on the card, so a legacy on-disk name must still resolve against
	// the re-keyed index in BOTH directions — otherwise every pre-flip download loses
	// its downloads/saves/evict resolution (observed on the Smart Pro card 2026-07-02:
	// a merge-mode re-mirror left "✓ Game.gba" on disk with only "Game (RomM)" keys
	// fresh in by_basename; single-file names survived via by_fsname, multi-disc and
	// own-flip names did not).
	if platform.HasRomMTag(nameNoExt) {
		// on-disk "(RomM)"-tagged, index keyed canonical (own mode now).
		stem := platform.StripRomMTag(nameNoExt)
		if id, found := pi.ByBasename[stem]; found && id != 0 {
			return id, true
		}
		if id, found := pi.ByFsname[stem+ext]; found && id != 0 {
			return id, true
		}
		if id, found := pi.ByFsname[stem]; found && id != 0 {
			return id, true
		}
	} else {
		// on-disk canonical, index keyed "(RomM)"-tagged (separate/merge mode now).
		if id, found := pi.ByBasename[nameNoExt+platform.RomMTag()]; found && id != 0 {
			return id, true
		}
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
	// Fallback: the folder is not in directory_mappings (a platform discovered by
	// installed pak, e.g. an NDS.pak added after onboarding). Derive the fs_slug from the
	// folder's trailing "(TAG)" — the same convention the mirror writes — so download and
	// save resolution work for capability-discovered platforms too.
	if tag := tagFromFolderName(dir); tag != "" {
		if slug, ok := platform.FsSlugForTag(tag); ok {
			return slug, true
		}
	}
	return "", false
}

// tagFromFolderName extracts the trailing "(TAG)" from a Roms folder name, e.g.
// "Nintendo DS (NDS)" -> "NDS". Returns "" when absent.
func tagFromFolderName(dir string) string {
	o := strings.LastIndex(dir, "(")
	c := strings.LastIndex(dir, ")")
	if o >= 0 && c > o {
		return dir[o+1 : c]
	}
	return ""
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

// adoptCandidate is one user file the merge dedup may adopt: its absolute path and
// size (0-byte user files are skipped, never adopted or stubbed over).
type adoptCandidate struct {
	abs  string
	size int64
}

// scanUserFilesForTag maps lower-cased filename -> the user's file for every
// UNMARKED top-level file in the adopted folder and every same-tag sibling folder
// (C1 §2: dedup checks the adopted folder AND siblings). Marked (✘/✓/legacy) names
// are Lodor's own artifacts and flow to Reconcile instead. On a duplicate filename
// the ADOPTED folder wins (it is scanned first); siblings keep first-seen order
// (deterministic: listRomsFolders sorts).
func scanUserFilesForTag(tag, adoptedDir string) map[string]adoptCandidate {
	out := map[string]adoptCandidate{}
	dirs := []string{}
	if adoptedDir != "" {
		dirs = append(dirs, adoptedDir)
	}
	for _, d := range listRomsFolders() {
		if d == adoptedDir {
			continue
		}
		if strings.EqualFold(tagFromFolderName(d), tag) || strings.EqualFold(d, tag) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		entries, err := os.ReadDir(filepath.Join(sdcardRoot(), "Roms", d))
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || strings.HasPrefix(n, ".") || platform.HasLeadingMarker(n) {
				continue
			}
			key := strings.ToLower(n)
			if _, dup := out[key]; dup {
				continue // first-seen (adopted-folder-first) wins
			}
			fi, ferr := e.Info()
			if ferr != nil {
				continue
			}
			out[key] = adoptCandidate{
				abs:  filepath.Join(sdcardRoot(), "Roms", d, n),
				size: fi.Size(),
			}
		}
	}
	return out
}

// pruneUnplayableStubs removes MIRROR-OWNED 0-byte stub files from a mapped
// platform's Roms folder when that platform has no installed emulator pak —
// self-healing a config that still maps a system the device can't launch (DS/3DS/
// PSP on a Mini Flip).
//
// MANIFEST-SCOPED (C1 design audit V1): the old prune deleted EVERY 0-byte file in
// the mapped folder and RemoveAll'd the folder when only .media remained — in
// merge mode the mapped folder is the USER's adopted folder, so that deleted their
// placeholder files and could remove their folder including their own box art. Now
// a file goes only when the manifest owns it as a stub (or the conservative
// triple-gate re-claims it on a manifest-less legacy card), the folder goes only
// when the manifest owns the FOLDER and our own covers are all that's left, and
// os.RemoveAll is gone entirely (os.Remove fails on non-empty — structurally
// incapable of taking user files with it). Real (non-zero) files are never
// touched, manifest-owned or not.
func pruneUnplayableStubs(cfg *config.Config, p romm.Platform, man *platform.Manifest, resolves func(string) bool) {
	m, ok := cfg.DirectoryMappings[p.FsSlug]
	if !ok || m.RelativePath == "" {
		return
	}
	dir := filepath.Join(sdcardRoot(), "Roms", m.RelativePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	removed, kept := 0, 0
	for _, e := range entries {
		if e.IsDir() {
			continue // .media handled below; other subfolders never touched
		}
		info, ierr := e.Info()
		if ierr != nil || info.Size() != 0 {
			continue // real bytes: never prunable, whoever owns them
		}
		fp := filepath.Join(dir, e.Name())
		if !man.OwnsKind(fp, platform.ManifestStub) && !platform.ReclaimableStub(fp, resolves) {
			kept++ // not ours to delete (user placeholder / unknowable) — leave it
			continue
		}
		if os.Remove(fp) == nil {
			man.Forget(fp)
			removed++
		}
	}
	if removed > 0 {
		fmt.Fprintf(os.Stderr, "MIRROR prune %s: removed %d un-launchable stub(s) (no Emus pak)\n", p.FsSlug, removed)
	}
	if kept > 0 {
		fmt.Fprintf(os.Stderr, "MIRROR prune %s: kept %d 0-byte file(s) not owned by the mirror\n", p.FsSlug, kept)
	}

	// Folder teardown — only when the MIRROR created the folder. Our own covers in
	// .media are deleted (they're manifest-owned), then .media and the folder are
	// removed with plain os.Remove: if the user put ANYTHING here, the remove
	// fails silently and everything stays.
	if !man.OwnsKind(dir, platform.ManifestFolder) {
		return
	}
	media := filepath.Join(dir, ".media")
	if mediaEntries, merr := os.ReadDir(media); merr == nil {
		for _, e := range mediaEntries {
			cp := filepath.Join(media, e.Name())
			if !e.IsDir() && man.OwnsKind(cp, platform.ManifestCover) {
				if os.Remove(cp) == nil {
					man.Forget(cp)
				}
			}
		}
		_ = os.Remove(media) // only falls if empty
	}
	if os.Remove(dir) == nil { // only falls if empty
		man.Forget(dir)
	}
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
		mapped := false
		if cfg != nil {
			_, mapped = cfg.DirectoryMappings[p.FsSlug]
		}
		// Also discover platforms the device can actually launch (a matching emulator
		// pak is installed) even when onboarding-baked directory_mappings predate the
		// pak — otherwise e.g. an NDS.pak added later never gets its library stubbed.
		capable := false
		if tag, ok := platform.PrimaryTag(p.FsSlug); ok && platform.HasEmuPak(tag) {
			capable = true
		}
		if mapped || capable {
			out = append(out, p)
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

// ReconcileAfterDownload flips ONE ROM's on-disk state marker to match the bytes now
// present on the card, in the POST-LAUNCH window (the game has exited, so the rename is
// safe — renaming during the download→launch sequence would pull the file out from under
// the launcher, which is exactly why decision #69 reverted relocate). It is the per-game
// equivalent of the bulk marker reconcile MirrorCatalog runs: a freshly-downloaded game
// whose 0-byte stub was filled IN PLACE under the cloud marker ("✘ Game.gb") is promoted
// to the on-device marker ("✓ Game.gb"), carrying its save + cover with the rename via
// platform.ReconcileMarkedPresence so nothing orphans. A game still a 0-byte stub stays
// "✘". Net behavior: after you download a game and play it once, its ✘ becomes ✓.
//
// Offline by design — it needs NO network. The platform fs_slug (which save folders to
// migrate) is derived from the on-disk path with the same directory_mappings reversal
// ResolveRomID uses; the rom_id (to keep the catalog index's by_id path current so
// --mirror-collections lists a path that exists) is a best-effort local index lookup,
// silently skipped on any failure since the next Refresh rebuilds the index.
//
// Returns whether the on-disk marker actually flipped.
func ReconcileAfterDownload(cfg *config.Config, romPath string) (flipped bool) {
	// LodorOS keeps canonical names (no markers), so a downloaded game already
	// carries its final name in place and there is no ✘->✓ flip to perform.
	if platform.HostShowsStateNatively() {
		return false
	}
	if cfg == nil || romPath == "" {
		return false
	}
	slug, ok := slugForRomPath(cfg, romPath)
	if !ok {
		return false
	}
	dir := filepath.Dir(romPath)
	canonBase := platform.StripLeadingMarker(filepath.Base(romPath))
	unmarked := filepath.Join(dir, canonBase)

	// ReconcileMarkedPresence only reads rom.PlatformFsSlug (to find this game's save
	// folders for the lockstep rename), so a minimal rom carrying just the slug is enough
	// and keeps the reconcile fully offline.
	rom := romm.Rom{PlatformFsSlug: slug}
	final, _ := platform.ReconcileMarkedPresence(cfg, rom, unmarked)
	if final == "" {
		return false
	}
	flipped = filepath.Base(final) != filepath.Base(romPath)

	if flipped {
		updateIndexByID(cfg, slug, final)
	}
	return flipped
}

// updateIndexByID best-effort patches the catalog index's by_id path for the ROM now on
// disk at finalAbs so --mirror-collections emits a path that exists after a marker flip.
// Any failure is silent: a stale index entry is harmless and the next library Refresh
// rebuilds it.
func updateIndexByID(cfg *config.Config, slug, finalAbs string) {
	id, ok := ResolveRomID(cfg, finalAbs)
	if !ok || id == 0 {
		return
	}
	idx, err := loadIndex(IndexPath(cfg))
	if err != nil {
		return
	}
	pi, pok := idx.Platforms[slug]
	if !pok || pi.ByID == nil {
		return
	}
	pi.ByID[id] = strings.TrimPrefix(finalAbs, sdcardRoot())
	idx.Platforms[slug] = pi
	_ = writeIndexAtomic(IndexPath(cfg), idx)
}
