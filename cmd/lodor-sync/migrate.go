package main

// Coexist mode-flip migration (C2 §6 — PROMPT-ONLY, and the field-mess cleanup).
//
// Runs at the start of --mirror-catalog, BEFORE the mirror walk, and only when the
// user has EXPLICITLY chosen a mode (the pak's toggle/consent prompt writes
// mirror_mode; a defaulted mode never migrates — a build upgrade must not silently
// restructure a card):
//
//	explicit MERGE   and the card isn't merge-shaped yet  -> migrate to merge
//	explicit SEPARATE and the manifest says merge          -> migrate to separate
//
// MERGE migration = regenerate mappings (adopt-by-tag), then NORMALIZE every
// mirror-owned (or triple-gate reclaimable) "(RomM)"-tagged or stranded entry into
// the adopted folder under the canonical merge name, carrying saves + cover. The
// FIELD MESS this cleans (Smart Pro 2026-07): "(RomM)"-twin duplicates beside the
// user's (possibly ✓-renamed-by-old-builds) real file, each with its own save
// lineage. Twin handling, in strict order:
//
//	1. PUSH any local save content for BOTH twins through the verified upload
//	   funnel (sync.PushSaveFile → uploadVerified: dedup, verify, never a blind
//	   2xx) so no lineage can be lost — a failed/skipped push DEFERS that twin
//	   (nothing is removed; the scan re-runs next mirror).
//	2. If the surviving file has NO local save and the twin does, rename the
//	   twin's save artifacts to the survivor's basename (instant continuity).
//	3. Remove ONLY a manifest-owned/triple-gated 0-byte stub twin; a REAL twin
//	   download moves (kept) unless the target name is taken — never deleted,
//	   never a user file.
//
// Idempotent: the merge trigger re-scans for "(RomM)"-tagged entries every mirror
// run, so a deferred twin is retried; a clean card is a cheap no-op scan.
// SEPARATE migration is the reverse move (mirror-owned entries out of the user's
// folders into fresh "… RomM (TAG)" ones) — no twin logic, nothing deleted.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lodor/catalog"
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
	"lodor/sync"
)

// migrateMirrorLayoutIfNeeded is the --mirror-catalog pre-pass. Never returns an
// error: migration is best-effort and defers anything it can't do safely; the
// mirror itself proceeds regardless.
func migrateMirrorLayoutIfNeeded(client *romm.Client, cfg *config.Config) {
	if cfg == nil || platform.HostShowsStateNatively() {
		return // LodorOS: own-mode card-is-the-library; no coexist layout to migrate
	}
	target, explicit := cfg.ExplicitMirrorMode()
	if !explicit {
		return // prompt-only: a defaulted mode never restructures a card
	}
	if len(cfg.DirectoryMappings) == 0 {
		return // fresh card: plain generation (ensureDirectoryMappings) owns it
	}
	man := platform.LoadManifest()
	switch target {
	case config.MirrorModeMerge:
		if man.Mode != config.MirrorModeMerge || cardHasRomMTaggedEntries(cfg) {
			migrateMirrorLayout(client, cfg, man, target)
		}
	case config.MirrorModeSeparate:
		if man.Mode == config.MirrorModeMerge {
			migrateMirrorLayout(client, cfg, man, target)
		}
	}
}

// cardHasRomMTaggedEntries reports whether any mapped folder still carries a
// " (RomM)"-tagged file — the separate-naming residue merge must normalize. Cheap:
// one ReadDir per mapped folder.
func cardHasRomMTaggedEntries(cfg *config.Config) bool {
	for _, m := range cfg.DirectoryMappings {
		if m.RelativePath == "" {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(sdRoot(), "Roms", m.RelativePath))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			stem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			if platform.HasRomMTag(stem) {
				return true
			}
		}
	}
	return false
}

