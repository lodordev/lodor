package main

// launchcardv2.go — the launch card CONTENT MODEL (task launch-card-v2).
//
// The v1 card (launchcard.go) was a news gate: three text options shown only
// when the server had something newer. v2 expands it into a full per-game
// menu — the "cover-art hero" design he chose:
//
//	┌──────────────────────────────────┐
//	│ [cover]  LEGEND OF DRAGOON        │
//	│   art    Played 4h 12m            │
//	│          Discs 1/3 on card        │
//	│          4 states - 2 saves       │
//	├──────────────────────────────────┤
//	│  >PLAY   States   Saves   Manage  │
//	│  * 2 loadable here  o 2 other dev │
//	└──────────────────────────────────┘
//
// This file is the PURE layer: fixture-in, model-out, no fb, no engine calls —
// fully unit-testable. Rendering and the input loop live beside it and consume
// this model. Glyph note: the fb font is ASCII-only (font8x8), so the design's
// ● / ◌ / ‹ › render as * / o / < > on the panel.
//
// COMPAT VERDICT (the one rule): a state row is loadable here iff the engine's
// --list-states row says compat=1. That flag is computed by the engine from
// Tier-0 tuple equality widened by the D8 certification whitelist
// (sync.certifiedCompatible) — the SAME verdict --pull-state enforces. The
// card never re-derives compatibility; it would only drift.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"lodor/catalog"
	"lodor/cover"
	"lodor/platform"
	"lodor/ui"
)

// Card actions, in row order. Play is index 0 — the default highlight.
const (
	cardActPlay = iota
	cardActStates
	cardActSaves
	cardActManage
)

var cardActions = []string{"PLAY", "States", "Saves", "Manage"}

// cardState is one server save-state row for the States sub-view.
type cardState struct {
	id, label string
	needs     string // dim rows: what it takes to load it ("needs gpsp on arm64")
	compat    bool   // the engine's compat=1 verdict — bright+selectable iff true
	known     bool   // known=1: this device's ledger has seen it (not news)
	age       int64
}

// cardSave is one server save row for the Saves sub-view.
type cardSave struct {
	id, label string
	current   bool // CURRENT trailer: the save this device already has
}

// cardModel is everything the card renders and the loop navigates. Built once
// per invocation from the two probe outputs; sub-view actions may mark
// evicted afterwards (the delete-the-launch-target edge case).
type cardModel struct {
	path, title string
	downloaded  bool
	hero        []string // present-only info lines: playtime, status, counts

	states              []cardState
	saves               []cardSave
	savesErr, statesErr bool // probe failed (offline/unpaired) — sub-views say so
	saveNewer           bool // --list-saves LOCAL=older (v1 news signal, kept)

	loadableN, otherN int // compat summary: * loadable here / o other device

	sel     int  // action-row cursor
	evicted bool // Manage→Delete ran on THIS rom during the card session
}

// buildCardModel assembles the model from the raw probe outputs. Pure: every
// input is a value; fs facts (downloaded, discs, playtime) are passed in.
func buildCardModel(path, savesOut string, savesErr error, statesOut string, statesErr error,
	downloaded bool, discPresent, discTotal int, playtimeLine string) *cardModel {
	m := &cardModel{
		path:       path,
		title:      cardTitle(path),
		downloaded: downloaded,
		savesErr:   savesErr != nil,
		statesErr:  statesErr != nil,
	}
	if !m.savesErr {
		m.saves = parseSaveRows(savesOut)
		m.saveNewer = listSavesLocal(savesOut) == "older"
	}
	if !m.statesErr {
		m.states = parseCardStates(statesOut)
	}
	for _, s := range m.states {
		if s.compat {
			m.loadableN++
		} else {
			m.otherN++
		}
	}
	if playtimeLine != "" {
		m.hero = append(m.hero, playtimeLine)
	}
	m.hero = append(m.hero, cardStatusLine(downloaded, discPresent, discTotal))
	m.hero = append(m.hero, fmt.Sprintf("%d states - %d saves", len(m.states), len(m.saves)))
	return m
}

