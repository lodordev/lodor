package main

// Native-menu CLI modes. A C MinUI launcher (minui.c) shells out to this binary and
// parses the line output, so the stdout contracts here are EXACT — no stray prints
// reach stdout. Diagnostics go to stderr and are kept host-free (no URL, token, or
// device_id ever appears in any line, file, or log). §3 download/BIOS orchestration
// lives here too.

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"lodor/catalog"
	"lodor/config"
	"lodor/cover"
	"lodor/platform"
	"lodor/romm"
	"lodor/sync"
)

// runDownloadRom downloads ONE ROM's real file (turning a 0-byte stub into a
// playable game), verifies it against RomM's recorded hash, and streams coarse
// percent progress to /tmp/dl-progress (0 → 90 on transfer → 100 on a verified
// write). Contract: RESULT downloaded=<0|1>. BLUEPRINT §3.
//
// The buffered raw GET can't give true byte-level progress, so the bar is coarse:
// the verify step is the real gate. Multi-file or no-hash ROMs are accepted without
// a hash check; a hash mismatch deletes the bad file and reports downloaded=0.
//
// SECURITY: every failure prints a generic, host-free DLFAIL token to stderr — the
// URL and the underlying client error (which can embed the host) are NEVER echoed.
func runDownloadRom(client *romm.Client, cfg *config.Config, romPath string) {
	if downloadRomCore(client, cfg, romPath) {
		fmt.Println("RESULT downloaded=1")
	} else {
		fmt.Println("RESULT downloaded=0")
	}
	os.Exit(0)
}

// downloadRomCore does the actual single-ROM download work and returns true iff a
// playable, hash-verified file landed. It prints NO RESULT line and never exits — so
// it is shared by both --download (runDownloadRom) and --download-queue
// (runDownloadQueue). Every progress/phase side-channel write and every host-free
// DLFAIL stderr diagnostic from the original --download path is preserved verbatim.
func downloadRomCore(client *romm.Client, cfg *config.Config, romPath string) bool {
	writeProgress(0)

	id, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || id == 0 {
		fmt.Fprintf(os.Stderr, "DLFAIL resolve: %s\n", filepath.Base(romPath))
		return false
	}
	rom, err := client.GetRom(id)
	if err != nil || rom.ID == 0 {
		fmt.Fprintf(os.Stderr, "DLFAIL getrom id=%d\n", id)
		return false
	}
	// Fill the stub IN PLACE at the exact path the launcher passed (and will launch).
	// That path carries the leading state marker ("[^] Game.gba"); a NextUI pre-launch
	// hook cannot redirect the post-hook launch and exFAT has no symlinks, so writing the
	// real bytes anywhere else (e.g. the unmarked LocalRomPath) would leave the launcher
	// opening a dead/empty path. The cloud->on-device marker SWAP happens later at mirror
	// time (platform.ReconcileMarkedPresence), which carries the first save with it. For a
	// single-file ROM romPath is the stub; multi-disc resolves its own paths below.
	dest := romPath
	if dest == "" {
		fmt.Fprintf(os.Stderr, "DLFAIL dest-empty rom=%d\n", rom.ID)
		return false
	}

	romName := rom.Name
	if romName == "" {
		romName = filepath.Base(rom.FsName)
	}

	// MULTI-DISC (folder-per-game, has_multiple_files): RomM serves all discs as a
	// mod_zip, OR each disc individually via a single file_ids selector. We download
	// disc-by-disc (streamed, no OOM; per-disc hash-verified) and write the .m3u.
	if rom.HasMultipleFiles {
		return downloadMultiDiscCore(client, cfg, rom, romName)
	}

	// BROKEN IMPORT GUARD: a single-file rom whose only file is a bare `.m3u` is a
	// mis-registered multi-disc game (RomM scanned a loose root .m3u as its own rom).
	// Downloading the m3u text would write a tiny fake "game" that launches nothing —
	// the exact 200-byte-stub trap we must not fall into. Report honestly and skip.
	if isBareM3U(rom) {
		fmt.Fprintf(os.Stderr, "DLSKIP broken-m3u rom=%d (single-file .m3u — re-import as folder-per-game)\n", rom.ID)
		writePhase("This game needs re-importing on the server")
		writeProgress(0)
		return false
	}

	writePhase(fmt.Sprintf("Downloading %s…", romName))

	// Stream the content straight to the .tmp file (io.Copy via DownloadRomContentTo),
	// NEVER buffering the whole ROM in RAM — a multi-hundred-MB CHD OOM-crashes the
	// 128 MB device otherwise (the bug behind the single-file download failures).
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	tmp := dest + ".tmp"
	// RESUMABLE: if a partial .tmp survives from an interrupted run, resume from its end
	// via an HTTP Range request instead of re-fetching from byte 0 — a dropped multi-hundred-MB
	// download no longer restarts at zero. Open WITHOUT O_TRUNC so the partial is preserved;
	// the resume transport truncates/seeks as the server's 206/200/416 response dictates.
	var startOffset int64
	if fi, statErr := os.Stat(tmp); statErr == nil && fi.Size() > 0 {
		startOffset = fi.Size()
	}
	out, oErr := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY, 0o644)
	if oErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL create rom=%d\n", rom.ID)
		writeProgress(0)
		return false
	}
	// Real progress bar: report 0→90% as bytes stream (reserve 90→100 for verify). On a
	// resume the callback's done is primed with startOffset against the full total, so the
	// bar picks up where it left off rather than jumping backward.
	lastPct := -1
	onProg := func(done, total int64) {
		if total <= 0 {
			return
		}
		pct := int(done * 90 / total)
		if pct > lastPct {
			lastPct = pct
			writeProgress(pct)
		}
	}
	n, derr := client.DownloadRomContentResumeTo(rom.ID, rom.FsName, out, startOffset, onProg)
	cErr := out.Close()
	if derr != nil || cErr != nil || n == 0 {
		// KEEP the partial .tmp on a transfer error so the NEXT --download (or the
		// download-queue retry) resumes from here instead of restarting at zero. A
		// corrupt/stale partial is caught later by the hash verify (which deletes the
		// bad file), so retaining it is safe.
		fmt.Fprintf(os.Stderr, "DLFAIL download rom=%d\n", rom.ID)
		writeProgress(0)
		return false
	}
	writeProgress(90)

	_ = os.Remove(dest) // remove the stub before rename (rename over a dir would fail)
	if rErr := os.Rename(tmp, dest); rErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL rename rom=%d\n", rom.ID)
		_ = os.Remove(tmp)
		return false
	}

	writePhase("Verifying…")
	if vErr := verifyRomHash(rom, dest); vErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL verify rom=%d: %s\n", rom.ID, vErr)
		_ = os.Remove(dest)
		writeProgress(0)
		return false
	}
	writeProgress(100)

	fetchRomCover(client, rom, dest)

	return true
}