// migrateMirrorLayout regenerates the directory mappings for the target mode and
// normalizes every movable mirror-owned entry into its target folder + name.
func migrateMirrorLayout(client *romm.Client, cfg *config.Config, man *platform.Manifest, target string) {
	fmt.Fprintf(os.Stderr, "MIGRATE start target=%s\n", target)

	oldMappings := cfg.DirectoryMappings
	newMappings, generated, _, gerr := catalog.GenerateDirectoryMappings(client, target)
	if gerr != nil || generated == 0 {
		fmt.Fprintf(os.Stderr, "MIGRATE abort: mapping regeneration failed (offline?) — card unchanged\n")
		return
	}
	if werr := config.WriteDirectoryMappings(newMappings); werr != nil {
		fmt.Fprintf(os.Stderr, "MIGRATE abort: could not persist mappings — card unchanged\n")
		return
	}
	cfg.DirectoryMappings = newMappings

	resolves := func(p string) bool { _, ok := catalog.ResolveRomID(cfg, p); return ok }
	moved, twinsRemoved, deferred := 0, 0, 0

	// Per platform: scan the union of the old and new folders for movable entries.
	for slug, nm := range newMappings {
		newDir := filepath.Join(sdRoot(), "Roms", nm.RelativePath)
		dirs := []string{newDir}
		if om, ok := oldMappings[slug]; ok && om.RelativePath != "" && om.RelativePath != nm.RelativePath {
			dirs = append(dirs, filepath.Join(sdRoot(), "Roms", om.RelativePath))
		}
		for _, dir := range dirs {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				m, t, d := migrateEntry(client, cfg, man, resolves, slug, filepath.Join(dir, e.Name()), newDir, target)
				moved, twinsRemoved, deferred = moved+m, twinsRemoved+t, deferred+d
			}
		}
		// Old separate-layout folder teardown: only a folder the manifest owns (or
		// the empty " RomM (" wart shape), only via plain os.Remove (falls on
		// anything left inside — structurally incapable of taking user files).
		if om, ok := oldMappings[slug]; ok && om.RelativePath != nm.RelativePath &&
			strings.Contains(om.RelativePath, " RomM (") {
			oldDir := filepath.Join(sdRoot(), "Roms", om.RelativePath)
			media := filepath.Join(oldDir, ".media")
			if mediaEntries, merr := os.ReadDir(media); merr == nil {
				for _, e := range mediaEntries {
					cp := filepath.Join(media, e.Name())
					if !e.IsDir() && man.OwnsKind(cp, platform.ManifestCover) {
						if os.Remove(cp) == nil {
							man.Forget(cp)
						}
					}
				}
				_ = os.Remove(media)
			}
			if os.Remove(oldDir) == nil {
				man.Forget(oldDir)
			}
		}
	}

	man.SetMode(target)
	if serr := man.Save(); serr != nil {
		fmt.Fprintf(os.Stderr, "MANIFEST save failed after migrate: %v\n", serr)
	}
	fmt.Fprintf(os.Stderr, "MIGRATE done target=%s moved=%d twins_removed=%d deferred=%d\n",
		target, moved, twinsRemoved, deferred)
}

