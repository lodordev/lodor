// Gamelist emitter (task #186 — Lodor for Knulli, Phase A).
//
// Batocera-derived hosts (Knulli) render their library from a per-folder
// gamelist.xml EmulationStation reads: <game><path>./file</path><name>…</name>
// <image>…</image></game>. Lodor keeps its ✘/✓ sync-state markers BAKED in the
// on-disk filenames (the engine's marker machinery is unchanged), and instead
// gives ES a clean display name through the gamelist: <name> is the
// marker-stripped title, <image> the engine-fetched cover. So the raw file
// listing shows sync state while the launcher shows a clean library.
//
// MERGE SEMANTICS (the user's gamelist is THEIRS): the writer parses any existing
// gamelist.xml and rewrites ONLY the entries whose <path> the mirror-owned
// manifest claims (stubs + downloads). Every foreign <game> — and any non-game
// element (<folder>, <provider>, …) — is preserved verbatim in document order via
// a raw-innerxml passthrough, so scraper data (<desc>, <rating>, attributes) is
// never dropped or re-shaped. For OUR entries, only <path>/<name>/<image> are
// (re)written; any children a scraper added to our entry survive. A ✘→✓ marker
// flip updates the owned entry's <path> in place: a marker-carrying path is never
// the user's (markers are Lodor's artifact), so it is safe to re-key it by its
// marker-stripped stem. An unparseable existing gamelist.xml is left byte-
// untouched (WARN + skip) — never clobbered.
//
// Writes are FAT32-atomic via fsutil (temp+fsync+rename+dir-fsync); an unchanged
// merge writes nothing (no mtime churn → no needless ES rescans). Each written
// gamelist.xml is recorded in the mirror-manifest (kind "gamelist").
//
// This file is COMPILED ON EVERY TAG (the merge logic is host-agnostic and
// tested tag-free); the call sites gate on gamelistEnabled, a build-tag constant
// that is true only under -tags knulli — every other platform's behavior stays
// byte-identical (dead code). CGO-free, stdlib only.
package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/config"
	"lodor/cover"
	"lodor/fsutil"
	"lodor/platform"
)

// gamelistFileName is the EmulationStation per-folder library file.
const gamelistFileName = "gamelist.xml"

// glEntry is one Lodor-owned gamelist entry: the on-disk filename (marker
// included), the clean display name, and the cover path ("" = no cover on card).
type glEntry struct {
	fsname string // on-disk base name, e.g. "✘ Game (USA).gba"
	name   string // marker-stripped stem, e.g. "Game (USA)"
	image  string // "./.media/<stem>.png" when the cover file exists, else ""
}

// rawXMLNode is the verbatim-passthrough shape for foreign XML: element name,
// attributes, and raw inner XML. xml ",innerxml" is written back UNMODIFIED on
// marshal, so unknown children/structure survive a parse→merge→marshal round trip.
type rawXMLNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Inner   string     `xml:",innerxml"`
}

// gamelistDoc is the <gameList> document: every child element captured in
// document order as a raw node (games AND non-game elements alike).
type gamelistDoc struct {
	XMLName xml.Name     `xml:"gameList"`
	Nodes   []rawXMLNode `xml:",any"`
}

// gamelistSDRoot mirrors platform.manifestRel's root (SDCARD_PATH, default
// /mnt/SDCARD): manifest entries are stored relative to it, and this is the same
// join uninstall uses to get back to absolute paths.
func gamelistSDRoot() string {
	if sd := os.Getenv("SDCARD_PATH"); sd != "" {
		return sd
	}
	return "/mnt/SDCARD"
}

// maybeWriteGamelists refreshes gamelist.xml for the given rom directories (none
// = every directory holding mirror-owned roms) — a NO-OP unless this build's host
// reads gamelists (gamelistEnabled, -tags knulli). Best-effort by contract: a
// gamelist failure must never fail the mirror/reconcile/evict that triggered it;
// problems go to stderr only, stdout contracts stay byte-identical.
func maybeWriteGamelists(cfg *config.Config, dirs ...string) {
	if !gamelistEnabled {
		return
	}
	var only map[string]bool
	if len(dirs) > 0 {
		only = map[string]bool{}
		for _, d := range dirs {
			if d != "" {
				only[filepath.Clean(d)] = true
			}
		}
	}
	if _, _, err := emitGamelists(cfg, only); err != nil {
		fmt.Fprintf(os.Stderr, "GAMELIST WARN: %s\n", safeErr(err))
	}
}

// runWriteGamelists is the standalone --write-gamelists mode: rebuild every
// owned gamelist.xml from the manifest. Offline (filesystem only). Contract:
//
//	RESULT gamelists=<files written> entries=<owned entries ensured>
//
// On a non-knulli build the mode REFUSES (exit 2): no other host reads
// gamelist.xml, and writing one there would be silent noise in the user's tree.
func runWriteGamelists(cfg *config.Config) {
	if !gamelistEnabled {
		fmt.Fprintln(os.Stderr, "FATAL flag: --write-gamelists is only supported on the knulli build (this host's launcher does not read gamelist.xml)")
		os.Exit(2)
	}
	files, entries, err := emitGamelists(cfg, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL gamelists: %s\n", safeErr(err))
		os.Exit(4)
	}
	fmt.Printf("RESULT gamelists=%d entries=%d\n", files, entries)
	os.Exit(0)
}

