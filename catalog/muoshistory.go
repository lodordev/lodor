// muOS native-History injection (task #181 — cross-device "Continue" on Lodor-muOS).
//
// muOS has no MinUI-style recent.txt. Its History menu (muxhistory) renders one
// content-pointer file per game under /run/muos/storage/info/history:
//
//	<content_name>-<%08X FNV-1a-32(file_path)>.cfg
//
// three lines, NO trailing newline (frontend common/content.c load_content:
// `snprintf(pointer, …, "%s\n%s\n%s", file_path, system_sub, content_name)`):
//
//	<file_path>     absolute REAL rom path — muxhistory launches exactly this
//	<system_sub>    the rom's folder relative to ROMS/ ("Sega Game Gear")
//	<content_name>  the rom basename without extension (on-disk marker kept)
//
// and orders the menu by file MTIME, newest first (collection/common.c
// time_compare_for_history; muxhistory.c gen_item → sort_items_time). Format, hash
// and dir verified against the muOS source (MustardOS/frontend @ 59029cc) AND the
// shipped RG34XX 2601.1 image's own binaries (libmuxcom.so carries the literal
// "/run/muos/storage/info/history/%s-%08X.cfg" format string). file_path must be the
// REAL mount path, not /mnt/union/…: gen_item_from_files runs
// union_rewrite_file_paths on every listing, which REWRITES (and re-mtimes) any cfg
// carrying a union path — a union-path injection would destroy its own ordering.
//
// So Lodor feeds cross-device recents into muOS's OWN History: one .cfg per Continue
// entry, mtime stamped to the RomM save's UpdatedAt (SERVER clock — the #147 rule:
// an RTC-less device's now() is garbage; the feed itself carries the truth), and
// muxhistory does the rest. Launching an injected entry goes muxhistory →
// load_content → the standard assign/launch pipeline — the SAME path the seeded
// info/override launch bracket wraps, so download-on-launch (0-byte stubs) and the
// save-sync bracket hold without any muOS change.
//
// THE USER'S REAL HISTORY IS SACRED (gamelist.go's merge rule, applied here):
//   - only .cfg files the mirror-manifest owns (kind "history") are created or
//     rewritten;
//   - a FOREIGN .cfg for the same game (same filename, or a pointer that resolves to
//     our target path) means the device has its own recency for it — skip, never
//     touching its bytes or mtime;
//   - the ONE exception mirrors gamelist's marker rule: a pointer whose rom name
//     carries a Lodor state marker (✘/✓/legacy) is never the user's own artifact —
//     muOS wrote it by launching a Lodor-mirrored file. When the marker flips
//     (✘→✓ after download-on-launch) that pointer is DEAD (muxhistory toasts
//     "Could not launch"), so it is re-keyed to the live on-card name instead of
//     stranding it;
//   - nothing is ever pruned: history accumulates by design on muOS (users remove
//     entries via muxhistory's own remove UI), and an injected entry the user later
//     launches natively is rewritten in place by muOS itself. Uninstall deliberately
//     leaves kind-"history" entries too — by then they may BE the user's history.
//
// Writes are FAT-atomic via fsutil (temp+fsync+rename+dir-fsync); an unchanged entry
// is not rewritten and not re-stamped (no mtime churn → no false recency). This file
// is COMPILED ON EVERY TAG (the core is dir-parameterized and tested tag-free); call
// sites gate on muosHistoryEnabled, a build-tag constant true only under -tags muos —
// every other host's behavior stays byte-identical (dead code). CGO-free, stdlib only.
package catalog

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lodor/fsutil"
	"lodor/platform"
)

// muosHistoryDirDefault is the runtime history dir muOS binds from the card
// (MUOS/info/history ↔ /run/muos/storage/info/history; both literals live in the
// 2601.1 image's libmuxmod.so).
const muosHistoryDirDefault = "/run/muos/storage/info/history"

// muosHistoryMtimeSlack tolerates filesystem timestamp granularity (FAT stores
// 2-second mtimes) when deciding whether an owned entry's stamp already matches
// the feed — otherwise every sync would rewrite every entry.
const muosHistoryMtimeSlack = 2 * time.Second

// muosHistoryDir resolves the history dir: MUOS_HISTORY_DIR (sandbox/tests, the
// same env-override pattern as the other muOS card roots) else the muOS default.
func muosHistoryDir() string {
	if v := os.Getenv("MUOS_HISTORY_DIR"); v != "" {
		return v
	}
	return muosHistoryDirDefault
}

// muosFNV32 is FNV-1a 32-bit — hash/fnv's New32a is the same offset-basis
// (2166136261) and prime (16777619) as frontend content.c fnv_hash_str, so an
// injected filename is byte-identical to the one muOS itself would write for the
// same file_path (native launches converge on OUR file instead of duplicating).
func muosFNV32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// muosHistTarget is one injectable history pointer.
type muosHistTarget struct {
	fname   string // "<content_name>-<%08X>.cfg"
	content string // "<file_path>\n<system_sub>\n<content_name>" (no trailing \n)
	path    string // file_path (line 1)
	key     string // identity: lower(system_sub) + NUL + lower(marker-stripped name)
	t       time.Time
}