// migrateEntry normalizes ONE candidate file. Returns (moved, twinRemoved,
// deferred) 0/1 counts. Anything not provably the mirror's is left byte-identical.
func migrateEntry(client *romm.Client, cfg *config.Config, man *platform.Manifest,
	resolves func(string) bool, slug, path, targetDir, target string) (moved, twinRemoved, deferred int) {

	base := filepath.Base(path)
	marked := platform.HasLeadingMarker(base)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	tagged := platform.HasRomMTag(platform.StripLeadingMarker(stem))
	if !marked && !tagged {
		return 0, 0, 0 // an unmarked, untagged file is the user's — never a candidate
	}

	fi, serr := os.Stat(path)
	if serr != nil || fi.IsDir() {
		return 0, 0, 0
	}
	isStub := fi.Size() == 0

	// Ownership: manifest, or (stubs only) the conservative triple gate. A real
	// file the manifest doesn't own is NEVER moved or removed.
	owned := man.Owns(path)
	if !owned && (!isStub || !platform.ReclaimableStub(path, resolves)) {
		return 0, 0, 0
	}

	// Canonical identity + the target on-disk name for the new mode.
	canonStem := platform.StripRomMTag(platform.StripLeadingMarker(stem))
	ext := filepath.Ext(base)
	canonName := canonStem + ext
	marker := platform.MarkerCloud
	if !isStub {
		marker = platform.MarkerOnDevice
	}
	targetStem := canonStem
	if target == config.MirrorModeSeparate {
		targetStem = canonStem + platform.RomMTag()
	}
	targetName := marker + targetStem + ext
	targetPath := filepath.Join(targetDir, targetName)
	if targetPath == path {
		return 0, 0, 0 // already normalized
	}

	// TWIN (merge only): a REAL file already holds this game's canonical identity
	// in the target folder — the user's own file at the bare canonical name, a
	// user file an OLD build ✓-renamed (the Smart Pro field shape), or our own ✓
	// download. Any of these makes our "(RomM)"-tagged duplicate redundant.
	if target == config.MirrorModeMerge && targetStem == canonStem {
		for _, survivorBase := range []string{canonName, platform.MarkerOnDevice + canonName} {
			if tfi, terr := os.Stat(filepath.Join(targetDir, survivorBase)); terr == nil && !tfi.IsDir() && tfi.Size() > 0 {
				return migrateTwin(client, cfg, man, slug, path, base, targetDir, survivorBase, isStub)
			}
		}
	}

	// Plain MOVE. A taken target name defers (never overwrite anything).
	if _, terr := os.Stat(targetPath); terr == nil {
		if isStub {
			// The normalized stub already exists — ours is a redundant duplicate row.
			if os.Remove(path) == nil {
				man.Forget(path)
				platform.RenameSaveArtifacts(slug, base, targetName) // orphan heal, best-effort
				return 0, 1, 0
			}
			return 0, 0, 1
		}
		fmt.Fprintf(os.Stderr, "MIGRATE defer %s: target name taken\n", base)
		return 0, 0, 1
	}
	// Multi-disc: move the owned disc folder (canonical stem) with its .m3u so the
	// m3u's relative "…/disc" lines keep resolving.
	if strings.EqualFold(ext, ".m3u") && !isStub {
		discDir := filepath.Join(filepath.Dir(path), canonStem)
		if dfi, derr := os.Stat(discDir); derr == nil && dfi.IsDir() && man.Owns(discDir) {
			targetDisc := filepath.Join(targetDir, canonStem)
			if _, terr := os.Stat(targetDisc); os.IsNotExist(terr) {
				if os.Rename(discDir, targetDisc) == nil {
					man.RenamePath(discDir, targetDisc)
				}
			}
		}
	}
	if merr := os.MkdirAll(targetDir, 0o755); merr != nil {
		return 0, 0, 1
	}
	if rerr := os.Rename(path, targetPath); rerr != nil {
		fmt.Fprintf(os.Stderr, "MIGRATE defer %s: rename failed\n", base)
		return 0, 0, 1
	}
	man.RenamePath(path, targetPath)
	platform.RenameSaveArtifacts(slug, base, targetName)
	moveCover(filepath.Dir(path), targetDir, stem, marker+targetStem, man)
	return 1, 0, 0
}