// isBareM3U reports whether a single-file ROM's one file is a bare `.m3u` playlist —
// a mis-registered multi-disc import (RomM scanned a loose root .m3u as its own rom).
// Such a "rom" has no real game bytes to fetch; treating it as a normal download would
// write a tiny fake game that launches nothing. Multi-file roms are excluded (their
// LocalRomPath m3u is legitimately generated by us from the discs).
func isBareM3U(rom romm.Rom) bool {
	if rom.HasMultipleFiles {
		return false
	}
	if len(rom.Files) == 1 {
		return strings.EqualFold(filepath.Ext(rom.Files[0].FileName), ".m3u")
	}
	// Some single-file roms carry no files[] but expose the extension via fs_extension.
	if len(rom.Files) == 0 && strings.EqualFold("."+strings.TrimPrefix(rom.FsExtension, "."), ".m3u") {
		return true
	}
	return false
}

// fetchRomCover fetches this one ROM's box-art into .media/ (BLUEPRINT §11),
// UNCONDITIONALLY of the bulk "fetch_covers" toggle (a per-game download is an
// explicit action, so its Details view should show art even when bulk is off).
// Best-effort, never gating the RESULT; honors pkg cover's skip-existing / no-cover /
// graceful-error contract. coverAnchor is the path cover.MediaPath() keys off — the
// .m3u for a multi-disc game, the rom file for a single-file game.
func fetchRomCover(client *romm.Client, rom romm.Rom, coverAnchor string) {
	if cp := rom.CoverPath(); cp != "" {
		if out, cerr := cover.FetchAndSave(client, cp, coverAnchor); out == cover.OutcomeError && cerr != nil {
			fmt.Fprintf(os.Stderr, "COVERWARN rom=%d: %s\n", rom.ID, safeErr(cerr))
		}
	}
}