// cardTitle is the hero title: basename, state marker stripped, RomM coexist
// tag stripped, extension dropped. "✘ Legend of Dragoon (RomM).m3u" →
// "Legend of Dragoon".
func cardTitle(path string) string {
	base := platform.StripLeadingMarker(filepath.Base(path))
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	return platform.StripRomMTag(stem)
}

// cardStatusLine is the on-card/stub + disc line. Multi-disc (a real disc
// census) wins; otherwise the single-file truth.
func cardStatusLine(downloaded bool, present, total int) string {
	if total > 1 {
		return fmt.Sprintf("Discs %d/%d on card", present, total)
	}
	if downloaded {
		return "On this card"
	}
	return "In your cloud library"
}

// compatSummary is the one-line bright/dim census under the action row.
// ASCII stands in for the design's ● / ◌ (the fb font is 7-bit).
func (m *cardModel) compatSummary() string {
	if len(m.states) == 0 {
		return ""
	}
	return fmt.Sprintf("* %d loadable here   o %d other device", m.loadableN, m.otherN)
}

// moveAction moves the action-row cursor (Left/L1 = -1, Right/R1 = +1), clamped.
func (m *cardModel) moveAction(delta int) {
	m.sel += delta
	if m.sel < 0 {
		m.sel = 0
	}
	if m.sel >= len(cardActions) {
		m.sel = len(cardActions) - 1
	}
}

// parseCardStates parses every LISTSTATE row into a cardState. Incompatible
// rows stay in the list (the timeline is real) but carry needs + compat=false;
// the cursor logic makes them unselectable.
func parseCardStates(out string) []cardState {
	var rows []cardState
	for _, ln := range strings.Split(out, "\n") {
		if !strings.HasPrefix(ln, "LISTSTATE ") {
			continue
		}
		kv := parseStateLine(ln)
		if kv["id"] == "" {
			continue
		}
		lbl := "Slot " + kv["slot"]
		if kv["slot"] == "auto" {
			lbl = "Auto-resume"
		}
		age := int64(1 << 62)
		if a, err := strconv.ParseInt(kv["age"], 10, 64); err == nil {
			age = a
			lbl += "  -  " + humanAge(a)
		}
		lbl += "  -  " + originLabel(kv["origin"])
		st := cardState{
			id:     kv["id"],
			label:  lbl,
			compat: kv["compat"] == "1",
			known:  kv["known"] == "1",
			age:    age,
		}
		if !st.compat {
			st.needs = needsLabel(kv)
		}
		rows = append(rows, st)
	}
	return rows
}

// needsLabel says what a dimmed state needs to load: the producing core+arch
// from its origin tuple, else the engine's why= text, else a plain fallback.
func needsLabel(kv map[string]string) string {
	p := strings.Split(kv["origin"], "/")
	if len(p) == 4 && p[0] == "lodor" {
		core := p[2]
		if i := strings.IndexByte(core, '@'); i >= 0 {
			core = core[:i]
		}
		return "needs " + core + " on " + p[3]
	}
	if w := kv["why"]; w != "" && w != "-" {
		return "needs: " + w
	}
	return "can't load here"
}

// nextLoadable returns the index of the next compat row from cur in dir
// (+1/-1), skipping dimmed rows; cur if none. cur=-1 means "no cursor yet":
// dir=+1 finds the first loadable row (or -1 when none exists at all).
func nextLoadable(states []cardState, cur, dir int) int {
	for i := cur + dir; i >= 0 && i < len(states); i += dir {
		if states[i].compat {
			return i
		}
	}
	if cur >= 0 && cur < len(states) && states[cur].compat {
		return cur
	}
	if cur == -1 && dir > 0 {
		return -1
	}
	return cur
}

