// Continue HEAD delivery (the one-press root "Continue" entry, task #134).
//
// The NextUI Lodor pak ships a root folder Roms/"0) Continue (LODORCT)" whose
// Emus pak (LODORCT.pak/launch.sh) is a RESUME DISPATCHER: one press on the root
// row must launch the NEWEST cross-device game. The dispatcher is offline shell —
// it cannot ask the server — so the engine persists the head of the Continue list
// it ALREADY derives (ContinueList, newest-first) to a tiny on-card file the
// dispatcher just reads:
//
//	.userdata/shared/Lodor/continue-head.txt
//	<SDCARD-relative rom path (leading '/')>\t<display name>\n   (newest first,
//	up to continueHeadMax lines — the dispatcher launches the first entry it can
//	resolve on the card, falling through past drifted paths; task #135)
//
// Written on the same cadences as the Continue collection (--sync-continue, full
// mirrors); an empty/unreachable feed leaves the existing head alone (same rule
// as writeContinueFile — a transient empty must not erase a good head; worst case
// the dispatcher resumes the newest KNOWN game). Gated on the shared Lodor config
// dir existing, so foreign-host cards never grow NextUI-specific files.
//
// The engine also owns the OPTIONAL dynamic root label (stretch): NextUI's root
// scan aliases folder names via Roms/map.txt (<fs name>\t<alias>; nextui.c
// getRoms — the alias becomes BOTH the display name and the re-sort key). Writing
//
//	0) Continue (LODORCT)\t0) Continue: <game>
//
// keeps the sort key digit-prefixed (still first; trimSortingMeta strips "0) ")
// while the row reads "Continue: <game>". The rewrite PRESERVES every other
// map.txt line verbatim — the pak's own heal owns the Game Manager alias line,
// users may own theirs. All writes are temp+fsync+rename (FAT32/exFAT —
// feedback_lodor_fat32_atomic_writes).
//
// CGO-free, stdlib only.
package catalog

import (
	"os"
	"path/filepath"
	"strings"
)

// continueRootDirName is the on-card Roms/ folder of the one-press Continue root
// entry (the LODORCT dispatcher's dummy folder) — the map.txt alias key.
const continueRootDirName = "0) Continue (LODORCT)"

// continueLabelMax caps the game-name part of the dynamic root label. NextUI
// reads map.txt lines into a 256-byte buffer (getRoms fgets) and truncates long
// rows at render — a short label stays honest in both.
const continueLabelMax = 48

// lodorSharedDir returns the shared Lodor config home on this card, or "" when
// it does not exist (capability gate — only a card the pak has onboarded gets
// host-delivery files).
func lodorSharedDir() string {
	dir := filepath.Join(sdcardRoot(), ".userdata", "shared", "Lodor")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// DisplayNameFor derives the browser-facing display name from an SDCARD-relative
// rom path, mirroring what the user actually sees: the on-disk download-state
// marker ("✘ "/"✓ ", legacy "[^] "/"[v] ") is browser chrome and is stripped
// first, then NextUI getDisplayName's rules (drop 2-4 char extensions, drop
// trailing paren/bracket groups, trim trailing space, never nuke the whole name).
func DisplayNameFor(rel string) string {
	name := filepath.Base(strings.TrimRight(rel, "/"))
	for _, m := range []string{"✘ ", "✓ ", "[^] ", "[v] "} {
		if strings.HasPrefix(name, m) {
			name = strings.TrimPrefix(name, m)
			break
		}
	}
	// remove extension(s), eg. .p8.png — NextUI strips a trailing ".xx".."." + 4
	// (strlen incl. the dot in (2,5]) repeatedly.
	for {
		i := strings.LastIndexByte(name, '.')
		if i < 0 {
			break
		}
		if l := len(name) - i; l > 2 && l <= 5 {
			name = name[:i]
			continue
		}
		break
	}
	// remove trailing parens (round and square), keeping a leading one.
	work := name
	for {
		i := strings.LastIndexAny(name, "([")
		if i <= 0 {
			break
		}
		name = name[:i]
	}
	if name == "" {
		name = work
	}
	return strings.TrimRight(name, " \t")
}

// continueHeadMax caps how many Continue entries the head file carries. A single
// entry proved fragile in the field (task #135, Smart Pro 2026-07-03): the lone head
// named a path that was marker-renamed (✘→✓) minutes after the write, and the
// dispatcher had nothing to fall through to — one press on "Continue: Pokemon -
// Emerald Version" answered "Nothing to continue yet". The dispatcher reads
// top-down and resumes the FIRST entry it can resolve on the card.
const continueHeadMax = 8

// WriteContinueHead persists the top of the Continue list (lines is ContinueList's
// newest-first output) — up to continueHeadMax lines of "<rel>\t<display>\n", newest
// first, so the LODORCT dispatcher can fall through past an entry whose on-card path
// drifted. Returns true when a head file was written. Empty lines or a card without
// the shared Lodor dir => no write, existing file left alone.
func WriteContinueHead(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	dir := lodorSharedDir()
	if dir == "" {
		return false
	}
	n := len(lines)
	if n > continueHeadMax {
		n = continueHeadMax
	}
	var b strings.Builder
	for _, rel := range lines[:n] {
		b.WriteString(rel)
		b.WriteString("\t")
		b.WriteString(DisplayNameFor(rel))
		b.WriteString("\n")
	}
	return writeFileAtomicSync(filepath.Join(dir, "continue-head.txt"), b.String())
}

// UpdateContinueRootLabel rewrites the LODORCT alias line in Roms/map.txt to
// "0) Continue: <display>", preserving every other line verbatim. No-ops (false)
// when the Continue root folder is absent (dispatcher not installed on this
// card), the display is empty, or the file already carries the exact line.
func UpdateContinueRootLabel(display string) bool {
	display = strings.TrimSpace(display)
	if display == "" {
		return false
	}
	romsDir := filepath.Join(sdcardRoot(), "Roms")
	if fi, err := os.Stat(filepath.Join(romsDir, continueRootDirName)); err != nil || !fi.IsDir() {
		return false
	}
	if len(display) > continueLabelMax {
		display = strings.TrimRight(display[:continueLabelMax], " ") + "…"
	}
	ours := continueRootDirName + "\t" + "0) Continue: " + display

	mapPath := filepath.Join(romsDir, "map.txt")
	var out []string
	if data, err := os.ReadFile(mapPath); err == nil {
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimRight(raw, "\r")
			if line == "" {
				continue
			}
			if key, _, found := strings.Cut(line, "\t"); found && key == continueRootDirName {
				continue // ours — replaced below
			}
			out = append(out, line) // foreign/user line: preserved verbatim
		}
	}
	out = append(out, ours)
	content := strings.Join(out, "\n") + "\n"
	if cur, err := os.ReadFile(mapPath); err == nil && string(cur) == content {
		return false // already exact — don't churn the card
	}
	return writeFileAtomicSync(mapPath, content)
}

// writeFileAtomicSync is the FAT32-safe write: temp file, fsync, rename.
func writeFileAtomicSync(path, content string) bool {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return false
	}
	if _, err = f.WriteString(content); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}