// runDownloadMultiDisc downloads every disc of a folder-per-game multi-file ROM and
// writes the .m3u that ties them together. Contract: RESULT downloaded=<0|1>, same as
// the single-file path. Each disc is fetched INDIVIDUALLY via a single file_ids
// selector (verified live: one file_ids → that file's raw bytes; two or more → a
// mod_zip). This is chosen over pulling RomM's all-files mod_zip deliberately:
//   - STREAMED to disk (io.Copy), so a 1.3 GB game never sits in 128 MB of RAM;
//   - each disc runs the SAME integrity gate as the single-file path (verifyFileHash:
//     non-CHD discs are checked against files[].sha1/md5; .chd discs are accepted on a
//     valid streamed transfer because RomM records a CHD's INTERNAL disc SHA1, not the
//     container's — see isCHD; a mod_zip would expose no per-member hash to check at all);
//   - a failed disc fails the WHOLE game honestly (partial multi-disc is unplayable):
//     we clean up what we wrote and report downloaded=0, never a half-game.
//
// Discs land in <Roms>/<system>/<FsNameNoExt>/<disc>.chd; the .m3u (at <FsNameNoExt>.m3u
// beside that folder) lists "<FsNameNoExt>/<disc>" lines, resolved relative to the m3u
// dir by both the launcher (getFirstDisc) and the emulator's m3u loader.
func downloadMultiDiscCore(client *romm.Client, cfg *config.Config, rom romm.Rom, romName string) bool {
	if len(rom.Files) == 0 {
		fmt.Fprintf(os.Stderr, "DLFAIL multidisc rom=%d: no files[]\n", rom.ID)
		writePhase("This game needs re-importing on the server")
		writeProgress(0)
		return false
	}

	m3uPath := platform.LocalRomPath(cfg, rom) // <…>/<FsNameNoExt>.m3u
	discDir := platform.MultiDiscDir(cfg, rom) // <…>/<FsNameNoExt>/
	if m3uPath == "" || discDir == "" {
		fmt.Fprintf(os.Stderr, "DLFAIL multidisc dest-empty rom=%d\n", rom.ID)
		return false
	}

	if mkErr := os.MkdirAll(discDir, 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL multidisc mkdir rom=%d\n", rom.ID)
		return false
	}

	folderName := filepath.Base(discDir) // == rom.FsNameNoExt; the m3u-relative prefix
	total := len(rom.Files)
	var m3uLines []string

	writeProgress(0)
	for i, f := range rom.Files {
		if f.ID == 0 || f.FileName == "" {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc rom=%d: disc %d missing id/name\n", rom.ID, i+1)
			cleanupMultiDisc(discDir)
			writeProgress(0)
			return false
		}
		writePhase(fmt.Sprintf("Downloading %s — disc %d/%d…", romName, i+1, total))

		discDest := filepath.Join(discDir, f.FileName)
		// Already present and hash-clean from a prior partial run? Skip the transfer but
		// still list it (idempotent resume — never re-pull a verified disc).
		if existsNonEmpty(discDest) && verifyFileHash(discDest, f.Sha1Hash, f.Md5Hash) == nil {
			m3uLines = append(m3uLines, folderName+"/"+f.FileName)
			writeProgress((i + 1) * 100 / total)
			continue
		}

		tmp := discDest + ".tmp"
		out, cErr := os.Create(tmp)
		if cErr != nil {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc create rom=%d disc=%d\n", rom.ID, i+1)
			cleanupMultiDisc(discDir)
			return false
		}
		// Real progress: this disc spans [i/total, (i+1)/total] of the overall bar;
		// move smoothly within that band as its bytes stream.
		discBase := i * 100 / total
		discSpan := (i+1)*100/total - discBase
		lastPct := -1
		onProg := func(done, tot int64) {
			if tot <= 0 || discSpan <= 0 {
				return
			}
			pct := discBase + int(done*int64(discSpan)/tot)
			if pct > lastPct {
				lastPct = pct
				writeProgress(pct)
			}
		}
		n, dErr := client.DownloadRomFileTo(rom.ID, rom.FsName, f.ID, out, onProg)
		cerr2 := out.Close()
		if dErr != nil || cerr2 != nil || n == 0 {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc download rom=%d disc=%d: %s\n", rom.ID, i+1, safeErr(dErr))
			_ = os.Remove(tmp)
			cleanupMultiDisc(discDir)
			writeProgress(0)
			return false
		}
		if rErr := os.Rename(tmp, discDest); rErr != nil {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc rename rom=%d disc=%d\n", rom.ID, i+1)
			_ = os.Remove(tmp)
			cleanupMultiDisc(discDir)
			return false
		}

		// Per-disc hash verify (same gate as single-file). A .chd or a disc with no
		// recorded hash is accepted (parity with verifyRomHash); a real mismatch fails
		// the whole game.
		if vErr := verifyFileHash(discDest, f.Sha1Hash, f.Md5Hash); vErr != nil {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc verify rom=%d disc=%d: %s\n", rom.ID, i+1, vErr)
			cleanupMultiDisc(discDir)
			writeProgress(0)
			return false
		}

		m3uLines = append(m3uLines, folderName+"/"+f.FileName)
		writeProgress((i + 1) * 100 / total)
	}

	// Write the .m3u atomically (.tmp → rename), clearing any 0-byte stub at that path.
	writePhase("Writing playlist…")
	m3uTmp := m3uPath + ".tmp"
	if wErr := os.WriteFile(m3uTmp, []byte(strings.Join(m3uLines, "\n")+"\n"), 0o644); wErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL multidisc m3u-write rom=%d\n", rom.ID)
		cleanupMultiDisc(discDir)
		return false
	}
	_ = os.Remove(m3uPath) // remove the stub before rename
	if rErr := os.Rename(m3uTmp, m3uPath); rErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL multidisc m3u-rename rom=%d\n", rom.ID)
		_ = os.Remove(m3uTmp)
		cleanupMultiDisc(discDir)
		return false
	}
	writeProgress(100)

	fetchRomCover(client, rom, m3uPath)

	return true
}

// cleanupMultiDisc removes the per-game disc folder after a failed multi-disc
// download, so a partial (unplayable) game never lingers as if it were complete. The
// stub .m3u itself is left for the menu to re-tap; only our half-written discs go.
func cleanupMultiDisc(discDir string) {
	if discDir != "" {
		_ = os.RemoveAll(discDir)
	}
}

// existsNonEmpty reports whether path exists and is a non-empty regular file.
func existsNonEmpty(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

// isCHD reports whether path is a MAME Compressed Hunks of Data file. RomM records a
// CHD's hash as the INTERNAL disc-data SHA1 (its identity hash for matching/dedup),
// NOT the SHA1 of the .chd CONTAINER bytes — verified live 2026-06-25: a GBA ROM's
// recorded sha1 matched the downloaded bytes exactly, while Tactics Ogre's .chd
// recorded sha1 (19cc1d52…) differed from the container bytes (b52cca5c…), and
// `chdman verify` confirmed the container was a complete, valid CHD. A CGO-free Go
// binary cannot reproduce the internal SHA1 (that needs the CHD codec), so verifying
// the container bytes against the internal hash would FALSELY reject every good CHD.
// We therefore skip container-hash verification for CHDs (see verifyFileHash /
// verifyRomHash) and rely on the streamed transfer (real Content-Length, non-zero) +
// the CHD's own self-checking structure at load time. This is honest: we never claim
// a hash matched — we acknowledge the recorded hash is not of the bytes we hold.
func isCHD(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".chd")
}

// verifyFileHash checks a file against a recorded sha1 (preferred) then md5,
// case-insensitively. An empty sha1 AND empty md5 means "no recorded hash" → accept
// (parity with verifyRomHash for single-file ROMs). A .chd is accepted without a
// container-hash check (RomM's recorded hash is the CHD's INTERNAL disc SHA1, not the
// container's — see isCHD). The error names only the hash kind and digests — never the host.
func verifyFileHash(path, wantSha1, wantMd5 string) error {
	if isCHD(path) {
		return nil
	}
	if wantSha1 == "" && wantMd5 == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if wantSha1 != "" {
		h := sha1.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSha1) {
			return fmt.Errorf("sha1 mismatch got %s want %s", got, wantSha1)
		}
		return nil
	}
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantMd5) {
		return fmt.Errorf("md5 mismatch got %s want %s", got, wantMd5)
	}
	return nil
}