// parseSaveRows parses --list-saves rows ("<id>\t<date>\t<who>\t<kb>KB
// [\tCURRENT]"); the single-field LOCAL= trailer drops out via the >=2-field
// filter. Shared with gmServerSaves — one parser, no drift.
func parseSaveRows(out string) []cardSave {
	var saves []cardSave
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Split(ln, "\t")
		if len(f) < 2 {
			continue
		}
		label := f[1]
		if len(f) >= 3 && strings.TrimSpace(f[2]) != "" {
			label += "  -  " + f[2]
		}
		if len(f) >= 4 && strings.TrimSpace(f[3]) != "" {
			label += "  -  " + f[3]
		}
		cur := len(f) >= 5 && f[4] == "CURRENT"
		if cur {
			label += "  (on this device)"
		}
		saves = append(saves, cardSave{id: f[0], label: label, current: cur})
	}
	return saves
}

// cardDiscCounts censuses a multi-disc game: present = playlist lines with real
// bytes beside the .m3u (the local-only shape), total = the manifest's canonical
// disc list (falling back to present when unmanifested). (0,0) for non-m3u or a
// stub playlist — the caller renders the single-file status instead.
func cardDiscCounts(path string) (present, total int) {
	if !strings.EqualFold(filepath.Ext(path), ".m3u") {
		return 0, 0
	}
	lines := catalog.M3UDiscLines(path)
	if len(lines) == 0 {
		return 0, 0
	}
	dir := filepath.Dir(path)
	for _, l := range lines {
		if fi, err := os.Stat(filepath.Join(dir, l)); err == nil && !fi.IsDir() && fi.Size() > 0 {
			present++
		}
	}
	total = present
	if e, ok := platform.LoadManifest().Entry(path); ok && len(e.Discs) > total {
		total = len(e.Discs)
	}
	return present, total
}

// cardCoverPath resolves the .media cover for a ROM: the catalog convention
// (cover.MediaPath on the on-disk name), plus the marker variants — the cover
// file keeps whichever marker the rom wore when it was fetched, and the rom's
// marker flips on download/evict. First existing candidate wins; "" = none.
func cardCoverPath(romPath string) string {
	dir, base := filepath.Dir(romPath), filepath.Base(romPath)
	bare := platform.StripLeadingMarker(base)
	seen := map[string]bool{}
	for _, b := range []string{base, bare, platform.MarkerOnDevice + bare, platform.MarkerCloud + bare} {
		p := cover.MediaPath(filepath.Join(dir, b))
		if seen[p] {
			continue
		}
		seen[p] = true
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			return p
		}
	}
	return ""
}

// ---- rendering + interaction (consumes the pure model) ---------------------

// cardHasNews is the SMART-trigger gate, factored pure from the v1 card: show
// only when the server has a save with newer content-lineage (LOCAL=older) or
// a compatible state this device's ledger has never seen (compat=1 known=0).
// A failed probe is silence, never news.
func cardHasNews(savesOut string, savesErr error, statesOut string, statesErr error) bool {
	saveNewer := savesErr == nil && listSavesLocal(savesOut) == "older"
	_, hasState := newestUnknownCompatState(statesOut)
	if statesErr != nil {
		hasState = false
	}
	return saveNewer || hasState
}

// fitScale returns the largest text scale <= pref at which s fits in maxW
// (floor 1 — a pathological title truncates off-canvas rather than crashing;
// Canvas.Set is bounds-checked).
func fitScale(s string, maxW, pref int) int {
	for sc := pref; sc > 1; sc-- {
		if ui.TextWidth(s, sc) <= maxW {
			return sc
		}
	}
	return 1
}

// fitText clips s to fit maxW at scale sc, appending "..." when it must drop
// characters (task #73: long hero/States lines clipped hard at the 720-wide
// compose edge). Pure and font-exact (font8x8 is monospace, so TextWidth is
// linear). maxW<=0 or an already-fitting string returns s unchanged; a maxW too
// small for even "..." returns as many leading chars as fit. ASCII "..." keeps
// the 7-bit fb font happy.
func fitText(s string, maxW, sc int) string {
	if maxW <= 0 || ui.TextWidth(s, sc) <= maxW {
		return s
	}
	const ell = "..."
	for n := len(s) - 1; n >= 0; n-- {
		if ui.TextWidth(s[:n]+ell, sc) <= maxW {
			return s[:n] + ell
		}
	}
	// Not even "..." fits: fall back to the longest bare prefix that does.
	for n := len(s); n >= 0; n-- {
		if ui.TextWidth(s[:n], sc) <= maxW {
			return s[:n]
		}
	}
	return ""
}

