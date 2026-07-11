package main

// Native-menu CLI modes. A C MinUI launcher (minui.c) shells out to this binary and
// parses the line output, so the stdout contracts here are EXACT — no stray prints
// reach stdout. Diagnostics go to stderr and are kept host-free (no URL, token, or
// device_id ever appears in any line, file, or log). §3 download/BIOS orchestration
// lives here too.

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"lodor/catalog"
	"lodor/config"
	"lodor/cover"
	"lodor/covercancel"
	"lodor/fsutil"
	"lodor/platform"
	"lodor/ranet"
	"lodor/romm"
	"lodor/sync"
)

// armDownloadCancel makes an INTERACTIVE download's transfer loops honor the
// launcher's B-press sentinel (/tmp/lodor-cover-cancel) — a REAL cancel, not the
// old wait-out-the-transfer soft-cancel: clear any leftover sentinel first (belt —
// the launcher rm -fs it at op start too), then point the client's between-chunks
// CancelCheck at it. Armed for --download / --fetch-next-disc / --fetch-discs
// ONLY; the daemons' --prefetch-discs and --download-queue never arm it, so a
// user cancelling a foreground op can't kill a background transfer mid-disc
// (sentinel absent = never polled = no behavior change anywhere else).
func armDownloadCancel(client *romm.Client) {
	covercancel.Clear()
	client.CancelCheck = covercancel.Requested
}

// extractTimeout bounds a single archive (7z/zip) extraction. Generous enough for a
// multi-GB CHD/ISO on slow handheld storage, finite so a wedged decoder can never hang
// the download path forever (HARDENING — the unbounded-wait class).
const extractTimeout = 20 * time.Minute

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
	// Legacy full-list .m3u → local-only before any network work (lodor#7): a
	// launcher-routed --download on a partially-downloaded legacy card must leave
	// the game launchable on its present discs even when the fetch fails offline.
	migrateLegacyM3U(romPath)
	ok, st := downloadRomCoreStats(client, cfg, romPath)
	// "cancelled=1" is ADDITIVE (parsers key on the downloaded= token): the user's
	// B-press stopped the transfer loop — partial kept, nothing failed dishonestly.
	cancelSuffix := ""
	if st.cancelled {
		cancelSuffix = " cancelled=1"
	}
	if st.multi {
		// Multi-disc (disc-1-first, lodor#7): the trailing disc fields are ADDITIVE —
		// every existing parser keys on the exact "downloaded=<0|1>" token and ignores
		// the rest of the line. downloaded=1 means the game is LAUNCHABLE (its first
		// missing disc landed, or every disc was already present) — NOT that every
		// disc is on the card; discs_present vs discs_total carries that truth.
		fmt.Printf("RESULT downloaded=%d discs_total=%d discs_present=%d discs_fetched=%d%s\n",
			b2i(ok), st.total, st.present, st.fetched, cancelSuffix)
	} else if ok {
		fmt.Println("RESULT downloaded=1")
	} else {
		fmt.Println("RESULT downloaded=0" + cancelSuffix)
	}
	exitMode(0)
}

// discStats is the honest per-disc accounting a multi-disc download reports on its
// RESULT line (disc-1-first, lodor#7): how many discs the playlist lists, how many
// hold real verified bytes on the card AFTER this run, and how many this run actually
// transferred. multi=false for a single-file ROM (the fields are then meaningless).
type discStats struct {
	multi   bool
	total   int
	present int
	fetched int
	// cancelled = the user's B-press sentinel stopped this run's transfer loop
	// (REAL cancel — partial .tmp kept for resume, verified discs kept + listed).
	// Reported additively as "cancelled=1" on the RESULT line; also set for a
	// cancelled single-file transfer (multi=false).
	cancelled bool
}

// complete reports whether every listed disc is on the card.
func (s discStats) complete() bool { return s.multi && s.total > 0 && s.present == s.total }

// downloadRomCore does the actual single-ROM download work and returns true iff a
// playable, hash-verified file landed. It prints NO RESULT line and never exits — so
// it is shared by both --download (runDownloadRom) and --download-queue
// (runDownloadQueue). Every progress/phase side-channel write and every host-free
// DLFAIL stderr diagnostic from the original --download path is preserved verbatim.
func downloadRomCore(client *romm.Client, cfg *config.Config, romPath string) bool {
	ok, _ := downloadRomCoreStats(client, cfg, romPath)
	return ok
}

// downloadRomCoreStats is downloadRomCore plus the multi-disc accounting the RESULT
// line reports (single-file ROMs return a zero discStats with multi=false).
func downloadRomCoreStats(client *romm.Client, cfg *config.Config, romPath string) (bool, discStats) {
	var st discStats
	ok := downloadRomCoreInner(client, cfg, romPath, &st)
	return ok, st
}

func downloadRomCoreInner(client *romm.Client, cfg *config.Config, romPath string, st *discStats) bool {
	writeProgress(0)

	id, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || id == 0 {
		fmt.Fprintf(os.Stderr, "DLFAIL resolve: %s\n", filepath.Base(romPath))
		return false
	}
	rom, err := client.GetRom(id)
	if err != nil || rom.ID == 0 {
		noteAuthErr(err)
		fmt.Fprintf(os.Stderr, "DLFAIL getrom id=%d\n", id)
		return false
	}
	// SECURITY (path-traversal belt): every server-supplied name this download will
	// join under the card (platform_fs_slug, fs_name_no_ext, each file_name) MUST be a
	// safe single path component. A malicious/compromised RomM returning
	// file_name:"../../../../.system/<plat>/bin/lodor-sync" is rejected here before any
	// path is computed — hash verify is no defence (the server supplies the hash). This
	// centralises the check for the single-file, stub, archive AND multi-disc paths that
	// all flow through this rom. The MultiDiscDir/discDest containment assertions below
	// are the suspenders.
	if !platform.ValidateRomNames(rom) {
		fmt.Fprintf(os.Stderr, "DLFAIL unsafe-name rom=%d\n", rom.ID)
		writePhase("This game's server filenames are invalid")
		writeProgress(0)
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

	// V5 gate (C1 design audit): a download FILL removes/overwrites whatever sits
	// at dest, so dest must be OURS — manifest-owned, or an unmanifested stub the
	// triple gate re-claims (legacy card). A USER's 0-byte file that happens to
	// carry a canonical name (the merge dedup edge) is neither, and their REAL file
	// is never replaced with the server's copy. Honest refusal, nothing touched.
	man := platform.LoadManifest()
	if !downloadDestAllowed(cfg, man, dest) {
		fmt.Fprintf(os.Stderr, "DLFAIL not-lodor-managed: %s\n", filepath.Base(dest))
		writePhase("This file isn't managed by Lodor")
		writeProgress(0)
		return false
	}

	romName := rom.Name
	if romName == "" {
		romName = filepath.Base(rom.FsName)
	}

	// MULTI-DISC (folder-per-game, has_multiple_files): RomM serves all discs as a
	// mod_zip, OR each disc individually via a single file_ids selector. We download
	// disc-by-disc (streamed, no OOM; per-disc hash-verified) and write the .m3u.
	// DISC-1-FIRST (lodor#7): the launch path fetches ONLY the first missing disc
	// (budget=1) — later discs stay 0-byte stubs until --fetch-next-disc /
	// --fetch-discs / the daemon prefetch completes the set.
	if rom.HasMultipleFiles {
		return downloadMultiDiscCore(client, cfg, rom, romName, man, 1, st)
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

	// ARCHIVE EXTRACT: the server stores this game in a .7z the standalone emulator can't
	// open (NDS/DraStic). The stub is named with the raw extension (LocalRomPath remaps via
	// onDiskExt), so dest is e.g. ".../Advance Wars.nds"; download the .7z, verify it, and
	// extract the inner ROM into dest. Paid once — the raw file persists.
	if _, ok := platform.ArchiveExtractTargetForRom(rom); ok {
		return downloadAndExtractArchive(client, cfg, rom, dest, romName, man)
	}

	writePhase(fmt.Sprintf("Downloading %s…", romName))

	// SECURITY (containment suspenders): assert the final destination resolves inside
	// Roms/ before creating/renaming any bytes. dest is the launcher-passed romPath; a
	// poisoned directory_mapping or slug that survived the belt cannot land a write
	// outside the ROM tree.
	if !platform.PathWithinRoms(dest) {
		fmt.Fprintf(os.Stderr, "DLFAIL escape-dest rom=%d\n", rom.ID)
		writePhase("This game's destination path is invalid")
		writeProgress(0)
		return false
	}

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
	// FAT32-durable: fsync streamed ROM bytes before rename (streaming path — too
	// large to buffer through fsutil). A Sync failure folds into the download gate.
	syncErr := out.Sync()
	cErr := out.Close()
	if derr != nil || syncErr != nil || cErr != nil || n == 0 {
		noteAuthErr(derr)
		// KEEP the partial .tmp on a transfer error so the NEXT --download (or the
		// download-queue retry) resumes from here instead of restarting at zero. A
		// corrupt/stale partial is caught later by the hash verify (which deletes the
		// bad file), so retaining it is safe.
		if errors.Is(derr, romm.ErrCancelled) {
			// REAL user cancel (B-press sentinel): same keep-the-partial contract,
			// reported as a cancel — never dressed up as a network failure.
			if st != nil {
				st.cancelled = true
			}
			fmt.Fprintf(os.Stderr, "DLCANCEL rom=%d (partial .tmp kept for resume)\n", rom.ID)
		} else {
			fmt.Fprintf(os.Stderr, "DLFAIL download rom=%d\n", rom.ID)
		}
		writeProgress(0)
		return false
	}
	writeProgress(90)

	_ = os.Remove(dest) // remove the stub before rename (rename over a dir would fail)
	if rErr := os.Rename(tmp, dest); rErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL rename rom=%d\n", rom.ID)
		_ = os.Remove(tmp)
		restoreStub(dest)
		return false
	}
	fsutil.SyncDir(filepath.Dir(dest)) // FAT32: persist the ROM rename into the folder

	writePhase("Verifying…")
	if vErr := verifyRomHash(rom, dest); vErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL verify rom=%d: %s\n", rom.ID, vErr)
		_ = os.Remove(dest)
		restoreStub(dest)
		writeProgress(0)
		return false
	}
	writeProgress(100)

	fetchRomCover(client, rom, dest, man)
	recordDownload(man, dest, rom.ID)

	return true
}