// emitGamelists walks the mirror-owned manifest, groups owned ROM presences
// (stubs + downloads) by their rom directory, and merge-writes each directory's
// gamelist.xml. only (nil = all) restricts to specific absolute directories.
// Returns files actually written (unchanged merges don't count) and the total
// owned entries ensured across all processed directories. Per-directory parse
// failures are WARN+skip (never clobber a user file, never abort the walk);
// only a manifest-save failure is returned as err.
func emitGamelists(cfg *config.Config, only map[string]bool) (files, entries int, err error) {
	man := platform.LoadManifest()
	sd := gamelistSDRoot()
	romsRoot := filepath.Clean(platform.RomsDir())

	// Group owned rom basenames by directory. Manifest keys are SDCARD-relative
	// (leading "/"); rejoin under the same root uninstall uses.
	byDir := map[string][]string{}
	for rel, e := range man.Entries {
		if e.Kind != platform.ManifestStub && e.Kind != platform.ManifestDownload {
			continue
		}
		abs := filepath.Join(sd, rel)
		dir := filepath.Dir(abs)
		if dir != romsRoot && !strings.HasPrefix(dir, romsRoot+string(filepath.Separator)) {
			continue // not under the roms tree (defensive)
		}
		if dir == romsRoot {
			continue // never write a gamelist at the roms ROOT — only per-system folders
		}
		if only != nil && !only[dir] {
			continue
		}
		byDir[dir] = append(byDir[dir], filepath.Base(abs))
	}

	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		names := byDir[dir]
		sort.Strings(names) // deterministic append order for brand-new entries
		ours := make([]glEntry, 0, len(names))
		for _, n := range names {
			ours = append(ours, buildGamelistEntry(dir, n))
		}
		glPath := filepath.Join(dir, gamelistFileName)
		existing, rerr := os.ReadFile(glPath)
		if rerr != nil && !os.IsNotExist(rerr) {
			fmt.Fprintf(os.Stderr, "GAMELIST WARN %s: unreadable (%v) — skipped\n", glPath, rerr)
			continue
		}
		merged, merr := mergeGamelist(existing, ours)
		if merr != nil {
			// Unparseable existing file: the user's bytes stay byte-identical.
			fmt.Fprintf(os.Stderr, "GAMELIST WARN %s: %v — left untouched\n", glPath, merr)
			continue
		}
		entries += len(ours)
		if bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(merged)) {
			continue // no change — no write, no mtime churn
		}
		if werr := fsutil.WriteFileAtomic(glPath, merged, 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "GAMELIST WARN %s: write failed (%v)\n", glPath, werr)
			continue
		}
		man.Record(glPath, platform.ManifestGamelist, 0)
		files++
	}
	if serr := man.Save(); serr != nil {
		return files, entries, fmt.Errorf("manifest save: %w", serr)
	}
	return files, entries, nil
}

// buildGamelistEntry derives one owned entry from a rom's on-disk basename:
// <name> is the marker-stripped, extension-less stem; <image> points at the
// engine's cover convention (.media/<on-disk stem>.png — cover.MediaPath anchors
// at the MARKED on-disk name, which migrateMarkedGame renames in lockstep with
// the rom) and is emitted ONLY when the cover file actually exists on the card.
func buildGamelistEntry(dir, fsname string) glEntry {
	stem := strings.TrimSuffix(fsname, filepath.Ext(fsname))
	e := glEntry{
		fsname: fsname,
		name:   platform.StripLeadingMarker(stem),
	}
	romAbs := filepath.Join(dir, fsname)
	if cover.Exists(romAbs) {
		e.image = "./.media/" + stem + ".png"
	}
	return e
}