// muosHistIdentity builds the game-identity key used to recognize the same game
// across marker flips: the ROMS-relative folder plus the marker-stripped,
// extension-less name, both case-folded (exFAT is case-insensitive).
func muosHistIdentity(sub, contentName string) string {
	return strings.ToLower(sub) + "\x00" + strings.ToLower(platform.StripLeadingMarker(contentName))
}

// buildMuosHistTarget derives the injectable pointer for one Continue entry.
// rel is the engine's SDCARD-relative on-card path ("/Roms/<system…>/<file>");
// romsRoot is the REAL muOS ROMS mount (platform.RomsDir() — /mnt/mmc/ROMS on
// hardware), used verbatim for line 1 so the pointer, and its FNV hash, match what
// muOS itself writes (the engine's "Roms" spelling only lands right through exFAT
// case-insensitivity — the hash would not). ok=false for a rel that is not a rom
// under Roms/<system>/ (defensive: Continue entries always are).
func buildMuosHistTarget(romsRoot, rel string, t time.Time) (muosHistTarget, bool) {
	r := strings.TrimPrefix(filepath.ToSlash(rel), "/")
	parts := strings.SplitN(r, "/", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Roms") {
		return muosHistTarget{}, false
	}
	subAndFile := parts[1]
	i := strings.LastIndexByte(subAndFile, '/')
	if i <= 0 || i == len(subAndFile)-1 {
		return muosHistTarget{}, false // no system folder / no filename
	}
	sub, file := subAndFile[:i], subAndFile[i+1:]
	name := strings.TrimSuffix(file, filepath.Ext(file))
	if name == "" {
		return muosHistTarget{}, false
	}
	path := filepath.Join(romsRoot, filepath.FromSlash(subAndFile))
	return muosHistTarget{
		fname:   fmt.Sprintf("%s-%08X.cfg", name, muosFNV32(path)),
		content: path + "\n" + sub + "\n" + name,
		path:    path,
		key:     muosHistIdentity(sub, name),
		t:       t,
	}, true
}

// muosHistExisting is one .cfg already present in the history dir.
type muosHistExisting struct {
	fname   string
	line1   string // file_path
	key     string // muosHistIdentity from lines 2+3 (line-1 fallback)
	marked  bool   // the pointer's rom name carries a Lodor state marker
	owned   bool   // manifest kind "history"
	mtime   time.Time
	content string
	removed bool
}