// downloadDestAllowed is the V5 choke-point check: may the download path
// remove/replace what currently sits at dest?
//
//   - nothing at dest            -> yes (purely additive write)
//   - manifest-owned             -> yes (our stub being filled, or our own
//     download being re-pulled)
//   - unmanifested 0-byte STUB   -> only via the ReclaimableStub triple gate
//     (0-byte + cloud-marker on marker-baking hosts + catalog-resolves) — the
//     legacy-card heal; a user's 0-byte file has no marker and is refused
//   - unmanifested REAL file     -> never (the user's game is not ours to replace)
func downloadDestAllowed(cfg *config.Config, man *platform.Manifest, dest string) bool {
	fi, err := os.Stat(dest)
	if err != nil {
		return true // nothing there — additive
	}
	if fi.IsDir() {
		return false
	}
	if man.Owns(dest) {
		return true
	}
	if fi.Size() != 0 {
		return false // real, unmanifested bytes: not ours
	}
	return platform.ReclaimableStub(dest, func(p string) bool {
		_, ok := catalog.ResolveRomID(cfg, p)
		return ok
	})
}

// recordDownload marks a landed download in the mirror-owned manifest (one atomic
// save per download). Failure is non-fatal: an unrecorded download is merely
// demoted to user-like until the next mirror pass re-records it — it can't be
// evicted in the window, never the unsafe direction.
func recordDownload(man *platform.Manifest, dest string, romID int) {
	man.Record(dest, platform.ManifestDownload, romID)
	if err := man.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "MANIFEST save failed after download: %v\n", err)
	}
}

// restoreStub re-creates the 0-byte cloud stub at dest after a failed download that
// had already removed it (rename/extract/hash-verify failures — the early transfer
// failures never touch the stub). A launch-time failure must leave the SAME honest
// on-card state it found: a 0-byte stub the emulator fails fast on and the library
// still lists — never a deleted entry, never a corrupt partial file (task #120).
// No-op if something already exists at dest.
func restoreStub(dest string) {
	if dest == "" {
		return
	}
	if _, err := os.Stat(dest); err == nil {
		return
	}
	if f, err := os.Create(dest); err == nil {
		_ = f.Close()
	}
}

// downloadAndExtractArchive handles a ROM stored in a .7z the standalone emulator can't
// open (NDS/DraStic): download the .7z, hash-verify it (RomM's hash is of the stored .7z),
// then extract the single inner ROM into dest (which is already named with the raw .nds
// extension, via onDiskExt). Logs the real on-device extract time (EXTRACT line).
func downloadAndExtractArchive(client *romm.Client, cfg *config.Config, rom romm.Rom, dest, romName string, man *platform.Manifest) bool {
	// SECURITY (containment suspenders): this path also writes real bytes to dest (via
	// the .7z.dl temp + extract), so assert dest is inside Roms/ before any create.
	if !platform.PathWithinRoms(dest) {
		fmt.Fprintf(os.Stderr, "DLFAIL escape-dest rom=%d\n", rom.ID)
		writePhase("This game's destination path is invalid")
		writeProgress(0)
		return false
	}
	writePhase(fmt.Sprintf("Downloading %s…", romName))
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	arc := dest + ".7z.dl"
	out, oErr := os.OpenFile(arc, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if oErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL create rom=%d\n", rom.ID)
		writeProgress(0)
		return false
	}
	lastPct := -1
	onProg := func(done, total int64) {
		if total <= 0 {
			return
		}
		pct := int(done * 80 / total) // reserve 80→100 for verify + extract
		if pct > lastPct {
			lastPct = pct
			writeProgress(pct)
		}
	}
	n, derr := client.DownloadRomContentTo(rom.ID, rom.FsName, out, onProg)
	cErr := out.Close()
	if derr != nil || cErr != nil || n == 0 {
		noteAuthErr(derr)
		_ = os.Remove(arc)
		fmt.Fprintf(os.Stderr, "DLFAIL download rom=%d\n", rom.ID)
		writeProgress(0)
		return false
	}
	// Extract FIRST, then verify the extracted ROM: RomM records the hash of the
	// DECOMPRESSED content (No-Intro/Redump DATs hash the raw .nds, not the .7z wrapper),
	// so the integrity gate must run on the extracted file, not the archive.
	writeProgress(82)
	writePhase("Extracting…")
	t0 := time.Now()
	if eErr := extract7zInto(arc, dest); eErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL extract rom=%d: %s\n", rom.ID, eErr)
		_ = os.Remove(arc)
		_ = os.Remove(dest)
		restoreStub(dest)
		writeProgress(0)
		return false
	}
	ms := time.Since(t0).Milliseconds()
	_ = os.Remove(arc)
	writeProgress(92)
	writePhase("Verifying…")
	if vErr := verifyRomHash(rom, dest); vErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL verify rom=%d: %s\n", rom.ID, vErr)
		_ = os.Remove(dest)
		restoreStub(dest)
		writeProgress(0)
		return false
	}
	if st, sErr := os.Stat(dest); sErr == nil {
		fmt.Fprintf(os.Stderr, "EXTRACT 7z rom=%d raw=%dB %dms\n", rom.ID, st.Size(), ms)
	}
	writeProgress(100)
	fetchRomCover(client, rom, dest, man)
	recordDownload(man, dest, rom.ID)
	return true
}

// extract7zInto extracts the single ROM from a .7z into dest by exec-ing a bundled static
// 7-Zip binary (7zz). We exec the C decoder rather than a pure-Go lib because the pure-Go
// path pulls a ppmd dep that doesn't compile on 32-bit ARM (armhf) and decodes ~10x slower.
// `x -so` streams the (single) archived file to stdout; we write it to dest atomically.
func extract7zInto(archivePath, dest string) error {
	bin := find7zz()
	if bin == "" {
		return fmt.Errorf("no 7zz binary (bundle Tools/<plat>/Lodor.pak/bin/7zz)")
	}
	// Extract to a temp dir on the SAME filesystem (so the final rename is atomic), then
	// move the largest extracted file (the ROM) into place. Using a dir extract avoids the
	// `-so` stdout-mixing quirk that varies across 7-Zip/p7zip versions.
	outdir, err := os.MkdirTemp(filepath.Dir(dest), ".7zx-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(outdir)
	// HARDENING: bound the extraction. A bare exec.Command().Run() waits forever, so a
	// wedged/hung 7zz child (corrupt archive, stuck FUSE mount, a decoder spinning on a
	// malformed header) would freeze the download path with no ceiling. CommandContext
	// kills the child on deadline; a legitimately large CHD/ISO extracts well inside it.
	ctx, cancel := context.WithTimeout(context.Background(), extractTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "e", "-y", "-o"+outdir, archivePath)
	cmd.Stdout = io.Discard
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("7z: extraction timed out after %s", extractTimeout)
		}
		return fmt.Errorf("7z: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	entries, err := os.ReadDir(outdir)
	if err != nil {
		return err
	}
	var best string
	var bestSize int64 = -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, ferr := e.Info()
		if ferr != nil {
			continue
		}
		if fi.Size() > bestSize {
			bestSize = fi.Size()
			best = filepath.Join(outdir, e.Name())
		}
	}
	if best == "" {
		return fmt.Errorf("7z: no file extracted")
	}
	_ = os.Remove(dest)
	return os.Rename(best, dest)
}