// Hero cover box geometry (720x480 panel).
const cardCoverW, cardCoverH = 150, 190

// renderCard draws the full cover-art-hero card into a fresh canvas. Pure
// function of (theme, model, cover) — the render smoke tests call it directly.
func renderCard(t ui.Theme, m *cardModel, cov *ui.Canvas) *ui.Canvas {
	c := ui.NewCanvas(W, H)
	x, y, w, _ := t.Frame(c, "Lodor", "</>/L1/R1: move   A: select   B: play")

	// HERO: cover art left (placeholder when missing/undecodable), text right.
	c.FillRect(x, y, cardCoverW, cardCoverH, t.Panel)
	if cov != nil {
		c.BlitScaled(cov, x+4, y+4, cardCoverW-8, cardCoverH-8)
	} else {
		c.Rect(x, y, cardCoverW, cardCoverH, t.Dim)
		c.DrawTextCentered(x, y+cardCoverH/2-wizGlyphH, cardCoverW, "NO ART", t.Dim, t.SmallScale)
	}
	tx := x + cardCoverW + 20
	tw := w - cardCoverW - 20
	ts := fitScale(m.title, tw, t.TitleScale-1)
	// #73: fitScale shrinks the scale, but a title too long even at scale 1 would still
	// clip at the compose edge — ellipsize as the final guard so nothing runs off-canvas.
	c.DrawText(tx, y, fitText(m.title, tw, ts), t.Accent, ts)
	yy := y + wizGlyphH*ts + 16
	for _, ln := range m.hero {
		c.DrawText(tx, yy, fitText(ln, tw, t.BodyScale), t.Text, t.BodyScale)
		yy += wizGlyphH*t.BodyScale + 10
	}
	if m.saveNewer {
		c.DrawText(tx, yy, fitText("Newer save on your server", tw, t.BodyScale), t.Good, t.BodyScale)
		yy += wizGlyphH*t.BodyScale + 10
	}
	if m.savesErr || m.statesErr {
		c.DrawText(tx, yy, fitText("Server unreachable - local info only", tw, t.SmallScale), t.Bad, t.SmallScale)
	}

	// Separator.
	sy := y + cardCoverH + 12
	c.FillRect(x, sy, w, 2, t.Accent)

	// ACTION ROW: horizontal, selected cell gets the panel + accent underline
	// and a ">" prefix (the design's ▸ in the fb font's ASCII).
	ay := sy + 12
	rowH := wizGlyphH*t.BodyScale + 18
	ax := x
	for i, a := range cardActions {
		label := a
		if i == m.sel {
			label = ">" + a
		}
		cw := ui.TextWidth(label, t.BodyScale) + 24
		if i == m.sel {
			c.FillRect(ax, ay, cw, rowH, t.Panel)
			c.FillRect(ax, ay+rowH-3, cw, 3, t.Accent)
		}
		col := t.Dim
		if i == m.sel {
			col = t.Text
		}
		c.DrawText(ax+12, ay+9, label, col, t.BodyScale)
		ax += cw + 14
	}

	// COMPAT SUMMARY: * loadable here / o other device. (#73: width-fit so it can't
	// clip at the 720-wide compose edge — w is the framed content width.)
	if s := m.compatSummary(); s != "" {
		c.DrawText(x, ay+rowH+12, fitText(s, w, t.SmallScale), t.Dim, t.SmallScale)
	}
	return c
}

// runCardLoop drives the card until the user plays (A on Play, B, or Start —
// returning IS the launch on hook lanes). Sub-views run nested and come back
// here. Never returns non-zero state: the caller always exits 0.
func (w *wizard) runCardLoop(m *cardModel, cov *ui.Canvas, draw func(*ui.Canvas), btn func() ui.Button) {
	for {
		draw(renderCard(w.t, m, cov))
		switch btn() {
		case ui.BtnBack, ui.BtnStart:
			return // play
		case ui.BtnLeft, ui.BtnL1:
			m.moveAction(-1)
		case ui.BtnRight, ui.BtnR1:
			m.moveAction(+1)
		case ui.BtnConfirm:
			switch m.sel {
			case cardActPlay:
				return // play
			case cardActStates:
				w.cardStatesView(m, draw, btn)
			case cardActSaves:
				w.cardSavesView(m, draw, btn)
			case cardActManage:
				w.cardManageView(m, draw, btn)
			}
		}
	}
}

