package main

// Multi-disc completion modes (lodor#7, disc-1-first hybrid). The launch path
// (--download) fetches only the FIRST missing disc of a multi-disc game; these
// modes are how the rest of the set arrives:
//
//   --check-rom <path>        OFFLINE completeness gate for the lane hooks: is this
//                             ROM fully present on the card? Filesystem only — no
//                             config, host, network, or device (runs pre-config,
//                             like --check-bios, so it works in any pairing state).
//   --fetch-next-disc <rom>   fetch the NEXT missing disc (m3u order) — the hooks'
//                             pre-launch re-trigger for a populated-but-incomplete
//                             .m3u (the state the 0-byte-stub gate can't see).
//   --fetch-discs <rom>       fetch EVERY missing disc of one game.
//   --prefetch-discs [--dry]  daemon leg: enumerate ALL downloaded (non-stub) .m3u
//                             games with incomplete disc sets (mirror-manifest walk,
//                             offline) and complete them; --dry only reports the
//                             pending work, touching neither network nor card, so a
//                             daemon can decide whether a cycle is worth the radio.
//
// All per-rom modes print an additive RESULT line and exit 0 (the RESULT is the
// signal, launch is never gated on an exit code); pairing-expiry still surfaces via
// exitMode's PAIRING_EXPIRED contract.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lodor/catalog"
	"lodor/config"
	"lodor/fsutil"
	"lodor/platform"
	"lodor/romm"
)

// migrateLegacyM3U normalizes a legacy multi-disc game to the current on-card
// contract, entirely OFFLINE — two legs, dot-folder first:
//
//  0. DOT-HIDE the per-game disc folder (migrateLegacyDiscDir, lodor#7 UX fix):
//     rename a legacy non-dot "<Game>/" to ".<Game>/" and rewrite the playlist's +
//     manifest's "<Game>/…" lines to ".<Game>/…" in lockstep, so MinUI's hide()
//     stops listing the folder as a second, browsable entry beside the .m3u.
//  1. seed the manifest's canonical disc list from the old playlist when the
//     record has none (on a legacy card the playlist IS the full set), then
//  2. atomically rewrite the .m3u to list only discs with real bytes (old order
//     kept — it is the canonical order the engine wrote), the local-only shape
//     the shipped LodorOS launcher requires (it refuses to launch past a listed
//     stub — the pre-local-only never-launches regression).
//
// Runs at the top of the fetch modes BEFORE any network work, so a
// partially-downloaded legacy card (e.g. 3 of 4 discs on card) becomes launchable
// on its present discs even when the fetch itself fails (no Wi-Fi). THE ONE RULE:
// only a manifest-owned playlist is ever rewritten — a user's own .m3u (merge
// mode, manifest-less card) is never touched. Zero present discs rewrites to a
// 0-byte playlist — the stub shape the launch path already owns. Best-effort:
// any failure leaves the card as found (the old full-list shape still censuses
// correctly via the m3u-refs fallback).
//
// CONVERGENCE ON A LIVE LEGACY CARD (e.g. the 3-of-4-disc Dragoon card): the
// SHIPPED launcher's own pre-launch playlist check is unrebuildable from here — on
// the next launch it still reads the OLD m3u and routes through --download / the
// hooks' --fetch-next-disc; THAT run lands here first, dot-migrates the folder +
// playlist + manifest, then completes the missing disc and writes the local-only
// dot m3u. So one launch on such a card still shows the old visible folder;
// every subsequent list shows a single entry and launches clean. Acceptable,
// documented.
func migrateLegacyM3U(romPath string) {
	if !strings.EqualFold(filepath.Ext(romPath), ".m3u") {
		return
	}
	// Dot-folder leg runs even for a 0-byte stub .m3u: an interrupted legacy
	// download leaves real discs in the old visible folder, and the coming
	// downloadMultiDiscCore must find them at the DOT path (or it would re-pull
	// every disc and strand the old folder visible forever).
	migrateLegacyDiscDir(romPath)
	fi, err := os.Stat(romPath)
	if err != nil || fi.IsDir() || fi.Size() == 0 {
		return // absent or a 0-byte stub — the download path owns those
	}
	lines := catalog.M3UDiscLines(romPath)
	if len(lines) == 0 {
		return // references nothing usable — broken, not ours to rewrite offline
	}
	man := platform.LoadManifest()
	if !man.Owns(romPath) {
		return // never rewrite a playlist the mirror didn't create
	}
	dir := filepath.Dir(romPath)
	var local []string
	for _, l := range lines {
		if dfi, derr := os.Stat(filepath.Join(dir, l)); derr == nil && !dfi.IsDir() && dfi.Size() > 0 {
			local = append(local, l)
		}
	}
	if len(local) == len(lines) {
		// Already local-only (every listed disc has bytes). Seed the canonical list
		// if missing — free, and it keeps the census manifest-first from here on.
		if e, _ := man.Entry(romPath); len(e.Discs) == 0 {
			man.SetDiscs(romPath, lines)
			if serr := man.Save(); serr != nil {
				fmt.Fprintf(os.Stderr, "MANIFEST save failed (m3u migrate seed): %v\n", serr)
			}
		}
		return
	}
	if e, _ := man.Entry(romPath); len(e.Discs) == 0 {
		man.SetDiscs(romPath, lines) // the old full list is the canonical set
	}
	content := ""
	if len(local) > 0 {
		content = strings.Join(local, "\n") + "\n"
	}
	if werr := fsutil.WriteFileAtomicString(romPath, content, 0o644); werr != nil {
		// Leave the manifest unsaved too: the card still carries the old full-list
		// playlist, and the m3u-refs fallback census remains correct for it.
		fmt.Fprintf(os.Stderr, "M3UMIGRATE rewrite failed: %s\n", filepath.Base(romPath))
		return
	}
	if serr := man.Save(); serr != nil {
		fmt.Fprintf(os.Stderr, "MANIFEST save failed (m3u migrate): %v\n", serr)
	}
	fmt.Fprintf(os.Stderr, "M3UMIGRATE local-only: %s (%d/%d discs on card)\n",
		filepath.Base(romPath), len(local), len(lines))
}