// find7zz locates a 7-Zip CLI: $LODOR_7ZZ, then ./bin/7zz (the engine runs CWD = the pak
// dir, where we bundle a static 7zz), then common names on PATH (covers p7zip too).
func find7zz() string {
	if p := os.Getenv("LODOR_7ZZ"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat("bin/7zz"); err == nil {
		if abs, aerr := filepath.Abs("bin/7zz"); aerr == nil {
			return abs
		}
	}
	for _, name := range []string{"7zz", "7zzs", "7za", "7zr", "7z"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
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
// man records a SAVED cover as mirror-owned (kind=cover) so uninstall can remove
// it; the caller persists the manifest (recordDownload's save covers it).
func fetchRomCover(client *romm.Client, rom romm.Rom, coverAnchor string, man *platform.Manifest) {
	if cp := rom.CoverPath(); cp != "" {
		out, cerr := cover.FetchAndSave(client, cp, coverAnchor, false)
		if out == cover.OutcomeError && cerr != nil {
			fmt.Fprintf(os.Stderr, "COVERWARN rom=%d: %s\n", rom.ID, safeErr(cerr))
		}
		if out == cover.OutcomeSaved {
			man.Record(cover.MediaPath(coverAnchor), platform.ManifestCover, rom.ID)
		}
	}
}

// downloadMultiDiscCore downloads a folder-per-game multi-file ROM's discs and writes
// the .m3u that ties them together — DISC-1-FIRST (lodor#7 hybrid). budget caps how
// many discs this run may TRANSFER: 1 = the launch path / --fetch-next-disc (first
// missing disc in canonical order — the disc the player is about to need); < 0 =
// unlimited (--fetch-discs / the daemon prefetch completes the set).
//
// LOCAL-ONLY .m3u (hardware-verified regression fix, 2026-07-11): the playlist on
// card lists ONLY discs with real bytes. The shipped LodorOS launcher (minui.c,
// unrebuildable) parses the .m3u pre-launch and refuses to launch while ANY listed
// disc is missing/0-byte — a full-list .m3u with stubs meant one disc downloaded
// per launch and the game NEVER launched. So:
//   - the FULL canonical disc list (server/natural order) is persisted on the
//     .m3u's mirror-manifest record (SetDiscs) before any byte moves — that record,
//     not the playlist, is what --check-rom / --prefetch-discs / evict key off;
//   - the .m3u is atomically REWRITTEN after each disc verifies (canonical order,
//     present discs only), so every landed disc is playable immediately;
//   - missing discs beyond the budget still leave honest 0-byte stubs in the
//     per-game folder (additive; the evict sweep owns their cleanup), but they are
//     NEVER listed in the playlist.
//
// Each disc is fetched INDIVIDUALLY via a single file_ids selector (verified live:
// one file_ids → that file's raw bytes; two or more → a mod_zip). This is chosen over
// pulling RomM's all-files mod_zip deliberately:
//   - STREAMED to disk (io.Copy), so a 1.3 GB game never sits in 128 MB of RAM;
//   - each disc runs the SAME integrity gate as the single-file path (verifyFileHash:
//     non-CHD discs are checked against files[].sha1/md5; .chd discs are accepted on a
//     valid streamed transfer because RomM records a CHD's INTERNAL disc SHA1, not the
//     container's — see isCHD; a mod_zip would expose no per-member hash to check at all);
//   - a failed TRANSFER fails this run honestly (ok=false) but KEEPS every
//     previously-verified disc — per-disc resume — and every verified disc is
//     ALREADY in the playlist (rewritten as each disc landed), so the game plays
//     on the discs it has; the next run fetches only what's still missing.
//
// Discs land in <Roms>/<system>/<FsNameNoExt>/<disc>.chd; the .m3u (at <FsNameNoExt>.m3u
// beside that folder) lists "<FsNameNoExt>/<disc>" lines, resolved relative to the m3u
// dir by both the launcher (getFirstDisc) and the emulator's m3u loader.
//
// ok=true means the game is LAUNCHABLE: every disc BEFORE the first fetched one was
// already present, so on success disc 1 always has real bytes. st (optional) carries
// the honest total/present/fetched accounting for the caller's RESULT line.
func downloadMultiDiscCore(client *romm.Client, cfg *config.Config, rom romm.Rom, romName string, man *platform.Manifest, budget int, st *discStats) bool {
	if st != nil {
		st.multi = true
	}
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
	// SECURITY (containment suspenders): both the .m3u and the per-game disc folder must
	// resolve inside Roms/. The per-component belt (ValidateRomNames, run in the caller)
	// already vetted the slug + FsNameNoExt these are built from; this catches any
	// poisoned directory_mapping folder that survived it.
	if !platform.PathWithinRoms(m3uPath) || !platform.PathWithinRoms(discDir) {
		fmt.Fprintf(os.Stderr, "DLFAIL multidisc escape-dest rom=%d\n", rom.ID)
		writePhase("This game's destination path is invalid")
		writeProgress(0)
		return false
	}

	// V5 gate, m3u leg: the canonical m3uPath may differ from the launcher-passed
	// (marked) romPath the caller already gated — e.g. an ADOPTED user .m3u in
	// merge mode. It is removed before the playlist rename below, so it must pass
	// the same ownership check.
	if !downloadDestAllowed(cfg, man, m3uPath) {
		fmt.Fprintf(os.Stderr, "DLFAIL not-lodor-managed m3u rom=%d\n", rom.ID)
		writePhase("This file isn't managed by Lodor")
		writeProgress(0)
		return false
	}

	// V5 gate, disc-folder leg: the per-game disc folder is written into (and
	// RemoveAll'd on failure by cleanupMultiDisc), so a PRE-EXISTING folder there
	// that the manifest doesn't own — the user's own same-named game folder — must
	// refuse the download rather than risk their files. A folder we're about to
	// create is recorded as ours FIRST (record-intent-then-act).
	if fi, serr := os.Stat(discDir); serr == nil {
		if !fi.IsDir() || !man.Owns(discDir) {
			fmt.Fprintf(os.Stderr, "DLFAIL not-lodor-managed discdir rom=%d\n", rom.ID)
			writePhase("This file isn't managed by Lodor")
			writeProgress(0)
			return false
		}
	} else {
		man.Record(discDir, platform.ManifestFolder, rom.ID)
		if err := man.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "MANIFEST save failed before multidisc: %v\n", err)
		}
	}

	if mkErr := os.MkdirAll(discDir, 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "DLFAIL multidisc mkdir rom=%d\n", rom.ID)
		return false
	}

	folderName := filepath.Base(discDir) // == rom.FsNameNoExt; the m3u-relative prefix

	// Defensive disc ordering: RomM is expected to return Files in disc order, but
	// don't trust it — a wrong order would boot "Disc 2" first and desync saves.
	// Stable natural sort by FileName so "Disc 1" < "Disc 2" < "Disc 10" (plain
	// lexical order puts "10" before "2"). Sort a copy; never mutate rom.Files.
	discFiles := make([]romm.RomFile, len(rom.Files))
	copy(discFiles, rom.Files)
	sort.SliceStable(discFiles, func(a, b int) bool {
		return naturalLess(discFiles[a].FileName, discFiles[b].FileName)
	})

	total := len(discFiles)
	if st != nil {
		st.total = total
	}

	// PRE-PASS (fail early, write late): validate every disc's server name +
	// destination and take a presence census BEFORE any byte moves. A validation
	// failure aborts with the card untouched — previously-landed discs always stay.
	type discPlan struct {
		file    romm.RomFile
		dest    string
		present bool
	}
	plans := make([]discPlan, 0, total)
	for i, f := range discFiles {
		if f.ID == 0 || f.FileName == "" {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc rom=%d: disc %d missing id/name\n", rom.ID, i+1)
			writeProgress(0)
			return false
		}
		// SECURITY (path-traversal belt, per-disc): each disc's server file_name must be a
		// safe single component before it is joined into discDir. ValidateRomNames in the
		// caller already vetted the whole rom, but re-check the exact value used here so the
		// join can never absorb "../" or a separator.
		if !platform.SafeName(f.FileName) {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc unsafe-name rom=%d disc=%d\n", rom.ID, i+1)
			writePhase("A disc's server filename is invalid")
			writeProgress(0)
			return false
		}
		discDest := filepath.Join(discDir, f.FileName)
		// SECURITY (containment suspenders, per-disc): assert the joined disc destination
		// stays inside Roms/ before any create/rename.
		if !platform.PathWithinRoms(discDest) {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc escape-dest rom=%d disc=%d\n", rom.ID, i+1)
			writePhase("A disc's destination path is invalid")
			writeProgress(0)
			return false
		}
		// Present and hash-clean from a prior run? It will be skipped, not re-pulled
		// (idempotent per-disc resume — same gate as before, taken once up front).
		present := existsNonEmpty(discDest) && verifyFileHash(discDest, f.Sha1Hash, f.Md5Hash) == nil
		plans = append(plans, discPlan{file: f, dest: discDest, present: present})
	}
	missing := 0
	for _, p := range plans {
		if !p.present {
			missing++
		}
	}
	// The progress bar spans the discs this run will actually TRANSFER — an
	// already-present disc or a beyond-budget stub never moves it (honest bar).
	toFetch := missing
	if budget >= 0 && toFetch > budget {
		toFetch = budget
	}

	// CANONICAL DISC LIST → manifest, before any byte moves (local-only .m3u): the
	// playlist stops carrying the full set, so the full set is recorded fact on the
	// .m3u's manifest entry (it survives stub↔download kind flips and evict). The
	// entry normally exists already (mirror stub / prior download); if not, record
	// the stub intent first — downloadDestAllowed vetted this path above.
	canonLines := make([]string, 0, total)
	for _, p := range plans {
		canonLines = append(canonLines, folderName+"/"+p.file.FileName)
	}
	if _, owned := man.Entry(m3uPath); !owned {
		man.Record(m3uPath, platform.ManifestStub, rom.ID)
	}
	man.SetDiscs(m3uPath, canonLines)
	if err := man.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "MANIFEST save failed before multidisc discs: %v\n", err)
	}

	// writeLocalM3U atomically (re)writes the playlist with the discs that hold real
	// verified bytes RIGHT NOW, canonical order. Zero present discs = empty (0-byte)
	// playlist — exactly the stub shape the launch path already owns. Unchanged
	// content writes nothing (no FAT32 churn on an idempotent relaunch). This is
	// also the legacy-card migration: a full-list .m3u still referencing stubs is
	// normalized to local-only the first time any fetch path touches the game.
	writeLocalM3U := func() bool {
		var lines []string
		for _, p := range plans {
			if p.present {
				lines = append(lines, folderName+"/"+p.file.FileName)
			}
		}
		content := ""
		if len(lines) > 0 {
			content = strings.Join(lines, "\n") + "\n"
		}
		if cur, rerr := os.ReadFile(m3uPath); rerr == nil && string(cur) == content {
			return true
		}
		if wErr := fsutil.WriteFileAtomicString(m3uPath, content, 0o644); wErr != nil {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc m3u-write rom=%d\n", rom.ID)
			return false
		}
		return true
	}

	fetched := 0
	writeProgress(0)
	for i, p := range plans {
		if p.present {
			if st != nil {
				st.present++
			}
			continue
		}
		if budget >= 0 && fetched >= budget {
			// Beyond this run's budget (disc-1-first): leave an honest 0-byte stub in
			// the per-game folder (NOT in the playlist). ADDITIVE only — anything
			// already at the path (even a stale partial) is left alone; the per-game
			// folder is manifest-owned, so the stub is ours to place.
			ensureDiscStub(p.dest)
			continue
		}
		// REAL CANCEL (B-press sentinel), checked between discs before committing to
		// the next transfer: everything fetched so far is verified, already listed in
		// the local-only .m3u, and stays. Only reached when a transfer WOULD start —
		// a cancel after the budget is spent never fails a completed run.
		if client.CancelCheck != nil && client.CancelCheck() {
			if st != nil {
				st.cancelled = true
			}
			fmt.Fprintf(os.Stderr, "DLCANCEL multidisc rom=%d before disc=%d\n", rom.ID, i+1)
			writeProgress(0)
			return false
		}
		writePhase(fmt.Sprintf("Downloading %s — disc %d/%d…", romName, i+1, total))

		tmp := p.dest + ".tmp"
		// RESUMABLE (parity with the single-file path): a partial .tmp from an
		// interrupted run resumes from its end via an HTTP Range request instead of
		// restarting at byte 0 — a dropped multi-hundred-MB disc no longer costs the
		// whole transfer again. Open WITHOUT O_TRUNC so the partial is preserved; the
		// resume transport truncates/seeks as the server's 206/200/416 response
		// dictates (a server that can't compose file_ids with Range answers 200 and
		// the disc is rewritten clean from byte 0).
		var discStart int64
		if dfi, statErr := os.Stat(tmp); statErr == nil && dfi.Size() > 0 {
			discStart = dfi.Size()
		}
		out, cErr := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY, 0o644)
		if cErr != nil {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc create rom=%d disc=%d\n", rom.ID, i+1)
			writeProgress(0)
			return false
		}
		// Real progress: this transfer spans [fetched/toFetch, (fetched+1)/toFetch] of
		// the overall bar; move smoothly within that band as its bytes stream. On a
		// resume, done is primed with the on-disk offset against the full total, so
		// the bar picks up where it left off rather than jumping backward.
		discBase := fetched * 100 / toFetch
		discSpan := (fetched+1)*100/toFetch - discBase
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
		n, dErr := client.DownloadRomFileResumeTo(rom.ID, rom.FsName, p.file.ID, out, discStart, onProg)
		// FAT32-durable: fsync the streamed disc bytes before rename so a power-yank
		// can't zero a "renamed-in" .chd (streaming path — too large to buffer through
		// fsutil, so we fsync in place). A Sync error folds into the download-failed gate.
		syncErr := out.Sync()
		cerr2 := out.Close()
		if dErr != nil || syncErr != nil || cerr2 != nil || n == 0 {
			noteAuthErr(dErr)
			if errors.Is(dErr, romm.ErrCancelled) {
				// REAL user cancel mid-disc: keep the partial, report the cancel.
				if st != nil {
					st.cancelled = true
				}
				fmt.Fprintf(os.Stderr, "DLCANCEL multidisc rom=%d disc=%d (partial .tmp kept for resume)\n", rom.ID, i+1)
			} else {
				fmt.Fprintf(os.Stderr, "DLFAIL multidisc download rom=%d disc=%d: %s\n", rom.ID, i+1, safeErr(dErr))
			}
			// KEEP the partial .tmp so the next run resumes this disc from here
			// instead of restarting at zero — same contract as the single-file path.
			// A stale/corrupt partial self-heals via the resume transport's 416
			// handling and the per-disc hash verify (which deletes a bad disc).
			// Previously-verified discs STAY (a partial multi-disc is a valid on-card
			// state under disc-1-first) and are already listed in the local-only .m3u.
			writeProgress(0)
			return false
		}
		if rErr := os.Rename(tmp, p.dest); rErr != nil {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc rename rom=%d disc=%d\n", rom.ID, i+1)
			_ = os.Remove(tmp)
			writeProgress(0)
			return false
		}
		fsutil.SyncDir(discDir) // FAT32: persist the disc rename into the folder

		// Per-disc hash verify (same gate as single-file). A .chd or a disc with no
		// recorded hash is accepted (parity with verifyRomHash); a real mismatch
		// deletes the bad disc and fails this run — never a corrupt disc left behind.
		if vErr := verifyFileHash(p.dest, p.file.Sha1Hash, p.file.Md5Hash); vErr != nil {
			fmt.Fprintf(os.Stderr, "DLFAIL multidisc verify rom=%d disc=%d: %s\n", rom.ID, i+1, vErr)
			_ = os.Remove(p.dest)
			writeProgress(0)
			return false
		}

		fetched++
		plans[i].present = true
		if st != nil {
			st.present++
			st.fetched++
		}
		// Append the verified disc to the playlist NOW (atomic rewrite, canonical
		// order): if a later disc's transfer fails, everything landed so far is
		// already listed and playable — the launcher's gate sees only real bytes.
		if !writeLocalM3U() {
			writeProgress(0)
			return false
		}
		writeProgress(fetched * 100 / toFetch)
	}

	// Final playlist write (idempotent when the loop already wrote it): local-only,
	// FAT32-atomic (temp + fsync + rename + dir fsync); the rename overwrites any
	// 0-byte stub at that path. This is also the pure-migration path — a legacy
	// full-list .m3u with nothing left to fetch normalizes here.
	writePhase("Writing playlist…")
	if !writeLocalM3U() {
		return false
	}
	writeProgress(100)

	fetchRomCover(client, rom, m3uPath, man)
	recordDownload(man, m3uPath, rom.ID) // the .m3u is the evictable download anchor

	return true
}