// verifyRomHash checks a downloaded single-file ROM against RomM's recorded hash:
// sha1 preferred, then md5, compared case-insensitively. Multi-file games, .chd files
// (RomM records the CHD's INTERNAL disc SHA1, not the container's — see isCHD), and
// games with no recorded hash are accepted (BLUEPRINT §3). The error it returns names
// only the hash kind and digests — never the host.
func verifyRomHash(rom romm.Rom, romPath string) error {
	if rom.HasMultipleFiles {
		return nil
	}
	if isCHD(romPath) {
		return nil
	}
	if rom.Sha1Hash == "" && rom.Md5Hash == "" {
		return nil
	}
	f, err := os.Open(romPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if rom.Sha1Hash != "" {
		h := sha1.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, rom.Sha1Hash) {
			return fmt.Errorf("sha1 mismatch got %s want %s", got, rom.Sha1Hash)
		}
		return nil
	}
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, rom.Md5Hash) {
		return fmt.Errorf("md5 mismatch got %s want %s", got, rom.Md5Hash)
	}
	return nil
}

// runDownloadBios downloads BIOS/firmware for every MAPPED platform and writes each
// file to EVERY BIOS destination for that platform. Contract: RESULT bios=<count>,
// count = files newly written. BLUEPRINT §3.
//
// Idempotent: a firmware whose destinations ALL already exist non-empty is skipped
// (never re-fetched — the bug grout had was overwriting every BIOS each run). No hash
// verification on BIOS.
func runDownloadBios(client *romm.Client, cfg *config.Config) {
	platforms, err := mappedPlatforms(client, cfg)
	if err != nil {
		// Couldn't list platforms — treat as reachable-but-nothing-done rather than a
		// hard error so the launcher still gets a parseable RESULT.
		fmt.Fprintf(os.Stderr, "BIOSFAIL platforms: %s\n", safeErr(err))
		fmt.Println("RESULT bios=0")
		os.Exit(0)
	}

	count := 0
	for _, p := range platforms {
		fw, ferr := client.GetFirmware(p.ID)
		if ferr != nil || len(fw) == 0 {
			continue // no BIOS on the server for this platform
		}
		for _, f := range fw {
			dests := platform.BIOSFilePaths(f.FileName, p.FsSlug)
			if len(dests) == 0 {
				continue
			}
			// Skip-if-present: all destinations already there non-empty → don't refetch.
			allPresent := true
			for _, d := range dests {
				if fi, sErr := os.Stat(d); sErr != nil || fi.Size() == 0 {
					allPresent = false
					break
				}
			}
			if allPresent {
				continue
			}

			data, derr := client.DownloadFirmwareContent(f.ID, f.FileName)
			if derr != nil || len(data) == 0 {
				continue
			}
			wrote := false
			for _, dest := range dests {
				if fi, sErr := os.Stat(dest); sErr == nil && fi.Size() > 0 {
					continue // keep the copy already there
				}
				if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
					continue
				}
				if wErr := os.WriteFile(dest, data, 0o644); wErr == nil {
					wrote = true
				}
			}
			if wrote {
				count++
			}
		}
	}
	fmt.Printf("RESULT bios=%d\n", count)
	os.Exit(0)
}

// runPushPending uploads every save in pending-saves.txt via sync.PushSaveDirect and
// removes each ROM that lands (Pushed or AlreadyOnServer). Contract:
//
//	RESULT pushed=<N> total=<M> stuck=<K>
//
// where M = number of pending entries, N = entries now safely on the server this
// run, K = M - N (still stuck). The queue file is mutated under a .queue.lock mkdir
// so an append from the minarch shim can't race the rewrite.
//
// CRITICAL (BLUEPRINT §8): one host-free line per save is logged to STDERR via
// PushResult.Line() — so a stuck save (NoLocalSave, UploadError, ResolveFail) names
// its own cause for Banjo-Pilot/Banjo-Kazooie self-diagnosis — WITHOUT polluting the
// single RESULT line on stdout that the launcher parses.
func runPushPending(client *romm.Client, cfg *config.Config) {
	release := acquireQueueLock()
	defer release()

	pending := pendingRead()
	total := len(pending)
	writeProgress(0)

	var allResults []sync.PushResult
	var remaining []string  // entries to keep in the queue (not landed)
	var stuckLines []string // human "STUCK\t<game>\t<why>" lines for the launcher (stdout)

	for i, line := range pending {
		romPath, stagedPath, emu, isStaged := parseQueueLine(line)
		writePhase(fmt.Sprintf("Uploading %s (%d/%d)…", filepath.Base(romPath), i+1, total))
		if total > 0 {
			writeProgress((i + 1) * 100 / total)
		}

		// Staged entry: upload the parked pre-flashback FILE (the live save now holds the
		// flashed-back bytes), then delete the staged copy once it's safely on the timeline.
		if isStaged {
			r := sync.PushSaveFile(client, cfg, romPath, stagedPath, emu)
			allResults = append(allResults, r)
			fmt.Fprintln(os.Stderr, r.Line())
			if r.Outcome == sync.OutcomePushed || r.Outcome == sync.OutcomeAlreadyOnServer {
				_ = os.Remove(stagedPath)
				continue // landed → drop from queue
			}
			if why := stuckReason(r); why != "" {
				stuckLines = append(stuckLines, fmt.Sprintf("STUCK\t%s\t%s", filepath.Base(romPath), why))
			}
			remaining = append(remaining, line) // keep the staged line to retry
			continue
		}

		results := sync.PushSaveDirect(client, cfg, romPath)
		allResults = append(allResults, results...)

		// One host-free diagnostic line per save result, to STDERR (the launcher reads
		// only stdout's RESULT line). This is the stuck-save self-diagnosis channel.
		for _, r := range results {
			fmt.Fprintln(os.Stderr, r.Line())
		}
		// A human reason per STUCK save, named by game, to STDOUT — the launcher reads
		// this and tells the user WHY a save didn't upload (instead of "Uploaded 0").
		for _, r := range results {
			if why := stuckReason(r); why != "" {
				stuckLines = append(stuckLines, fmt.Sprintf("STUCK\t%s\t%s", filepath.Base(romPath), why))
			}
		}

		// A ROM is dequeued only when EVERY one of its save results is safely on the
		// server (Pushed or AlreadyOnServer). A ROM that resolved-but-has-no-save, or
		// errored, stays queued so the user retries it with WiFi later.
		if entryLanded(results) {
			continue // landed → drop from queue
		}
		remaining = append(remaining, romPath)
	}

	pushed, totalSaves, stuck := sync.Counts(allResults)
	// Counts is over save FILES; the §8 contract's total/stuck are over save files
	// too (a no-local-save ROM contributes one stuck row), so this is the right shape.
	_ = totalSaves

	if err := pendingWrite(remaining); err != nil {
		fmt.Fprintf(os.Stderr, "QUEUEWARN rewrite failed: %s\n", safeErr(err))
	}

	fmt.Printf("RESULT pushed=%d total=%d stuck=%d\n", pushed, len(allResults), stuck)
	for _, s := range stuckLines {
		fmt.Println(s)
	}
	os.Exit(0)
}