// migrateLegacyDiscDir converges one legacy multi-disc game onto the dot-hidden
// per-game folder layout (lodor#7 UX fix — MultiDiscDir/DiscFolderName): rename a
// NON-dot "<Game>/" beside the .m3u to ".<Game>/", then rewrite the playlist's and
// the manifest canonical disc list's "<Game>/…" lines to ".<Game>/…". Offline,
// idempotent (second touch finds no non-dot folder and no non-dot lines), and
// best-effort throughout. Ownership gates mirror the rest of the mirror's
// destructive paths:
//
//   - the FOLDER is renamed only when the manifest owns it as a mirror folder
//     (downloadMultiDiscCore records it before any disc lands) — a user's own
//     same-named folder is never moved;
//   - lines are rewritten only for a manifest-owned playlist (THE ONE RULE) and
//     only once the dot folder actually EXISTS (renamed now, or previously — a
//     crash between rename and rewrite heals on the next touch); lines must never
//     point at a folder that isn't there;
//   - both-layouts-exist (a fresh dot download beside a stale legacy folder, or
//     that same crash window) MERGES, dot copy wins: move only what the dot folder
//     lacks, then sweep the manifest-owned remainder.
//
// The disc BASENAMES never change — only the folder component — so per-disc hash
// state, saves (keyed to the .m3u stem), and box art (.media/<stem>.png, anchored
// to the .m3u) are all untouched.
func migrateLegacyDiscDir(romPath string) {
	base := platform.StripLeadingMarker(filepath.Base(romPath))
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" || strings.HasPrefix(stem, ".") {
		return // no stem, or an already-hidden name (DiscFolderName leaves it alone)
	}
	dir := filepath.Dir(romPath)
	oldDir := filepath.Join(dir, stem)
	newDir := filepath.Join(dir, platform.DiscFolderName(stem))
	man := platform.LoadManifest()

	if fi, err := os.Stat(oldDir); err == nil && fi.IsDir() && man.OwnsKind(oldDir, platform.ManifestFolder) {
		renamed := false
		if _, derr := os.Stat(newDir); derr != nil {
			renamed = os.Rename(oldDir, newDir) == nil // atomic: the discs move as one
		} else if ents, rerr := os.ReadDir(oldDir); rerr == nil {
			for _, en := range ents {
				dst := filepath.Join(newDir, en.Name())
				if _, serr := os.Stat(dst); serr != nil {
					_ = os.Rename(filepath.Join(oldDir, en.Name()), dst)
				}
			}
			renamed = os.RemoveAll(oldDir) == nil
		}
		if renamed {
			man.RenamePath(oldDir, newDir)
			fsutil.SyncDir(dir) // FAT32: persist the folder rename
			fmt.Fprintf(os.Stderr, "M3UMIGRATE dot-folder: %s\n", filepath.Base(newDir))
		}
	}

	// Line rewrite — only against an existing dot folder, only on owned records.
	if fi, err := os.Stat(newDir); err != nil || !fi.IsDir() || !man.Owns(romPath) {
		if serr := man.Save(); serr != nil { // persist a rename even if lines wait
			fmt.Fprintf(os.Stderr, "MANIFEST save failed (dot-folder migrate): %v\n", serr)
		}
		return
	}
	oldPrefix := stem + "/"
	newPrefix := platform.DiscFolderName(stem) + "/"
	if fi, err := os.Stat(romPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
		if data, rerr := os.ReadFile(romPath); rerr == nil {
			rawLines := strings.Split(string(data), "\n")
			changed := false
			for i, l := range rawLines {
				t := strings.TrimSuffix(l, "\r")
				if strings.HasPrefix(t, oldPrefix) {
					rawLines[i] = newPrefix + strings.TrimPrefix(t, oldPrefix)
					changed = true
				}
			}
			if changed {
				if werr := fsutil.WriteFileAtomicString(romPath, strings.Join(rawLines, "\n"), 0o644); werr != nil {
					fmt.Fprintf(os.Stderr, "M3UMIGRATE dot-rewrite failed: %s\n", filepath.Base(romPath))
				}
			}
		}
	}
	if e, ok := man.Entry(romPath); ok && len(e.Discs) > 0 {
		out := make([]string, len(e.Discs))
		for i, l := range e.Discs {
			if strings.HasPrefix(l, oldPrefix) {
				out[i] = newPrefix + strings.TrimPrefix(l, oldPrefix)
			} else {
				out[i] = l
			}
		}
		man.SetDiscs(romPath, out)
	}
	if serr := man.Save(); serr != nil {
		fmt.Fprintf(os.Stderr, "MANIFEST save failed (dot-folder migrate): %v\n", serr)
	}
}