// naturalLess reports whether a < b under a natural (human) ordering, where runs of
// ASCII digits compare by numeric value rather than lexically — so "Disc 2" sorts
// before "Disc 10". Non-digit runs compare byte-wise, case-insensitively for ASCII
// letters so casing never reorders discs. Deterministic and allocation-light; used to
// order multi-disc playlist entries defensively (see runMultiDisc).
func naturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ai, bi := a[i], b[j]
		aDig, bDig := ai >= '0' && ai <= '9', bi >= '0' && bi <= '9'
		if aDig && bDig {
			// Compare two digit runs numerically: skip leading zeros, then longer run
			// (more significant digits) wins; equal length breaks on first difference.
			si, sj := i, j
			for i < len(a) && a[i] == '0' {
				i++
			}
			for j < len(b) && b[j] == '0' {
				j++
			}
			ns, ms := i, j
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			la, lb := i-ns, j-ms
			if la != lb {
				return la < lb
			}
			if d := compareASCII(a[ns:i], b[ms:j]); d != 0 {
				return d < 0
			}
			// Numerically equal: the run WITH more leading zeros ("03" vs "3") sorts
			// first — deterministic tiebreak so ordering is stable.
			if (ns-si) != (ms-sj) {
				return (ns - si) > (ms - sj)
			}
			continue
		}
		la, lb := lowerASCII(ai), lowerASCII(bi)
		if la != lb {
			return la < lb
		}
		i++
		j++
	}
	return len(a)-i < len(b)-j
}