// Sub-views. Implemented incrementally on this branch: each is wired into the
// loop above and lands with its own tests. A stub is a NO-OP (returns to the
// card, draws nothing) — never a fake screen.

// cardStatesView is the States sub-view: EVERY server state renders (the
// timeline is real) — bright "*" rows are loadable here (the engine's
// compat=1 verdict) and selectable; dimmed "o" rows show their needs label
// and the cursor SKIPS them. A on a loadable row places it via the existing
// --pull-state mode and returns to the card; B returns without touching
// anything. With zero loadable rows there is no cursor and A is a no-op.
func (w *wizard) cardStatesView(m *cardModel, draw func(*ui.Canvas), btn func() ui.Button) {
	if m.statesErr {
		w.showMsg("States", "Server unreachable - can't list save states.", w.t.Bad, draw, btn)
		return
	}
	if len(m.states) == 0 {
		w.showMsg("States", "No save states on your server for "+m.title+".", w.t.Text, draw, btn)
		return
	}
	cur := nextLoadable(m.states, -1, +1) // -1 when nothing is loadable
	off := 0
	for {
		c := ui.NewCanvas(W, H)
		x, y, ww, hh := w.t.Frame(c, "States - "+m.title, "Up/Down: move   A: place here   B: back")
		rowH := wizGlyphH*w.t.BodyScale + 18
		vis := hh / rowH
		if vis < 1 {
			vis = 1
		}
		if cur >= 0 {
			if cur < off {
				off = cur
			}
			if cur >= off+vis {
				off = cur - vis + 1
			}
		}
		end := off + vis
		if end > len(m.states) {
			end = len(m.states)
		}
		for i := off; i < end; i++ {
			st := m.states[i]
			ry := y + (i-off)*rowH
			if i == cur {
				c.FillRect(x, ry, ww, rowH, w.t.Panel)
				c.FillRect(x, ry, 6, rowH, w.t.Accent)
			}
			mark, col := "o", w.t.Dim
			if st.compat {
				mark = "*"
				if i == cur {
					col = w.t.Text
				}
			}
			lbl := mark + " " + st.label
			if !st.compat && st.needs != "" {
				lbl += "  -  " + st.needs
			}
			c.DrawText(x+16, ry+9, fitText(lbl, ww-30, w.t.BodyScale), col, w.t.BodyScale)
		}
		if off > 0 {
			c.DrawText(x+ww-14, y+2, "^", w.t.Accent, w.t.BodyScale)
		}
		if end < len(m.states) {
			c.DrawText(x+ww-14, y+(vis-1)*rowH+9, "v", w.t.Accent, w.t.BodyScale)
		}
		draw(c)
		switch btn() {
		case ui.BtnBack:
			return
		case ui.BtnUp:
			cur = nextLoadable(m.states, cur, -1)
		case ui.BtnDown:
			cur = nextLoadable(m.states, cur, +1)
		case ui.BtnConfirm, ui.BtnStart:
			if cur < 0 || !m.states[cur].compat {
				continue // dimmed rows are unselectable by construction
			}
			st := m.states[cur]
			w.working(draw, "Placing state...")
			// --pull-state mutates the on-card state file; guard against a mid-write
			// watchdog auto-exit orphaning the engine child (lodor#63).
			pout, perr := w.runEngineMutating("--pull-state", m.path, "--state-id", st.id)
			if exitCode(perr) == 0 && ui.ResultToken(pout, "placedstate") == "1" {
				w.flash(draw, "State placed - load it from the in-game menu.", w.t.Good)
				fmt.Println("LAUNCHCARD action=pull-state placed=1")
			} else {
				w.flash(draw, "Couldn't place the state - nothing was changed.", w.t.Bad)
				fmt.Println("LAUNCHCARD action=pull-state placed=0 reason=" + ui.ResultToken(pout, "reason"))
			}
			return
		}
	}
}