// mergeGamelist merges Lodor-owned entries into an existing gamelist.xml body
// (nil/empty = start fresh) and returns the full new document bytes.
//
// Rules (in document order):
//   - a <game> whose path exactly matches an owned on-disk name is OURS →
//     path/name/image updated in place, other children preserved verbatim;
//   - a <game> whose path carries a leading ✘/✓/legacy marker AND marker-strips
//     to an owned name is OURS under an old marker (the ✘→✓ flip) → same update,
//     path re-pointed at the current on-disk name. User files never carry
//     markers, so this can never capture a foreign entry;
//   - duplicate matches for one owned rom are dropped (self-heal);
//   - everything else — foreign <game>s and non-game elements — passes through
//     verbatim;
//   - owned roms with no existing entry are appended (sorted by the caller).
func mergeGamelist(existing []byte, ours []glEntry) ([]byte, error) {
	doc := gamelistDoc{XMLName: xml.Name{Local: "gameList"}}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := xml.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("existing gamelist does not parse: %w", err)
		}
	}

	// Owned lookups: exact on-disk name, and marker-stripped canonical name.
	byExact := map[string]int{}
	byCanon := map[string]int{}
	for i, e := range ours {
		byExact[e.fsname] = i
		byCanon[platform.StripLeadingMarker(e.fsname)] = i
	}

	emitted := make([]bool, len(ours))
	outNodes := make([]rawXMLNode, 0, len(doc.Nodes)+len(ours))
	for _, node := range doc.Nodes {
		if node.XMLName.Local != "game" {
			outNodes = append(outNodes, node) // <folder>/<provider>/… — verbatim
			continue
		}
		base := gamelistEntryBase(node.Inner)
		idx, matched := byExact[base]
		if !matched && platform.HasLeadingMarker(base) {
			idx, matched = byCanon[platform.StripLeadingMarker(base)]
		}
		if !matched {
			outNodes = append(outNodes, node) // foreign game — verbatim
			continue
		}
		if emitted[idx] {
			continue // duplicate row for one owned rom — drop (self-heal)
		}
		upd, err := updateGameNode(node, ours[idx])
		if err != nil {
			return nil, err
		}
		outNodes = append(outNodes, upd)
		emitted[idx] = true
	}
	for i, e := range ours {
		if !emitted[i] {
			outNodes = append(outNodes, newGameNode(e))
		}
	}
	doc.Nodes = outNodes

	body, err := xml.MarshalIndent(&doc, "", "  ")
	if err != nil {
		return nil, err
	}
	out := append([]byte(xml.Header), append(body, '\n')...)
	// Belt: the output must itself parse (the foreign innerxml is verbatim user
	// content — prove the assembled document is still well-formed before it can
	// ever replace theirs).
	if verr := xml.Unmarshal(out, &gamelistDoc{}); verr != nil {
		return nil, fmt.Errorf("merged gamelist failed self-check: %w", verr)
	}
	return out, nil
}

// gamelistEntryBase extracts a <game>'s path and normalizes it to the bare
// basename ("./✘ Game.gba" → "✘ Game.gba") for matching against owned names.
// Returns "" when the entry has no parseable path (never matches — preserved).
func gamelistEntryBase(inner string) string {
	var g struct {
		Path string `xml:"path"`
	}
	if xml.Unmarshal([]byte("<game>"+inner+"</game>"), &g) != nil {
		return ""
	}
	p := strings.TrimSpace(g.Path)
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

// newGameNode builds a fresh owned <game> node (path, name, image-if-any).
func newGameNode(e glEntry) rawXMLNode {
	children := []rawXMLNode{
		textNode("path", "./"+e.fsname),
		textNode("name", e.name),
	}
	if e.image != "" {
		children = append(children, textNode("image", e.image))
	}
	return rawXMLNode{XMLName: xml.Name{Local: "game"}, Inner: marshalChildren(children)}
}

// updateGameNode rewrites an existing owned <game>'s path/name (and image when a
// cover exists), preserving its attributes and EVERY other child verbatim in
// place (scraper <desc>/<rating>/… survive). A missing path/name/image child is
// appended. An existing <image> is left alone when we have no cover to point at
// (never blank out art the user scraped themselves).
func updateGameNode(node rawXMLNode, e glEntry) (rawXMLNode, error) {
	var wrapper struct {
		Nodes []rawXMLNode `xml:",any"`
	}
	if err := xml.Unmarshal([]byte("<game>"+node.Inner+"</game>"), &wrapper); err != nil {
		return rawXMLNode{}, fmt.Errorf("owned game entry does not parse: %w", err)
	}
	havePath, haveName, haveImage := false, false, false
	children := wrapper.Nodes
	for i := range children {
		switch children[i].XMLName.Local {
		case "path":
			children[i] = textNode("path", "./"+e.fsname)
			havePath = true
		case "name":
			children[i] = textNode("name", e.name)
			haveName = true
		case "image":
			if e.image != "" {
				children[i] = textNode("image", e.image)
			}
			haveImage = true
		}
	}
	if !havePath {
		children = append(children, textNode("path", "./"+e.fsname))
	}
	if !haveName {
		children = append(children, textNode("name", e.name))
	}
	if !haveImage && e.image != "" {
		children = append(children, textNode("image", e.image))
	}
	return rawXMLNode{XMLName: node.XMLName, Attrs: node.Attrs, Inner: marshalChildren(children)}, nil
}

// textNode builds a simple <name>escaped text</name> raw node.
func textNode(name, text string) rawXMLNode {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(text))
	return rawXMLNode{XMLName: xml.Name{Local: name}, Inner: b.String()}
}

// marshalChildren renders child nodes back into a parent's inner XML. rawXMLNode
// marshals its Inner verbatim, so foreign children round-trip byte-preserved.
func marshalChildren(children []rawXMLNode) string {
	var b bytes.Buffer
	for i := range children {
		out, err := xml.Marshal(&children[i])
		if err != nil {
			continue // structurally impossible for parsed input; defensive
		}
		b.WriteString("\n    ")
		b.Write(out)
	}
	if b.Len() > 0 {
		b.WriteString("\n  ")
	}
	return b.String()
}