// stuckReason maps a non-landed push outcome to a short, human, host-free reason the
// launcher can show the user. Returns "" for outcomes that aren't stuck (Pushed /
// AlreadyOnServer), so only genuinely-stuck saves produce a line.
func stuckReason(r sync.PushResult) string {
	switch r.Outcome {
	case sync.OutcomeResolveFail:
		return "this game is no longer on your server"
	case sync.OutcomeNoLocalSave:
		return "no save file found on the card"
	case sync.OutcomeUploadError:
		return "upload failed — check Wi-Fi and retry"
	default:
		return ""
	}
}

// entryLanded reports whether every result for one ROM is safely on the server. An
// empty slice (shouldn't happen — PushSaveDirect always returns ≥1 row) is treated
// as not landed so the entry is kept rather than silently dropped.
func entryLanded(results []sync.PushResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.Outcome != sync.OutcomePushed && r.Outcome != sync.OutcomeAlreadyOnServer {
			return false
		}
	}
	return true
}

// runSyncSave reconciles the save for ONE ROM two-way (the per-game two-way sync).
// Contract: RESULT pulled=<0|1> pushed=<0|1> conflict=<0|1>.
//
// SYNC V2 (research #1 — the save-sync MOAT): this now uses the anchor-based 3-way
// reconciler (sync.SyncSaveReconciled) instead of the old newest-wins pull-then-push,
// which could SILENTLY OVERWRITE a locally-advanced save on PULL. The reconciler keeps
// the local copy, the server copy, or — when BOTH advanced from the last-synced anchor
// to different content — surfaces a CONFLICT and overwrites NOTHING. On a server that
// exposes no per-save content_hash it falls back to the exact old newest-wins behavior
// (no regression for older servers).
//
// The conflict= field is ADDITIVE (fields pulled=/pushed= are unchanged and still
// first), and a CONFLICT line is emitted on stdout so a launcher that learns to read it
// can prompt the user. NOT yet round-trip tested on hardware — see the buildout report.
func runSyncSave(client *romm.Client, cfg *config.Config, romPath string) {
	writeProgress(0)
	writePhase("Reconciling save…")

	res := sync.SyncSaveReconciled(client, cfg, romPath, sync.ReconcileOpts{
		PendingUpload: isPendingUpload(romPath),
	})

	writeProgress(100)
	// One host-free diagnostic line to STDERR (never the launcher's parsed stdout).
	fmt.Fprintf(os.Stderr, "sync-save: decision=%s pulled=%t pushed=%t conflict=%t reason=%q\n",
		res.Decision, res.Pulled, res.Pushed, res.Conflict, res.Reason)

	fmt.Printf("RESULT pulled=%d pushed=%d conflict=%d\n",
		b2i(res.Pulled), b2i(res.Pushed), b2i(res.Conflict))
	// A divergent save the engine refused to auto-resolve: surface it explicitly so the
	// launcher can offer the user a choice (restore server / keep local) instead of the
	// engine silently picking a winner. Additive — older launchers ignore unknown lines.
	if res.Conflict {
		fmt.Printf("CONFLICT\t%s\tboth this device and the server changed this save\n", filepath.Base(romPath))
	}
	os.Exit(0)
}