// runCheckRom is the OFFLINE pre-launch completeness gate. Contract:
//
//	RESULT complete=<0|1> [discs_total=<N> discs_present=<M>] [reason=<missing|stub|empty-m3u>]
//
// complete=1 = the ROM is fully present (single-file: non-empty; multi-disc: every
// canonical disc has real bytes). Multi-disc census is MANIFEST-FIRST (local-only
// .m3u, lodor#7): the playlist lists only present discs, so the full set comes from
// the mirror manifest's canonical disc list; a legacy record without one censuses
// by the m3u's own refs (on such a card those ARE the full list). Filesystem +
// local manifest only — still offline and pre-config, so a hook can key "should I
// re-trigger a fetch?" off this without config or network. Always exits 0: the
// caller's fail-open convention (an unparseable answer must launch as before).
func runCheckRom(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		fmt.Println("RESULT complete=0 reason=missing")
		os.Exit(0)
	}
	if fi.Size() == 0 {
		fmt.Println("RESULT complete=0 reason=stub")
		os.Exit(0)
	}
	if !strings.EqualFold(filepath.Ext(path), ".m3u") {
		fmt.Println("RESULT complete=1")
		os.Exit(0)
	}
	total, present := catalog.RomDiscCompleteness(platform.LoadManifest(), path)
	if total == 0 {
		// A real .m3u that references nothing usable is broken, not complete.
		fmt.Println("RESULT complete=0 discs_total=0 discs_present=0 reason=empty-m3u")
		os.Exit(0)
	}
	fmt.Printf("RESULT complete=%d discs_total=%d discs_present=%d\n",
		b2i(present == total), total, present)
	os.Exit(0)
}