// cardSavesView is the Saves sub-view: the server save list (date/device/
// size, CURRENT tagged), restore any via the existing --restore-save (the
// engine preserves the occupant first — invariant, not UI copy). B backs out.
func (w *wizard) cardSavesView(m *cardModel, draw func(*ui.Canvas), btn func() ui.Button) {
	if m.savesErr {
		w.showMsg("Saves", "Server unreachable - can't list saves.", w.t.Bad, draw, btn)
		return
	}
	if len(m.saves) == 0 {
		w.showMsg("Saves", "No server saves for "+m.title+".", w.t.Text, draw, btn)
		return
	}
	labels := make([]string, len(m.saves))
	for i, sv := range m.saves {
		labels[i] = sv.label
	}
	sm := &ui.ScrollMenu{Items: labels}
	sel, ok := w.pickScroll("Saves - "+m.title, "Up/Down: move   A: restore   B: back", sm, draw, btn)
	if !ok {
		return
	}
	sv := m.saves[sel]
	w.working(draw, "Restoring save...")
	// --restore-save rewrites the on-card save (occupant preserved first, then swapped);
	// guard against a mid-write watchdog auto-exit orphaning the engine child (lodor#63).
	rout, rerr := w.runEngineMutating("--restore-save", m.path, sv.id)
	if exitCode(rerr) == 0 && ui.ResultToken(rout, "restored") == "1" {
		w.flash(draw, "Save restored - your previous save is kept.", w.t.Good)
		fmt.Println("LAUNCHCARD action=restore-save restored=1")
	} else {
		w.flash(draw, "Couldn't restore - nothing was changed.", w.t.Bad)
		fmt.Println("LAUNCHCARD action=restore-save restored=0")
	}
}

// cardManageView is the Manage sub-view: the existing engine modes behind a
// small picker — Download now (stubs), Delete from card (downloaded), and
// Details. Reuses the Game Manager handlers verbatim (confirm flows, honest
// results, Wi-Fi gating included).
//
// DELETE-THE-LAUNCH-TARGET EDGE CASE (documented contract): deleting the
// game this card is gating turns the about-to-launch ROM into a 0-byte cloud
// stub. That is SAFE by design — the card still exits 0 and the launch
// proceeds onto the stub, which is exactly the shape every lane already
// handles: hook lanes route stubs through download-on-launch (re-download,
// then play), and stock launchers show the honest cloud state. The card logs
// the fact (launch_target=stubbed), updates its own status line, and never
// pretends the file is still on card.
func (w *wizard) cardManageView(m *cardModel, draw func(*ui.Canvas), btn func() ui.Button) {
	items := []string{"Download now", "Details"}
	if m.downloaded {
		items = []string{"Delete from card", "Details"}
	}
	sm := &ui.ScrollMenu{Items: items}
	sel, ok := w.pickScroll("Manage - "+m.title, "Up/Down: move   A: select   B: back", sm, draw, btn)
	if !ok {
		return
	}
	switch items[sel] {
	case "Download now":
		if w.gmDownload(m.path, draw, btn) {
			m.downloaded = true
			m.rebuildStatus()
			fmt.Println("LAUNCHCARD action=download downloaded=1")
		}
	case "Delete from card":
		if w.gmDelete(m.path, draw, btn) {
			m.evicted = true
			m.downloaded = false
			m.rebuildStatus()
			fmt.Println("LAUNCHCARD action=evict evicted=1 launch_target=stubbed")
		}
	case "Details":
		w.gmDetails(m.path, draw, btn)
	}
}

// rebuildStatus refreshes the hero'"'"'s status line after a Manage action
// changed the on-card truth (download/evict). The status line is always the
// second-from-last hero line (counts is last; playtime, when present, first).
func (m *cardModel) rebuildStatus() {
	if len(m.hero) < 2 {
		return
	}
	present, total := cardDiscCounts(m.path)
	m.hero[len(m.hero)-2] = cardStatusLine(m.downloaded, present, total)
}