// runPushSave is the HYBRID post-game save sync for ONE ROM (the minarch shim's
// post-game phase, invoked only when WiFi is already up). It pushes the changed save
// straight to the server; on a LANDED push it writes the synced-✓ signal file the
// launcher flashes and queues NOTHING. If the push does NOT land (server unreachable,
// upload error, or the ROM can't be resolved), the save is recorded in the offline
// pending queue so the pending-count badge shows the backlog and --push-pending uploads
// it later — and the synced signal is NEVER written (honor the no-fake-state rule: a
// ✓ means a verified, landed upload, nothing less). Contract:
//
//	RESULT pushed=<0|1> staged=<N>
//
// pushed=1 (and staged=0) only on a landed push; otherwise pushed=0 and staged=N is the
// number of pending entries recorded for later upload (>=1 whenever a save existed).
func runPushSave(client *romm.Client, cfg *config.Config, romPath string) {
	results := sync.PushSaveDirect(client, cfg, romPath)
	if entryLanded(results) {
		pushed, _, _ := sync.Counts(results)
		// Landed → write the launcher's synced-✓ signal. Best-effort: a failed signal
		// write must not turn a real, verified upload into a reported failure.
		if err := writeLastSynced(romPath, pushed); err != nil {
			fmt.Fprintf(os.Stderr, "push-save: WARN couldn't write synced signal: %s\n", safeErr(err))
		}
		fmt.Printf("RESULT pushed=1 staged=0\n")
		os.Exit(0)
	}

	// Not landed → stage the current save bytes and queue them for a later upload
	// (offline backlog). Mirrors runRestoreSave's stage+enqueue path. When the ROM
	// couldn't be resolved (e.g. the server was unreachable so GetRom failed), there is
	// no per-file list to stage — fall back to a bare-path queue entry so a changed save
	// is NEVER silently dropped and the pending badge still counts it (--push-pending
	// resolves+uploads the live save when WiFi returns).
	staged := 0
	for _, f := range sync.LocalSaveFilesForRom(client, cfg, romPath) {
		sp, serr := stageSaveFile(f)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "push-save: WARN couldn't stage save: %s\n", safeErr(serr))
			continue
		}
		if qerr := enqueueStaged(romPath, sp, filepath.Base(filepath.Dir(f))); qerr != nil {
			fmt.Fprintf(os.Stderr, "push-save: WARN couldn't queue staged save: %s\n", safeErr(qerr))
			_ = os.Remove(sp)
			continue
		}
		staged++
	}
	if staged == 0 {
		if err := enqueueBare(romPath); err != nil {
			fmt.Fprintf(os.Stderr, "push-save: WARN couldn't queue pending save: %s\n", safeErr(err))
		} else {
			staged = 1
		}
	}
	fmt.Printf("RESULT pushed=0 staged=%d\n", staged)
	os.Exit(0)
}

// writeLastSynced records a just-LANDED push as the launcher's synced-✓ signal at
// <LODOR_PAK_DIR>/last-synced.txt, atomically (tmp+rename). One line, exactly:
//
//	<unix_ts> <count> <basename>
//
// where unix_ts is the moment the push landed (the launcher compares it against its
// last-synced-ack.txt to flash the ✓ once), count is how many save files landed, and
// basename is the ROM's on-disk file name (may contain spaces — it is the REST of the
// line after the first two space-separated fields, so the launcher must parse it that
// way). Written ONLY on a verified landed push.
func writeLastSynced(romPath string, count int) error {
	p := filepath.Join(pakDir(), "last-synced.txt")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	line := fmt.Sprintf("%d %d %s\n", time.Now().Unix(), count, filepath.Base(romPath))
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(line), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// runReconcile flips ONE ROM's on-disk state marker (✘→✓) to match the bytes now on the
// card, AFTER the game has exited — the only safe window to rename, since renaming during
// the download→launch sequence would pull the file out from under the launcher (the reason
// decision #69 reverted relocate). A freshly-downloaded game whose 0-byte stub was filled
// in place under the cloud marker is promoted to the on-device marker, carrying its save +
// cover with the rename. A game still a 0-byte stub stays ✘. Contract:
// RESULT reconciled=<0|1> (1 = the marker actually flipped). Offline; no network/device.
func runReconcile(cfg *config.Config, romPath string) {
	flipped := catalog.ReconcileAfterDownload(cfg, romPath)
	fmt.Printf("RESULT reconciled=%d\n", b2i(flipped))
	os.Exit(0)
}

// runListSaves prints every server save for one ROM, NEWEST FIRST, one tab-separated
// line per save and nothing else:
//
//	<save_id>\t<YYYY-MM-DD HH:MM>\t<device-or-emulator>\t<size_kb>KB
//
// "who" is the first device sync's DeviceName when present, else the emulator. Zero
// saves (or an unmanaged ROM) → print nothing, exit 0.
func runListSaves(client *romm.Client, cfg *config.Config, romPath string) {
	id, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || id == 0 {
		os.Exit(0) // unmanaged ROM — nothing to list
	}
	saves, err := client.GetSaves(romm.SaveQuery{RomID: id})
	if err != nil || len(saves) == 0 {
		os.Exit(0)
	}
	sort.Slice(saves, func(i, j int) bool { return saves[i].UpdatedAt.After(saves[j].UpdatedAt) })

	// Determine which server revision matches the bytes CURRENTLY on the device, so the
	// launcher can mark it. The signal is RomM's per-save content_hash (the MD5 of the save
	// bytes) compared against the MD5 of the on-device save file(s) — the exact same signal
	// AlreadyOnServer trusts for dedup. Mark only the NEWEST matching revision (saves are
	// sorted newest-first), and only when the server actually exposes a content_hash AND a
	// local save exists; otherwise emit NO marker rather than guess (honest by omission).
	localHashes := sync.LocalSaveHashesForRom(client, cfg, romPath)
	matchesDevice := func(s romm.Save) bool {
		if s.ContentHash == nil {
			return false
		}
		for _, h := range localHashes {
			if strings.EqualFold(*s.ContentHash, h) {
				return true
			}
		}
		return false
	}

	markedCurrent := false
	for _, s := range saves {
		who := s.Emulator
		if len(s.DeviceSyncs) > 0 && s.DeviceSyncs[0].DeviceName != "" {
			who = s.DeviceSyncs[0].DeviceName
		}
		// Field 4 (optional — EXTENDS, never reorders fields 0-3):
		//   "UNAVAILABLE" — a ghost save (#63): the record reports 0 bytes, so its content
		//                   is missing on the server and a restore would fail. Flag it here
		//                   so the launcher can grey it out / warn instead of letting the
		//                   user pick a save that dies with the cryptic reason=download.
		//   "CURRENT"     — the single newest revision whose content matches the on-device
		//                   save. A 0-byte ghost can never match local bytes, so the two
		//                   markers are mutually exclusive by construction.
		mark := ""
		if s.FileSizeBytes <= 0 {
			mark = "\tUNAVAILABLE"
		} else if !markedCurrent && matchesDevice(s) {
			mark = "\tCURRENT"
			markedCurrent = true
		}
		fmt.Printf("%d\t%s\t%s\t%dKB%s\n",
			s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), who, s.FileSizeBytes/1024, mark)
	}
	os.Exit(0)
}