// runFetchDiscs backs both --fetch-next-disc (budget=1) and --fetch-discs
// (budget<0): resolve the ROM, then ride the SAME downloadMultiDiscCore machinery
// the launch path uses (per-disc verify, idempotent skip of verified discs, honest
// stubs beyond budget, full .m3u rewrite). Contract:
//
//	RESULT fetched=<N> complete=<0|1> [discs_total=<T> discs_present=<P>] [reason=<token>]
//
// fetched counts discs actually transferred THIS run (0 on an already-complete set —
// idempotent, honest). complete=1 = every listed disc now has real bytes. A transfer
// failure keeps every verified disc (per-disc resume) and reports the partial truth.
// Exit 0 either way (RESULT is the signal); pairing-expiry exits 6 via exitMode.
func runFetchDiscs(client *romm.Client, cfg *config.Config, romPath string, budget int) {
	writeProgress(0)
	// Legacy full-list .m3u → local-only, BEFORE any network work: even if the
	// fetch below fails (no Wi-Fi, server down), the game must leave this run
	// launchable on the discs it already has.
	migrateLegacyM3U(romPath)
	id, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || id == 0 {
		fmt.Fprintf(os.Stderr, "DLFAIL resolve: %s\n", filepath.Base(romPath))
		fmt.Println("RESULT fetched=0 complete=0 reason=resolve")
		exitMode(0)
	}
	rom, err := client.GetRom(id)
	if err != nil || rom.ID == 0 {
		noteAuthErr(err)
		fmt.Fprintf(os.Stderr, "DLFAIL getrom id=%d\n", id)
		fmt.Println("RESULT fetched=0 complete=0 reason=getrom")
		exitMode(0)
	}
	if !rom.HasMultipleFiles {
		// Not a multi-disc game: nothing to fetch here, but answer the completeness
		// question honestly (the hook may call this on any ROM).
		complete := 0
		if fi, serr := os.Stat(romPath); serr == nil && !fi.IsDir() && fi.Size() > 0 {
			complete = 1
		}
		fmt.Printf("RESULT fetched=0 complete=%d reason=not-multidisc\n", complete)
		exitMode(0)
	}
	// SECURITY (path-traversal belt): same server-supplied-name vetting the download
	// path runs before any path is computed from this rom.
	if !platform.ValidateRomNames(rom) {
		fmt.Fprintf(os.Stderr, "DLFAIL unsafe-name rom=%d\n", rom.ID)
		writePhase("This game's server filenames are invalid")
		fmt.Println("RESULT fetched=0 complete=0 reason=unsafe-name")
		exitMode(0)
	}
	romName := rom.Name
	if romName == "" {
		romName = filepath.Base(rom.FsName)
	}
	man := platform.LoadManifest()
	var st discStats
	okDL := downloadMultiDiscCore(client, cfg, rom, romName, man, budget, &st)
	if !okDL && st.total == 0 {
		// Failed before the disc census (gates/validation) — no honest counts to print.
		fmt.Println("RESULT fetched=0 complete=0 reason=refused")
		exitMode(0)
	}
	// "cancelled=1" is ADDITIVE: the user's B-press stopped the transfer loop —
	// verified discs stay listed, the partial .tmp stays for resume.
	cancelSuffix := ""
	if st.cancelled {
		cancelSuffix = " cancelled=1"
	}
	fmt.Printf("RESULT fetched=%d complete=%d discs_total=%d discs_present=%d%s\n",
		st.fetched, b2i(st.complete()), st.total, st.present, cancelSuffix)
	exitMode(0)
}