// migrateTwin handles OUR "(RomM)"-tagged entry when a REAL file (survivorBase)
// already owns this game's canonical identity in the target folder — the user's
// file, a ✓-renamed user file, or our own download (the Smart Pro pair shape).
// Push-before-remove, strictly.
func migrateTwin(client *romm.Client, cfg *config.Config, man *platform.Manifest,
	slug, path, base, targetDir, survivorBase string, isStub bool) (moved, twinRemoved, deferred int) {

	// 1. Push both lineages through the verified funnel. The twin's push GATES its
	// removal; the survivor's push is best-effort lineage merging (its file stays
	// on card either way, and the pre-launch A2 lineage logic protects it).
	twinSaves := localSavesFor(slug, base)
	survivorSaves := localSavesFor(slug, survivorBase)
	if cfg.ActiveHost().DeviceID == "" && len(twinSaves) > 0 {
		fmt.Fprintf(os.Stderr, "MIGRATE defer %s: no device registered — twin save can't be pushed\n", base)
		return 0, 0, 1
	}
	for _, sp := range twinSaves {
		r := sync.PushSaveFile(client, cfg, path, sp, "")
		if !pushOutcomeSafe(r.Outcome) {
			fmt.Fprintf(os.Stderr, "MIGRATE defer %s: twin save push did not land (%s)\n", base, stuckReason(r))
			return 0, 0, 1
		}
	}
	for _, sp := range survivorSaves {
		_ = sync.PushSaveFile(client, cfg, filepath.Join(targetDir, survivorBase), sp, "")
	}

	// 2. Save continuity: survivor has no save, twin does -> the twin's saves take
	// the survivor's name (offline continuity the moment the twin disappears).
	if len(survivorSaves) == 0 && len(twinSaves) > 0 {
		platform.RenameSaveArtifacts(slug, base, survivorBase)
	}

	// 3. Remove the twin — STUBS ONLY here; a real twin download is a second copy
	// of the game and is kept (moved by the caller's plain-move path only when the
	// target name is free; it can never overwrite or replace the user's file).
	if !isStub {
		fmt.Fprintf(os.Stderr, "MIGRATE keep %s: real download twin kept (user file wins the name)\n", base)
		return 0, 0, 0
	}
	if os.Remove(path) != nil {
		return 0, 0, 1
	}
	man.Forget(path)
	// Our stub's cover in this folder's .media is manifest-owned litter now.
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	media := filepath.Join(filepath.Dir(path), ".media")
	for _, ext := range []string{".png", ".jpg", ".jpeg"} {
		cp := filepath.Join(media, stem+ext)
		if man.Owns(cp) {
			if os.Remove(cp) == nil {
				man.Forget(cp)
			}
		}
	}
	return 0, 1, 0
}

// localSavesFor lists the existing save files named for a ROM basename across the
// slug's save folders ("<base>.sav" — the MinUI/NextUI save shape).
func localSavesFor(slug, romBase string) []string {
	var out []string
	for _, folder := range platform.EmulatorFoldersForFSSlug(slug) {
		p := filepath.Join(platform.SavesDir(), folder, romBase+".sav")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Size() > 0 {
			out = append(out, p)
		}
	}
	return out
}

// pushOutcomeSafe reports whether a push outcome proves the save content is safe
// server-side (or there was nothing to protect): only then may its twin be removed.
func pushOutcomeSafe(o sync.PushOutcome) bool {
	switch o {
	case sync.OutcomePushed, sync.OutcomeAlreadyOnServer, sync.OutcomeEmptyLocalSave:
		return true
	}
	return false
}

// moveCover relocates a game's box art between folders' .media dirs across the
// common image extensions, following the stem rename. Best-effort; manifest kept
// in lockstep when the cover was ours.
func moveCover(oldDir, newDir, oldStem, newStem string, man *platform.Manifest) {
	oldMedia := filepath.Join(oldDir, ".media")
	newMedia := filepath.Join(newDir, ".media")
	for _, ext := range []string{".png", ".jpg", ".jpeg"} {
		src := filepath.Join(oldMedia, oldStem+ext)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.MkdirAll(newMedia, 0o755); err != nil {
			return
		}
		dst := filepath.Join(newMedia, newStem+ext)
		if _, err := os.Stat(dst); err == nil {
			continue // never overwrite existing art
		}
		if os.Rename(src, dst) == nil && man.Owns(src) {
			man.RenamePath(src, dst)
		}
	}
}