// runRestoreSave flashes one specific server save (by id) onto the local save file.
// Contract: RESULT restored=<0|1> [staged=<N>] [reason=<...>]. The save id is the
// positional arg.
//
// Flashback Pillar A — lose-proof, OFFLINE-FIRST: before overwriting the live save, the
// device's CURRENT save is preserved. We first try to push it to the timeline now; when
// that fails (offline), its bytes are STAGED and queued so they upload on the next sync
// instead of blocking the flashback. The flashback is never aborted just because we're
// offline — staged=N reports how many current saves were parked for later upload, so the
// launcher can say so honestly.
func runRestoreSave(client *romm.Client, cfg *config.Config, romPath, saveIDArg string) {
	id, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || id == 0 {
		fmt.Println("RESULT restored=0 reason=resolve")
		os.Exit(0)
	}
	saveID, perr := strconv.Atoi(strings.TrimSpace(saveIDArg))
	if perr != nil {
		fmt.Println("RESULT restored=0 reason=badid")
		os.Exit(0)
	}
	saves, err := client.GetSaves(romm.SaveQuery{RomID: id})
	if err != nil {
		fmt.Println("RESULT restored=0 reason=download")
		os.Exit(0)
	}
	var chosen romm.Save
	found := false
	for _, s := range saves {
		if s.ID == saveID {
			chosen = s
			found = true
			break
		}
	}
	if !found {
		fmt.Println("RESULT restored=0 reason=notfound")
		os.Exit(0)
	}

	// Preserve the current save before the overwrite. Push it to the timeline now; if
	// that doesn't land (offline), stage each current save file and queue it for later.
	staged := 0
	if current := sync.LocalSaveFilesForRom(client, cfg, romPath); len(current) > 0 {
		if !entryLanded(sync.PushSaveDirect(client, cfg, romPath)) {
			for _, f := range current {
				sp, serr := stageSaveFile(f)
				if serr != nil {
					fmt.Fprintf(os.Stderr, "flashback: WARN couldn't stage current save: %s\n", safeErr(serr))
					continue
				}
				if qerr := enqueueStaged(romPath, sp, filepath.Base(filepath.Dir(f))); qerr != nil {
					fmt.Fprintf(os.Stderr, "flashback: WARN couldn't queue staged save: %s\n", safeErr(qerr))
					_ = os.Remove(sp)
					continue
				}
				staged++
			}
		}
	}

	res := sync.RestoreSave(client, cfg, romPath, chosen)
	if res.Pulled() {
		fmt.Printf("RESULT restored=1 staged=%d\n", staged)
	} else {
		reason := "download"
		switch res.Outcome {
		case sync.PullResolveFail:
			reason = "resolve"
		case sync.PullGhostSave:
			// Ghost save (#63): the RomM record exists but its content is gone (404 /
			// empty). Surface a CLEAR, distinct reason so the launcher can tell the user
			// "this save is no longer available on the server" instead of the cryptic
			// reason=download. The current save was NOT overwritten (writeSave bailed
			// before the rename), and any staged current-save bytes remain queued.
			reason = "unavailable"
			fmt.Fprintf(os.Stderr, "flashback: save %d is a GHOST (record present, content missing) — %s\n", saveID, res.Reason)
		}
		fmt.Printf("RESULT restored=0 reason=%s staged=%d\n", reason, staged)
	}
	os.Exit(0)
}

// runSyncFeed lists recent server saves across the MAPPED platforms, deduped by save
// ID, newest-first, capped at 20. One tab-separated line per save:
//
//	<game>\t<YYYY-MM-DD HH:MM>\t<device>
//
// game = FileNameNoExt with any trailing 2-5 char rom-ext trimmed; device = the first
// device sync's DeviceName when present, else empty. Zero saves → print nothing.
func runSyncFeed(client *romm.Client, cfg *config.Config) {
	platforms, err := mappedPlatforms(client, cfg)
	if err != nil {
		os.Exit(0) // unreachable / no platforms — an empty feed, not a hard error
	}

	var saves []romm.Save
	seen := map[int]bool{}
	for _, p := range platforms {
		ps, gerr := client.GetSaves(romm.SaveQuery{PlatformID: p.ID})
		if gerr != nil {
			continue
		}
		for _, s := range ps {
			if !seen[s.ID] {
				seen[s.ID] = true
				saves = append(saves, s)
			}
		}
	}
	if len(saves) == 0 {
		os.Exit(0)
	}
	sort.Slice(saves, func(i, j int) bool { return saves[i].UpdatedAt.After(saves[j].UpdatedAt) })
	if len(saves) > 20 {
		saves = saves[:20]
	}
	for _, s := range saves {
		game := s.FileNameNoExt
		if e := filepath.Ext(game); len(e) >= 2 && len(e) <= 5 {
			game = strings.TrimSuffix(game, e)
		}
		device := ""
		if len(s.DeviceSyncs) > 0 && s.DeviceSyncs[0].DeviceName != "" {
			device = s.DeviceSyncs[0].DeviceName
		}
		fmt.Printf("%s\t%s\t%s\n", game, s.UpdatedAt.Format("2006-01-02 15:04"), device)
	}
	os.Exit(0)
}