// readMuosHistory snapshots the history dir. Pointer files are tiny (3 lines);
// one read each keeps every skip/re-key decision working from recorded fact.
func readMuosHistory(dir string, man *platform.Manifest) []muosHistExisting {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []muosHistExisting
	for _, de := range des {
		if de.IsDir() || !strings.EqualFold(filepath.Ext(de.Name()), ".cfg") {
			continue
		}
		abs := filepath.Join(dir, de.Name())
		data, rerr := os.ReadFile(abs)
		if rerr != nil {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		var l1, l2, l3 string
		if len(lines) > 0 {
			l1 = lines[0]
		}
		if len(lines) > 1 {
			l2 = lines[1]
		}
		if len(lines) > 2 {
			l3 = lines[2]
		}
		if l3 == "" && l1 != "" { // malformed/older shape: fall back to line 1
			b := filepath.Base(l1)
			l3 = strings.TrimSuffix(b, filepath.Ext(b))
		}
		if l2 == "" && l1 != "" {
			l2 = filepath.Base(filepath.Dir(l1))
		}
		e := muosHistExisting{
			fname:   de.Name(),
			line1:   l1,
			key:     muosHistIdentity(l2, l3),
			owned:   man.OwnsKind(abs, platform.ManifestHistory),
			content: string(data),
		}
		b := filepath.Base(l1)
		e.marked = platform.StripLeadingMarker(b) != b || platform.StripLeadingMarker(l3) != l3
		if fi, serr := de.Info(); serr == nil {
			e.mtime = fi.ModTime()
		}
		out = append(out, e)
	}
	return out
}

// muosPathExists reports whether a pointer's file_path resolves to a real file on
// the card — the liveness test behind the #187 dead-pointer heal.
func muosPathExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// muosPathsEqualFold compares two pointer paths the way the card's exFAT does.
func muosPathsEqualFold(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// injectMuosHistory materializes the Continue entries as muOS history pointers in
// dir per the package contract. It mutates man (Record/Forget) but does NOT save
// it — the gated wrapper owns the single save. Returns files written/updated,
// stale Lodor pointers re-keyed away, and entries left alone (foreign recency or
// already current).
func injectMuosHistory(dir, romsRoot string, entries []ContinueEntry, man *platform.Manifest) (wrote, rekeyed, skipped int) {
	if len(entries) == 0 {
		return 0, 0, 0
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "MUOS-HISTORY WARN: mkdir %s: %v\n", dir, err)
		return 0, 0, 0
	}
	existing := readMuosHistory(dir, man)

	for _, ce := range entries {
		tgt, ok := buildMuosHistTarget(romsRoot, ce.Rel, ce.T)
		if !ok {
			continue
		}

		// Classify every same-game file already present.
		var cur *muosHistExisting // ours, already at the target name
		var stale []*muosHistExisting
		var foreignEx *muosHistExisting
		foreign := false
		for i := range existing {
			ex := &existing[i]
			if ex.removed {
				continue
			}
			same := ex.fname == tgt.fname
			if !same && ex.key != tgt.key && !muosPathsEqualFold(ex.line1, tgt.path) {
				continue // unrelated game
			}
			switch {
			case same && (ex.owned || ex.marked):
				cur = ex
			case ex.owned || ex.marked:
				stale = append(stale, ex) // Lodor pointer under an old name (marker flip)
			default:
				foreign = true // the user's own entry — their recency wins
				foreignEx = ex
			}
			if foreign {
				break
			}
		}
		if foreign {
			// HEAL (#187): the user's pointer is sacred — but a pointer whose
			// file_path no longer EXISTS is dead weight: muxhistory toasts "Could
			// not launch" forever (a Lodor rename — marker flip, (RomM)
			// disambiguator, stub->real download — moved the rom out from under
			// it, and unlike the marked case its name gives no proof we did it).
			// When the live target for the SAME game exists on card, re-point it:
			// live 3-line content under the canonical filename, the USER'S mtime
			// preserved (their recency, never the feed's), dead file removed, and
			// ownership NOT taken (no manifest record — it stays their entry and
			// every future run still classifies it foreign-and-skips).
			if foreignEx != nil && !muosPathExists(foreignEx.line1) && muosPathExists(tgt.path) {
				abs := filepath.Join(dir, tgt.fname)
				if err := fsutil.WriteFileAtomicString(abs, tgt.content, 0o644); err == nil {
					_ = os.Chtimes(abs, foreignEx.mtime, foreignEx.mtime)
					if foreignEx.fname != tgt.fname {
						old := filepath.Join(dir, foreignEx.fname)
						if rmErr := os.Remove(old); rmErr == nil || os.IsNotExist(rmErr) {
							foreignEx.removed = true
						}
					}
					rekeyed++ // counted with re-keys: a pointer moved to a live name
					continue
				}
			}
			skipped++
			continue
		}

		// Re-key: drop Lodor pointers stranded under a previous marker/name.
		for _, ex := range stale {
			abs := filepath.Join(dir, ex.fname)
			if err := os.Remove(abs); err == nil || os.IsNotExist(err) {
				man.Forget(abs)
				ex.removed = true
				rekeyed++
			}
		}

		// Already current (same bytes, mtime within FS granularity of the feed
		// time): leave it byte- and stamp-identical — no churn, no false recency.
		if cur != nil && cur.content == tgt.content {
			d := cur.mtime.Sub(tgt.t)
			if d < 0 {
				d = -d
			}
			if d <= muosHistoryMtimeSlack {
				skipped++
				continue
			}
		}

		abs := filepath.Join(dir, tgt.fname)
		if err := fsutil.WriteFileAtomicString(abs, tgt.content, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "MUOS-HISTORY WARN: write %s: %v\n", tgt.fname, err)
			continue
		}
		if err := os.Chtimes(abs, tgt.t, tgt.t); err != nil {
			fmt.Fprintf(os.Stderr, "MUOS-HISTORY WARN: chtimes %s: %v\n", tgt.fname, err)
		}
		man.Record(abs, platform.ManifestHistory, 0)
		wrote++
	}
	return wrote, rekeyed, skipped
}

// maybeInjectMuosHistory is the gated call site (SyncContinue + mirrorContinue —
// both Continue cadences). A NO-OP unless this build's host renders history from
// pointer files (muosHistoryEnabled, -tags muos). Best-effort by contract: a
// history failure must never fail the sync that triggered it; stdout contracts
// stay byte-identical (report goes to stderr only, like the gamelist emitter).
// An empty feed changes nothing — same rule as writeContinueFile.
func maybeInjectMuosHistory(entries []ContinueEntry) {
	if !muosHistoryEnabled || len(entries) == 0 {
		return
	}
	man := platform.LoadManifest()
	wrote, rekeyed, skipped := injectMuosHistory(muosHistoryDir(), platform.RomsDir(), entries, man)
	if err := man.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "MUOS-HISTORY WARN: manifest save: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "MUOS-HISTORY injected=%d rekeyed=%d skipped=%d\n", wrote, rekeyed, skipped)
}