// compareASCII returns -1/0/1 comparing two equal-length digit strings lexically.
func compareASCII(a, b string) int {
	for k := 0; k < len(a) && k < len(b); k++ {
		if a[k] != b[k] {
			if a[k] < b[k] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// lowerASCII lowercases a single ASCII byte so disc ordering ignores letter case.
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// ensureDiscStub creates a 0-byte placeholder for a not-yet-fetched disc inside the
// (manifest-owned) per-game folder, ADDITIVELY: anything already at the path — even a
// stale partial — is left alone. The stub keeps the on-card disc set aligned with the
// full .m3u so the hooks' incomplete-scan and --check-rom see one honest shape.
// Best-effort: a failed create just means the disc reads "absent" instead of "stub" to
// the same detectors. (Pre-lodor#7 a failed multi-disc download RemoveAll'd the whole
// disc folder; under disc-1-first a partial set is a VALID state, so nothing is ever
// bulk-deleted on failure anymore — per-disc resume keeps verified discs.)
func ensureDiscStub(dest string) {
	if _, err := os.Stat(dest); err == nil {
		return
	}
	if f, err := os.Create(dest); err == nil {
		_ = f.Close()
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
		noteAuthErr(err)
		fmt.Fprintf(os.Stderr, "BIOSFAIL platforms: %s\n", safeErr(err))
		fmt.Println("RESULT bios=0")
		exitMode(0)
	}

	count := 0
	cancelledRun := false
	for _, p := range platforms {
		if cancelledRun {
			break
		}
		fw, ferr := client.GetFirmware(p.ID)
		if ferr != nil || len(fw) == 0 {
			continue // no BIOS on the server for this platform
		}
		for _, f := range fw {
			// REAL CANCEL (lodor#42, B-press sentinel via --cancellable), checked
			// BETWEEN files: BIOS already written is verified and kept; the rest
			// re-fetches next run (skip-if-present makes the retry cheap).
			if client.CancelCheck != nil && client.CancelCheck() {
				cancelledRun = true
				fmt.Fprintln(os.Stderr, "CANCEL download-bios between files")
				break
			}
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
				noteAuthErr(derr)
				continue
			}
			wrote := false
			for _, dest := range dests {
				if fi, sErr := os.Stat(dest); sErr == nil && fi.Size() > 0 {
					continue // keep the copy already there
				}
				// FAT32-atomic firmware write (temp + fsync + rename + dir fsync); a
				// torn BIOS file would fail the emulator until re-fetched. fsutil also
				// MkdirAll's the parent.
				if wErr := fsutil.WriteFileAtomic(dest, data, 0o644); wErr == nil {
					wrote = true
				}
			}
			if wrote {
				count++
			}
		}
	}
	// cancelled=1 is ADDITIVE (lodor#42) — existing parsers key on the bios= token.
	if cancelledRun {
		fmt.Printf("RESULT bios=%d cancelled=1\n", count)
	} else {
		fmt.Printf("RESULT bios=%d\n", count)
	}
	exitMode(0)
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
	var remaining []string   // entries to keep in the queue (not landed)
	var stuckLines []string  // human "STUCK\t<game>\t<why>" lines for the launcher (stdout)

	cancelledRun := false
	for i, line := range pending {
		// REAL CANCEL (lodor#42, B-press sentinel via --cancellable), checked BETWEEN
		// entries before committing to the next upload: everything already landed is
		// verified and dropped from the queue, everything not yet attempted stays
		// queued verbatim for the next run. Never interrupts an in-flight upload —
		// a half-sent save is worse than a short wait.
		if client.CancelCheck != nil && client.CancelCheck() {
			cancelledRun = true
			fmt.Fprintf(os.Stderr, "CANCEL push-pending before item %d/%d\n", i+1, total)
			remaining = append(remaining, pending[i:]...)
			break
		}
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

	notePushResults(allResults)
	// #43: a drain that verifiably reached the server stamps the last-sync record —
	// real uploads landed (pushed>0), or a non-empty queue fully landed (stuck==0
	// means every save file verified on the server this run, including
	// already-on-server dedups). An empty queue, an all-stuck (offline) run, or a
	// CANCELLED run (saves deliberately left behind) proves nothing and never
	// stamps — "Last synced: just now" must not paper over an unfinished drain.
	if !cancelledRun && (pushed > 0 || (len(allResults) > 0 && stuck == 0)) {
		stampSync(pushed, 0)
	}
	// cancelled=1 is ADDITIVE (lodor#42) — existing parsers key on pushed=/stuck=.
	cancelSuffix := ""
	if cancelledRun {
		cancelSuffix = " cancelled=1"
	}
	fmt.Printf("RESULT pushed=%d total=%d stuck=%d%s\n", pushed, len(allResults), stuck, cancelSuffix)
	for _, s := range stuckLines {
		fmt.Println(s)
	}
	exitMode(0)
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
	case sync.OutcomeHashMismatch:
		return "server copy couldn't be verified — will retry"
	case sync.OutcomeEmptyLocalSave:
		return "save file on the card is empty — not uploaded"
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

// runSyncSave pulls per the content-hash lineage decision then pushes the local
// save for ONE ROM (the per-game two-way sync). Contract:
//
//	RESULT pulled=<0|1> pushed=<0|1> ghosts=<N> reason=<token>
//
// ghosts counts the server-side save records for this ROM whose bytes are
// missing/zero (#63). reason (APPENDED field, A2 — earlier fields unchanged for
// existing parsers) is the machine token for WHY the pull leg decided what it did:
//
//	in-sync         local content == newest server revision (nothing moved)
//	older-lineage   local matched an OLDER revision → newest pulled (.bak kept)
//	no-local        no local save existed → newest pulled
//	unpushed-local  local matched NO revision → never overwritten; push leg uploads it
//	tombstone       no local save because it was DELETED here after a sync and the
//	                server has nothing newer than the deleted revision — skipped, the
//	                deletion sticks (deleted-save tombstone; explicit restore resurrects)
//	no-server-save  server has no (non-ghost) save for this ROM
//	offline         the server couldn't be reached — NOT "in sync" (the pre-A2
//	                0/0 line let the pak claim "already in sync" while offline)
//	resolve         the ROM didn't match a RomM game / has no save directory
//	snapshot        flashback lose-proof abort
//
// pushed=1 now means a REAL verified upload traveled (sync.Uploaded) — a
// content-identical save already on the server reports pushed=0 + reason=in-sync
// instead of stacking a duplicate revision (pre-upload dedup, A2).
func runSyncSave(client *romm.Client, cfg *config.Config, romPath string) {
	writeProgress(0)
	writePhase("Checking your save…")
	pull := sync.PullSaveDirect(client, cfg, romPath)
	notePullResult(pull)

	writeProgress(50)
	writePhase("Backing up your save…")
	pushResults := sync.PushSaveDirect(client, cfg, romPath)
	notePushResults(pushResults)
	uploaded := sync.Uploaded(pushResults)

	writeProgress(100)
	// #43: stamp when the server verifiably answered — a real upload landed, or the
	// pull leg's outcome required a server listing (pullSawServer). An offline /
	// resolve-failed run never stamps.
	if uploaded >= 1 || pullSawServer(pull) {
		stampSync(b2i(pull.Pulled())+uploaded, 0)
	}
	fmt.Printf("RESULT pulled=%d pushed=%d ghosts=%d reason=%s\n",
		b2i(pull.Pulled()), b2i(uploaded >= 1), pull.Ghosts, pullReasonToken(pull))
	exitMode(0)
}

// pullReasonToken maps a PullResult to the stable machine token the RESULT
// reason= field carries (see runSyncSave). Host-free, no spaces.
func pullReasonToken(r sync.PullResult) string {
	switch r.Outcome {
	case sync.PullWritten:
		if r.Reason != "" {
			return r.Reason // "no-local" | "older-lineage"
		}
		return "pulled"
	case sync.PullInSync:
		return "in-sync"
	case sync.PullLocalUnpushed:
		return "unpushed-local"
	case sync.PullTombstoned:
		return "tombstone"
	case sync.PullNoServerSave:
		return "no-server-save"
	case sync.PullError:
		return "offline"
	case sync.PullResolveFail:
		return "resolve"
	case sync.PullSnapshotFail:
		return "snapshot"
	default:
		return "unknown"
	}
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
	notePushResults(results)
	if entryLanded(results) {
		pushed, _, _ := sync.Counts(results)
		// Landed (server-VERIFIED — entryLanded only accepts Pushed/AlreadyOnServer,
		// both now hash-confirmed) → write the launcher's synced-✓ signal.
		// Best-effort: a failed signal write must not turn a real, verified upload
		// into a reported failure.
		if err := writeLastSynced(romPath, pushed); err != nil {
			fmt.Fprintf(os.Stderr, "push-save: WARN couldn't write synced signal: %s\n", safeErr(err))
		}
		// #43: the landed, hash-verified push is exactly the "last synced" moment —
		// stamp the generalized record beside the launcher's one-shot ✓ signal.
		stampSync(pushed, 0)
		// Cross-device sidecars (#146/#149): with the save landed (radio warm,
		// server reachable), push this ROM's compact .lodortime record and its
		// newest preview alongside it. BEST-EFFORT: never changes this mode's
		// outcome or stdout contract.
		pushSessionMetas(client, cfg, romPath)
		fmt.Printf("RESULT pushed=1 staged=0\n")
		exitMode(0)
	}

	// Not landed → stage the current save bytes and queue them for a later upload
	// (offline backlog). Mirrors runRestoreSave's stage+enqueue path. When the ROM
	// couldn't be resolved (e.g. the server was unreachable so GetRom failed), there is
	// no per-file list to stage — fall back to a bare-path queue entry so a changed save
	// is NEVER silently dropped and the pending badge still counts it (--push-pending
	// resolves+uploads the live save when WiFi returns).
	files := sync.LocalSaveFilesForRom(client, cfg, romPath)
	if len(files) == 0 {
		// No save exists on the card at all: nothing to stage OR queue. A bare
		// queue entry here is a phantom (drains forever, surfaces as a false
		// "save queued" in every lane). reason=no-save lets frontends say the
		// honest thing (bughunt 2026-07-10 M2).
		fmt.Printf("RESULT pushed=0 staged=0 reason=no-save\n")
		exitMode(0)
	}
	staged := 0
	for _, f := range files {
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
	exitMode(0)
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
	line := fmt.Sprintf("%d %d %s\n", time.Now().Unix(), count, filepath.Base(romPath))
	// FAT32-atomic: temp + fsync + rename + dir fsync (fsutil).
	return fsutil.WriteFileAtomicString(p, line, 0o644)
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
	// Gamelist refresh (#186, knulli builds only — a compiled no-op elsewhere): a
	// ✘→✓ flip renamed the on-disk file, so the owned gamelist <path> in THIS rom's
	// directory must follow it. Scoped to the one directory; offline; best-effort.
	if flipped {
		maybeWriteGamelists(cfg, filepath.Dir(romPath))
	}
	fmt.Printf("RESULT reconciled=%d\n", b2i(flipped))
	os.Exit(0)
}

// runEvict is --reconcile's mirror image (task #125 — the Game Manager's "Delete
// from card"): remove ONE downloaded ROM's bytes and leave its 0-byte cloud stub
// (✓→✘), the save + cover riding the rename so nothing is deleted or orphaned. A
// multi-disc .m3u's referenced disc files are deleted too. Contract:
// RESULT evicted=<0|1> [reason=missing|stub|resolve|truncate]. Offline; exit 0
// either way — the reason token is the honest failure signal.
func runEvict(cfg *config.Config, romPath string) {
	evicted, reason := catalog.EvictToStub(cfg, romPath)
	// Gamelist refresh (#186, knulli builds only — a compiled no-op elsewhere): the
	// ✓→✘ flip renamed the on-disk file back to its cloud-stub name, so the owned
	// gamelist <path> must follow. Scoped to the one directory; offline; best-effort.
	if evicted {
		maybeWriteGamelists(cfg, filepath.Dir(romPath))
		fmt.Println("RESULT evicted=1")
	} else {
		fmt.Printf("RESULT evicted=0 reason=%s\n", reason)
	}
	os.Exit(0)
}

// runListSaves prints every server save for one ROM, NEWEST FIRST, one tab-separated
// line per save:
//
//	<save_id>\t<YYYY-MM-DD HH:MM>\t<device-or-emulator>\t<size_kb>KB[\tCURRENT]
//
// plus, AFTER the rows, one single-field trailer line (A3 — no tab, so every
// existing `awk -F'\t' NF>=2` row filter drops it):
//
//	LOCAL=<none|current|older|unpushed|deleted>
//
// summarizing how the save THIS LAUNCH WILL LOAD relates to the listed set. STRICT
// semantics (task #135): the trailer is judged against the launched basename's OWN
// save file (sync.PrimaryLocalSaveFilesForRomPath), never a coexist twin's, and
// "current" requires a match with the NEWEST non-ghost revision (none = the launched
// name has no local save; current = it matches the newest revision; older = it
// matches an older revision only; unpushed = it matches no revision — unpushed local
// progress). The pre-launch hook keys its prompt/pull/silent decision off this
// trailer.
//
// deleted (deleted-save tombstone) REFINES none: no local save exists because it was
// deliberately DELETED on this device after a sync, and the server's newest revision
// is the one this device last synced (or older) — auto-pull paths must honor the
// deletion instead of silently restoring the newest revision. The listed rows are
// still printed in full: an EXPLICIT user restore from this list always resurrects.
// Hooks written before this value existed fall through their none/current checks to
// the prompt path — the user decides, which is an acceptable (explicit) degrade. The row-level CURRENT tag keeps the AGGREGATE any-local-file semantics —
// "these bytes are on this device" is the Game Manager's display truth, possibly via
// the twin's save file. Different jobs; do not conflate (Smart Pro 2026-07-03: the
// twin's save matched the newest revision, the aggregate signal read "current", and
// the clean twin silently launched into its OLDER save).
//
// "who" is the first device sync's DeviceName when present, else the emulator. Zero
// saves (or an unmanaged ROM) → print nothing, exit 0. GHOST records (#63 — bytes
// missing/zero on the server) are SKIPPED entirely: an unrestorable revision must
// never be offered in the rollback menu. Pairing-expired → exit 6, stdout stays a
// pure list (see authgate.go). A GetSaves failure now exits 3 (reachability, like
// every other net mode) — pre-A3 it exited 0 with empty output, making "your server
// is unreachable" indistinguishable from "no saves exist" (the Smart Pro
// 2026-07-02 "No server saves for ✓ Pokemon Emerald" field lie).
func runListSaves(client *romm.Client, cfg *config.Config, romPath string) {
	id, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || id == 0 {
		os.Exit(0) // unmanaged ROM — nothing to list
	}
	allSaves, err := sync.GetSavesAttributed(client, cfg, romm.SaveQuery{RomID: id})
	if err != nil {
		noteAuthErr(err)
		fmt.Fprintf(os.Stderr, "FATAL list-saves: %s\n", safeErr(err))
		exitModeQuiet(3)
	}
	saves, _ := sync.SplitGhosts(allSaves)
	if len(saves) == 0 {
		os.Exit(0)
	}
	sort.Slice(saves, func(i, j int) bool { return saves[i].UpdatedAt.After(saves[j].UpdatedAt) })

	// ROW-LEVEL CURRENT tag — the Game Manager's "(on this device)" display: which server
	// revision's bytes exist in ANY of this ROM's local save files (both coexist twins).
	// The signal is RomM's per-save content_hash (the MD5 of the save bytes) compared
	// against the MD5 of the on-device save file(s) — the exact same signal AlreadyOnServer
	// trusts for dedup. Mark only the NEWEST matching revision (saves are sorted
	// newest-first), and only when the server actually exposes a content_hash AND a local
	// save exists; otherwise emit NO marker rather than guess (honest by omission).
	// NOTE (task #135): this aggregate mark is display truth ONLY — the launch decision
	// lives in the LOCAL= trailer below, which is per-launched-file strict.
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

	markedCurrent := false // the NEWEST matching row got the CURRENT tag
	for _, s := range saves {
		who := s.Emulator
		if dn := sync.AttributedDeviceName(s, sync.SelfDeviceID(cfg)); dn != "" {
			who = dn
		}
		// Field 4 (optional — EXTENDS, never reorders fields 0-3): "CURRENT" on the single
		// newest revision whose content matches the on-device save; absent on every other row.
		mark := ""
		if !markedCurrent && matchesDevice(s) {
			mark = "\tCURRENT"
			markedCurrent = true
		}
		fmt.Printf("%d\t%s\t%s\t%dKB%s\n",
			s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), who, s.FileSizeBytes/1024, mark)
	}
	// LOCAL= trailer (single field — row parsers drop it; see contract above). STRICT
	// (task #135): judged against the save file the LAUNCH will load, not any twin's,
	// and "current" only against the NEWEST revision — see ListSavesLocalState.
	state := sync.ListSavesLocalState(saves, sync.PrimaryLocalSaveFilesForRomPath(client, cfg, romPath))
	// TOMBSTONE REFINEMENT (see contract above): "none" that the save ledger can
	// prove is a post-sync local DELETE — with nothing newer on the server — reads
	// "deleted", so auto-pull hooks stop resurrecting it (saves[0] is the newest
	// non-ghost revision; the sort above guarantees it).
	if state == "none" && sync.SaveTombstoned(cfg, id, saves[0]) {
		state = "deleted"
	}
	fmt.Printf("LOCAL=%s\n", state)
	exitModeQuiet(0)
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
		noteAuthErr(err)
		fmt.Println("RESULT restored=0 reason=download")
		exitMode(0)
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
	// META GUARD (#146): a .lodortime/.lodorshot.png sidecar record is not a
	// save — restoring one over a real local save would destroy it.
	if sync.IsMetaSave(chosen) {
		fmt.Println("RESULT restored=0 reason=meta")
		os.Exit(0)
	}
	// GHOST GUARD (#63): the chosen record has no stored bytes — it cannot be
	// restored (the listing already hides ghosts; this covers a direct call).
	if sync.IsGhostSave(chosen) {
		fmt.Println("RESULT restored=0 reason=ghost")
		os.Exit(0)
	}

	// Preserve the current save before the overwrite. Push it to the timeline now; if
	// that doesn't land (offline), stage each current save file and queue it for later.
	staged := 0
	if current := sync.LocalSaveFilesForRom(client, cfg, romPath); len(current) > 0 {
		preserve := sync.PushSaveDirect(client, cfg, romPath)
		notePushResults(preserve)
		if !entryLanded(preserve) {
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
	notePullResult(res)
	if res.Pulled() {
		// #28: retire the stale auto-load-state so the frontend can't silently
		// auto-load it over the freshly-restored save. Best-effort, fail-safe
		// (only retires after a clean state push); never blocks the restore.
		retired, _ := sync.RetireAutoStateAfterRestore(client, cfg, romPath)
		// #43: a landed flashback restore is a verified server transfer — stamp it.
		stampSync(1, 0)
		fmt.Printf("RESULT restored=1 staged=%d retiredauto=%d\n", staged, b2i(retired))
	} else {
		reason := "download"
		if res.Outcome == sync.PullResolveFail {
			reason = "resolve"
		}
		fmt.Printf("RESULT restored=0 reason=%s\n", reason)
	}
	exitMode(0)
}

// runSyncFeed lists recent server saves across the MAPPED platforms, deduped by save
// ID, newest-first, capped at 20. One tab-separated line per save:
//
//	<game>\t<YYYY-MM-DD HH:MM>\t<device>
//
// game = FileNameNoExt with any trailing 2-5 char rom-ext trimmed; device = the first
// device sync's DeviceName when present, else empty. Zero saves → print nothing, exit 0.
// A platform-fetch failure exits 3 (reachability, the --list-saves convention) — before
// lodor#44 it exited 0 with an empty feed, so "server unreachable" rendered as the
// launcher's empty state (an honest-UI violation).
// syncFeedRC maps the feed's platform-fetch outcome to an exit code (lodor#44): a fetch
// failure is reachability (3, matching --list-saves), never a fake empty feed.
func syncFeedRC(err error) int {
	if err != nil {
		return 3
	}
	return 0
}

func runSyncFeed(client *romm.Client, cfg *config.Config) {
	platforms, err := mappedPlatforms(client, cfg)
	if err != nil {
		noteAuthErr(err)
		fmt.Fprintf(os.Stderr, "FATAL sync-feed: %s\n", safeErr(err))
		exitModeQuiet(syncFeedRC(err))
	}

	var saves []romm.Save
	seen := map[int]bool{}
	for _, p := range platforms {
		ps, gerr := sync.GetSavesAttributed(client, cfg, romm.SaveQuery{PlatformID: p.ID})
		if gerr != nil {
			noteAuthErr(gerr)
			continue
		}
		for _, s := range ps {
			if sync.IsGhostSave(s) || sync.IsMetaSave(s) {
				continue // a byte-less record (#63) or meta sidecar (#146) isn't a playable session — keep it out of the feed
			}
			if !seen[s.ID] {
				seen[s.ID] = true
				saves = append(saves, s)
			}
		}
	}
	if len(saves) == 0 {
		exitModeQuiet(0)
	}
	sort.Slice(saves, func(i, j int) bool { return saves[i].UpdatedAt.After(saves[j].UpdatedAt) })
	if len(saves) > 20 {
		saves = saves[:20]
	}
	for _, s := range saves {
		// Historical marker-named records (pre-#126 uploads) still display clean:
		// the leading ✘/✓ is a device-local artifact, never part of the game name.
		game := platform.StripLeadingMarker(s.FileNameNoExt)
		if e := filepath.Ext(game); len(e) >= 2 && len(e) <= 5 {
			game = strings.TrimSuffix(game, e)
		}
		device := sync.AttributedDeviceName(s, sync.SelfDeviceID(cfg))
		fmt.Printf("%s\t%s\t%s\n", game, s.UpdatedAt.Format("2006-01-02 15:04"), device)
	}
	exitModeQuiet(0)
}

// runRecent prints the single most-recently-played game across mapped platforms — the
// data source for the launcher's "Continue" tile. Output is ONE TAB-separated line:
//   <localRomPath>\t<game>\t<when>\t<device>
// localRomPath is the on-card stub/file path (the .m3u for multi-disc). Unreachable, no
// platforms, or no saves => prints nothing and exits 0 (the launcher falls back to its own
// local recents). One extra GetRom resolves the newest save's rom to a launchable path.
func runRecent(client *romm.Client, cfg *config.Config) {
	platforms, err := mappedPlatforms(client, cfg)
	if err != nil {
		noteAuthErr(err)
		exitModeQuiet(0)
	}
	var newest romm.Save
	found := false
	for _, p := range platforms {
		ps, gerr := sync.GetSavesAttributed(client, cfg, romm.SaveQuery{PlatformID: p.ID})
		if gerr != nil {
			noteAuthErr(gerr)
			continue
		}
		for _, s := range ps {
			if sync.IsGhostSave(s) || sync.IsMetaSave(s) {
				continue // a ghost (#63) or meta sidecar (#146) must never drive the Continue tile
			}
			if !found || s.UpdatedAt.After(newest.UpdatedAt) {
				newest = s
				found = true
			}
		}
	}
	if !found {
		exitModeQuiet(0)
	}
	rom, rerr := client.GetRom(newest.RomID)
	if rerr != nil || rom.ID == 0 {
		noteAuthErr(rerr)
		exitModeQuiet(0)
	}
	path := platform.LocalRomPath(cfg, rom)
	if path == "" {
		os.Exit(0)
	}
	game := rom.Name
	if game == "" {
		game = rom.FsNameNoExt
	}
	device := sync.AttributedDeviceName(newest, sync.SelfDeviceID(cfg))
	fmt.Printf("%s\t%s\t%s\t%s\n", path, game, newest.UpdatedAt.Format("2006-01-02 15:04"), device)
	exitModeQuiet(0)
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

// sdRoot mirrors catalog's card-root resolution for the modes that turn the index's
// SDCARD-relative paths back into absolute on-card paths.
func sdRoot() string {
	sd := os.Getenv("SDCARD_PATH")
	if sd == "" {
		sd = "/mnt/SDCARD"
	}
	return sd
}

// runPullSaves is the TARGETED bulk save pull (task #133, the fast half of "Sync now"):
// one GetSaves per mapped platform finds every game with a real (non-ghost) server
// save; each of those that is mirrored on THIS card gets the same per-ROM
// PullSaveDirect the pre-launch hook uses (newest-wins, .bak before overwrite, ghosts
// filtered) — so after it runs, every on-card game carries its latest cross-device
// save. Games with no server save are never touched; no catalog mirror is involved.
// Contract:
//
//	RESULT pulled=<N> checked=<M> ghosts=<G> pushed=<K> tombstones=<T>
//
// checked = candidates on this card that had a server save; pulled = local files
// actually written (LocalNewer/NoServerSave count as checked, not pulled). No local
// index yet (fresh device before any Refresh) => pulled=0 checked=0, exit 0 — honest
// no-op, the seed/Refresh path owns first population. Total platform-list failure is
// a reachability error: exit 3 like the other net modes.
//
// tombstones= (APPENDED field — earlier fields unchanged for existing parsers)
// counts games SKIPPED because their save was deliberately deleted on this device
// after a sync and the server holds nothing newer (reason=tombstone; one host-free
// stderr line per skip names the game). includeDeleted (--include-deleted) bypasses
// the tombstones — the explicit "yes, resurrect my deleted saves" escape hatch.
func runPullSaves(client *romm.Client, cfg *config.Config, includeDeleted bool) {
	writeProgress(0)
	writePhase("Checking saves…")

	idPath := catalog.LoadIndexIDPath(cfg)
	if len(idPath) == 0 {
		writeProgress(100)
		fmt.Println("RESULT pulled=0 checked=0 ghosts=0")
		exitMode(0)
	}

	platforms, err := mappedPlatforms(client, cfg)
	if err != nil {
		writeProgress(0)
		noteAuthErr(err)
		if pairingExpired {
			writePhase("Pairing expired — re-pair this device")
		} else {
			writePhase("Couldn't reach RomM")
		}
		fmt.Fprintf(os.Stderr, "FATAL pull-saves: %s\n", safeErr(err))
		exitMode(3)
	}

	// Newest real save per ROM across mapped platforms (same walk the Continue list
	// does — a handful of list calls, not one per game).
	type cand struct {
		romID int
		t     time.Time
	}
	newest := map[int]time.Time{}
	ghosts := 0
	listedOK := false // #43: at least one saves listing answered — server contact proven
	for _, p := range platforms {
		saves, gerr := client.GetSaves(romm.SaveQuery{PlatformID: p.ID})
		if gerr != nil {
			noteAuthErr(gerr)
			continue
		}
		listedOK = true
		for _, s := range saves {
			if sync.IsMetaSave(s) {
				continue // meta sidecar (#146): not a save — never counted, never pulled here
			}
			if sync.IsGhostSave(s) {
				ghosts++
				continue
			}
			if s.RomID == 0 {
				continue
			}
			if t, seen := newest[s.RomID]; !seen || s.UpdatedAt.After(t) {
				newest[s.RomID] = s.UpdatedAt
			}
		}
	}

	// Only games mirrored on THIS card are targets; newest-first so the saves the
	// user most likely wants next land first.
	sd := sdRoot()
	var cands []cand
	for id, t := range newest {
		rel, found := idPath[id]
		if !found || rel == "" {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(sd, rel)); statErr != nil {
			continue
		}
		cands = append(cands, cand{id, t})
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].t.Equal(cands[j].t) {
			return cands[i].t.After(cands[j].t)
		}
		return cands[i].romID < cands[j].romID
	})

	pulled, checked, pushed, tombstones := 0, 0, 0, 0
	perItemFail := false
	cancelledRun := false
	for i, c := range cands {
		// REAL CANCEL (lodor#42, B-press sentinel via --cancellable), checked BETWEEN
		// candidates: every save already pulled/pushed is verified and stays; the
		// rest simply wait for the next sync. Never interrupts an in-flight item.
		if client.CancelCheck != nil && client.CancelCheck() {
			cancelledRun = true
			fmt.Fprintf(os.Stderr, "CANCEL pull-saves before item %d/%d\n", i+1, len(cands))
			break
		}
		rel := idPath[c.romID]
		writePhase(fmt.Sprintf("Pulling saves (%d/%d)…", i+1, len(cands)))
		if len(cands) > 0 {
			writeProgress((i + 1) * 100 / len(cands))
		}
		p := filepath.Join(sd, rel)
		res := sync.PullSaveDirectOpts(client, cfg, p, sync.PullOptions{IncludeDeleted: includeDeleted})
		notePullResult(res)
		if !pullSawServer(res) {
			perItemFail = true // #43: a transport/resolve failure taints the run's stamp
		}
		ghosts += res.Ghosts
		checked++
		if res.Pulled() {
			pulled++
		}
		// DELETED-SAVE TOMBSTONE: the save was deliberately deleted on this device
		// after a sync and the server has nothing newer — skipped, named honestly on
		// stderr (stdout stays the parsed RESULT contract). --include-deleted pulls it.
		if res.Outcome == sync.PullTombstoned {
			tombstones++
			fmt.Fprintf(os.Stderr, "PULL %s: skipped reason=tombstone (save deleted on this device; server has nothing newer)\n", filepath.Base(p))
		}
		// UNPUSHED LOCAL PROGRESS (A2): the local save matches no server revision —
		// never overwritten. Push it through the verified-upload funnel NOW so the
		// bulk sync leaves it the newest revision instead of a silent skip.
		if res.Outcome == sync.PullLocalUnpushed {
			pres := sync.PushSaveDirect(client, cfg, p)
			notePushResults(pres)
			pushed += sync.Uploaded(pres)
		}
	}

	writeProgress(100)
	// #43: stamp when the bulk pull VERIFIABLY synced against the server — a saves
	// listing answered (server contact) and no candidate died on transport/resolve.
	// checked==0 with a good listing is a verified "nothing to pull" and stamps too;
	// moved counts are honest (may be 0). A partial-failure or CANCELLED run never
	// stamps: the user must not read "Last synced: just now" over saves that
	// didn't travel.
	if listedOK && !perItemFail && !cancelledRun {
		stampSync(pulled+pushed, 0)
	}
	// pushed= is APPENDED (A2), tombstones= after it, cancelled=1 after that
	// (lodor#42) — all ADDITIVE; existing parsers key on pulled=/checked=/ghosts=.
	cancelSuffix := ""
	if cancelledRun {
		cancelSuffix = " cancelled=1"
	}
	fmt.Printf("RESULT pulled=%d checked=%d ghosts=%d pushed=%d tombstones=%d%s\n", pulled, checked, ghosts, pushed, tombstones, cancelSuffix)
	exitMode(0)
}

// runSyncContinue is the LIGHT Continue/recents refresh (task #133): rebuild the
// cross-device "0) Continue" collection and merge its entries into the host's native
// Recently Played (task #132) from the LOCAL index + per-platform save listings —
// no catalog mirror, no collections listing, so it belongs in the fast "Sync now"
// (the full derivation still rides --mirror-collections unchanged). Contract:
//
//	CONTINUE entries=<N>
//	RECENTS merged=<M> total=<T>
//
// entries=0 = empty/unreachable feed (the existing Continue file is left alone —
// only the full mirror's prune retires it); merged=0 = nothing new for Recently
// Played (or no MinUI-family recents on this card). Always exit 0: this is a
// convenience layer — the other Sync-now legs own the honest failure codes.
func runSyncContinue(client *romm.Client, cfg *config.Config) {
	writeProgress(0)
	writePhase("Updating Continue…")
	entries, merged, total := catalog.SyncContinue(client, cfg)
	writeProgress(100)
	fmt.Printf("CONTINUE entries=%d\n", entries)
	fmt.Printf("RECENTS merged=%d total=%d\n", merged, total)
	exitMode(0)
}

// runRACmd sends one RetroArch Network Control Interface command over loopback
// UDP (task #145 — heavy-pak session bracketing; see engine/ranet). Contract:
//
//	fire-and-forget (default): exit 0 once the datagram left, 4 on a local
//	  send failure. No stdout.
//	--recv: send, wait 250ms, retry x3; print the reply line to stdout and
//	  exit 0. A silent peer exits 3 — the wrapper reads that as "this vendor
//	  RetroArch has no network interface" (ra-net UNSUPPORTED) and degrades.
//
// Purely local: no config, no host, no device — callable in any pairing state.
func runRACmd(cmd string, recv bool, port int) {
	addr := ranet.Addr(port)
	if recv {
		reply, err := ranet.SendRecv(addr, cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ra-cmd: %v\n", err)
			os.Exit(3)
		}
		fmt.Println(reply)
		os.Exit(0)
	}
	if err := ranet.Send(addr, cmd); err != nil {
		fmt.Fprintf(os.Stderr, "ra-cmd: %v\n", err)
		os.Exit(4)
	}
	os.Exit(0)
}

// runReportSession reports ONE finished play session to RomM (POST /api/play-sessions)
// so cross-device Continue / recently-played reflects time played on THIS device. It is
// pure telemetry and best-effort: it ALWAYS exits 0 (a plain os.Exit, deliberately NOT
// exitMode — even a noted pairing expiry must not turn a quit-path telemetry call into
// exit 6 / a persisted fail reason) and never blocks a game quit. The minarch shim
// brackets each launch and passes the rom path plus the start/end unix epochs; this
// resolves the rom_id locally, then posts a single-entry batch. Contract:
// RESULT reported=<0|1>. An older RomM without the endpoint (404) reports 0 silently.
func runReportSession(client *romm.Client, host config.Host, cfg *config.Config, romPath string, started, ended int64) {
	if reportSessionCore(client, host, cfg, romPath, started, ended) {
		fmt.Println("RESULT reported=1")
	} else {
		fmt.Println("RESULT reported=0")
	}
	os.Exit(0)
}

// reportSessionCore does the play-session report and returns true iff the server
// created the session. It prints NO RESULT line and never exits. Every diagnostic is a
// host-free PSFAIL/PSSKIP token on stderr (no URL, token, or device_id ever echoed).
// host is the RESOLVED host main dispatched on (multi-user: the ACTIVE profile), so the
// session is attributed to the active profile's device_id — never a blind Hosts[0].
func reportSessionCore(client *romm.Client, host config.Host, cfg *config.Config, romPath string, started, ended int64) bool {
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
	entry := romm.PlaySessionEntry{
		RomID:      id,
		StartTime:  time.Unix(started, 0).UTC().Format(time.RFC3339),
		EndTime:    time.Unix(ended, 0).UTC().Format(time.RFC3339),
		DurationMs: (ended - started) * 1000,
	}
	resp, err := client.ReportPlaySessions(host.DeviceID, []romm.PlaySessionEntry{entry})
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