// runRecent prints the single most-recently-played game across mapped platforms — the
// data source for the launcher's "Continue" tile. Output is ONE TAB-separated line:
//
//	<localRomPath>\t<game>\t<when>\t<device>
//
// localRomPath is the on-card stub/file path (the .m3u for multi-disc). Unreachable, no
// platforms, or no saves => prints nothing and exits 0 (the launcher falls back to its own
// local recents). One extra GetRom resolves the newest save's rom to a launchable path.
func runRecent(client *romm.Client, cfg *config.Config) {
	platforms, err := mappedPlatforms(client, cfg)
	if err != nil {
		os.Exit(0)
	}
	var newest romm.Save
	found := false
	for _, p := range platforms {
		ps, gerr := client.GetSaves(romm.SaveQuery{PlatformID: p.ID})
		if gerr != nil {
			continue
		}
		for _, s := range ps {
			if !found || s.UpdatedAt.After(newest.UpdatedAt) {
				newest = s
				found = true
			}
		}
	}
	if !found {
		os.Exit(0)
	}
	rom, rerr := client.GetRom(newest.RomID)
	if rerr != nil || rom.ID == 0 {
		os.Exit(0)
	}
	path := platform.LocalRomPath(cfg, rom)
	if path == "" {
		os.Exit(0)
	}
	game := rom.Name
	if game == "" {
		game = rom.FsNameNoExt
	}
	device := ""
	if len(newest.DeviceSyncs) > 0 {
		device = newest.DeviceSyncs[0].DeviceName
	}
	fmt.Printf("%s\t%s\t%s\t%s\n", path, game, newest.UpdatedAt.Format("2006-01-02 15:04"), device)
	os.Exit(0)
}

// mappedPlatforms returns the RomM platforms the user has a directory mapping for,
// fetching the platform list once. Only mapped platforms have a Roms/BIOS folder on
// the card, so BIOS download and the sync feed both scope to them.
func mappedPlatforms(client *romm.Client, cfg *config.Config) ([]romm.Platform, error) {
	all, err := client.GetPlatforms()
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	var out []romm.Platform
	for _, p := range all {
		if _, mapped := cfg.DirectoryMappings[p.FsSlug]; mapped {
			out = append(out, p)
		}
	}
	return out, nil
}

// runReportSession reports ONE finished play session to RomM (POST /api/play-sessions)
// so cross-device Continue / recently-played reflects time played on THIS device. It is
// pure telemetry and best-effort: it ALWAYS exits 0 and never blocks a game quit. The
// minarch shim brackets each launch and passes the rom path plus the start/end unix
// epochs; this resolves the rom_id locally, then posts a single-entry batch. Contract:
// RESULT reported=<0|1>. An older RomM without the endpoint (404) reports 0 silently.
func runReportSession(client *romm.Client, cfg *config.Config, romPath string, started, ended int64) {
	if reportSessionCore(client, cfg, romPath, started, ended) {
		fmt.Println("RESULT reported=1")
	} else {
		fmt.Println("RESULT reported=0")
	}
	os.Exit(0)
}

// reportSessionCore does the play-session report and returns true iff the server
// created the session. It prints NO RESULT line and never exits. Every diagnostic is a
// host-free PSFAIL/PSSKIP token on stderr (no URL, token, or device_id ever echoed).
func reportSessionCore(client *romm.Client, cfg *config.Config, romPath string, started, ended int64) bool {
	// The server REJECTS (422) any session whose end_time is not strictly after
	// start_time, so enforce the window here before spending a request.
	if started <= 0 || ended <= 0 || ended <= started {
		fmt.Fprintf(os.Stderr, "PSFAIL window start=%d end=%d\n", started, ended)
		return false
	}
	id, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || id == 0 {
		fmt.Fprintf(os.Stderr, "PSFAIL resolve: %s\n", filepath.Base(romPath))
		return false
	}
	deviceID := ""
	if len(cfg.Hosts) > 0 {
		deviceID = cfg.ActiveHost().DeviceID // MULTI-USER: report the session under the active profile's device
	}
	entry := romm.PlaySessionEntry{
		RomID:      id,
		StartTime:  time.Unix(started, 0).UTC().Format(time.RFC3339),
		EndTime:    time.Unix(ended, 0).UTC().Format(time.RFC3339),
		DurationMs: (ended - started) * 1000,
	}
	resp, err := client.ReportPlaySessions(deviceID, []romm.PlaySessionEntry{entry})
	if err != nil {
		var se *romm.StatusError
		if errors.As(err, &se) && se.Code == 404 {
			// Older RomM without /api/play-sessions — graceful no-op, not a failure.
			fmt.Fprintln(os.Stderr, "PSSKIP play-sessions unsupported by server")
			return false
		}
		fmt.Fprintf(os.Stderr, "PSFAIL report rom=%d: %s\n", id, safeErr(err))
		return false
	}
	return resp.CreatedCount > 0
}