// runPrefetchDiscs is the daemons' background leg: walk the mirror manifest for
// downloaded (non-stub) .m3u games with incomplete disc sets and complete each one
// (budget unlimited — this is the "hybrid B" half that makes mid-game disc swaps
// work without a relaunch). Contract:
//
//	RESULT prefetch_roms=<N> discs_missing=<M> fetched=<F> failed=<K>
//
// prefetch_roms/discs_missing describe the work found (offline census); fetched is
// discs actually transferred; failed counts games whose completion did not finish
// (they stay incomplete and are re-found next cycle — no queue to corrupt). --dry
// prints the census and exits without touching network or card, so a daemon can
// decide whether the cycle is worth the radio. Quiet by design: per-game PREFETCH
// lines go to stderr (the daemons' log), never stdout. Exit: 0, or 4 when any game
// failed (the documented ran-but-errored code). A game evicted between the census
// and its turn in the fetch loop (its .m3u back to a 0-byte stub) is skipped, not
// refilled — and not counted as failed (there is nothing left to complete).
func runPrefetchDiscs(client *romm.Client, cfg *config.Config, dry bool) {
	inc := catalog.IncompleteMultiDiscDownloads()
	missing := 0
	for _, g := range inc {
		missing += g.Total - g.Present
	}
	if dry || len(inc) == 0 {
		fmt.Printf("RESULT prefetch_roms=%d discs_missing=%d fetched=0 failed=0\n", len(inc), missing)
		os.Exit(0)
	}
	fetched, failed := 0, 0
	for _, g := range inc {
		// Re-stat before spending network on this game: the census ran at cycle start,
		// and a "Delete from card" since then flipped the .m3u back to a 0-byte stub.
		// Prefetching it now would silently UNDO the user's eviction.
		if !prefetchStillWanted(g.Path) {
			fmt.Fprintf(os.Stderr, "PREFETCH skip (evicted since census): %s\n", filepath.Base(g.Path))
			continue
		}
		id, ok := catalog.ResolveRomID(cfg, g.Path)
		if !ok || id == 0 {
			fmt.Fprintf(os.Stderr, "PREFETCH skip (unresolved): %s\n", filepath.Base(g.Path))
			failed++
			continue
		}
		rom, err := client.GetRom(id)
		if err != nil || rom.ID == 0 {
			noteAuthErr(err)
			fmt.Fprintf(os.Stderr, "PREFETCH getrom failed rom=%d\n", id)
			failed++
			continue
		}
		if !rom.HasMultipleFiles || !platform.ValidateRomNames(rom) {
			fmt.Fprintf(os.Stderr, "PREFETCH skip (not multi-disc / unsafe names) rom=%d\n", id)
			failed++
			continue
		}
		romName := rom.Name
		if romName == "" {
			romName = filepath.Base(rom.FsName)
		}
		// Fresh manifest per game: downloadMultiDiscCore records + saves ownership
		// as it goes, and a stale in-memory copy must never clobber those writes.
		man := platform.LoadManifest()
		var st discStats
		okDL := downloadMultiDiscCore(client, cfg, rom, romName, man, -1, &st)
		fetched += st.fetched
		if okDL && st.complete() {
			fmt.Fprintf(os.Stderr, "PREFETCH complete rom=%d discs=%d/%d fetched=%d\n", id, st.present, st.total, st.fetched)
		} else {
			fmt.Fprintf(os.Stderr, "PREFETCH incomplete rom=%d discs=%d/%d fetched=%d (retry next cycle)\n", id, st.present, st.total, st.fetched)
			failed++
		}
	}
	fmt.Printf("RESULT prefetch_roms=%d discs_missing=%d fetched=%d failed=%d\n",
		len(inc), missing, fetched, failed)
	if failed > 0 {
		exitMode(4)
	}
	exitMode(0)
}

// prefetchStillWanted re-stats a censused .m3u immediately before its network work:
// only a still-real (non-stub, non-dir, still-present) playlist wants completion. A
// 0-byte size means EvictToStub ran mid-cycle — the mirror's stub shape — so the
// game must NOT be refilled behind the user's back.
func prefetchStillWanted(m3uPath string) bool {
	fi, err := os.Stat(m3uPath)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}
