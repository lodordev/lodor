package sync

// Handoff v1 offer + placement (design §6.2, invariants 7.1-7.5).
//
// OFFER policy v1 = Tier-0-equivalent: a server state is compatible iff its
// producer tuple EQUALS this device's tuple for that system (same frontend
// string not required — core@version/arch must match; frontend differences are
// exactly what normalization erases). The certification whitelist (design D8)
// widens this later; starting narrower than the design's ceiling is the safe
// direction. Grout's "builtin" records and tuple-less states list as
// incompatible — visible, never offered for placement.
//
// PLACEMENT never destroys: an occupant of the target slot whose bytes the
// ledger doesn't know is uploaded FIRST (abort on failure), and the occupant is
// renamed to .bak regardless. Delivery is wrapped for RA hosts, raw for minarch
// (D9); a state that fails normalization or the D11 size check is refused.

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"lodor/config"
	"lodor/fsutil"
	"lodor/platform"
	"lodor/romm"
	"lodor/statefmt"
)

// StateOffer is one server state, annotated for the picker/machine listing.
type StateOffer struct {
	ID         int
	Slot       string
	Origin     string // producer tuple (or "foreign:<emulator>" for non-lodor records)
	AgeSeconds int64
	Size       int64
	FileName   string
	Compatible bool
	Known      bool   // this device's ledger carries the record's server id —
	//                   it originated here or was already pulled here. NOT news.
	Why string // "" when compatible; short reason otherwise
}

// ListStatesResult is the cmd contract for --list-states.
type ListStatesResult struct {
	Offers []StateOffer
	Reason string // ok | no-manifest | no-system | none | resolve | offline
}

// ListStates returns the rom's server states, compat-annotated.
func ListStates(client *romm.Client, cfg *config.Config, romPath string) ListStatesResult {
	sc, ok := loadStateCores()
	if !ok {
		return ListStatesResult{Reason: "no-manifest"}
	}
	rom, _, fail, okr := resolveRomAndLocalSavePath(client, cfg, romPath, "")
	if !okr {
		if fail.Outcome == PullError {
			return ListStatesResult{Reason: "offline"}
		}
		return ListStatesResult{Reason: "resolve"}
	}
	info, oks := sc.Systems[rom.PlatformFsSlug]
	if !oks {
		return ListStatesResult{Reason: "no-system"}
	}
	states, err := client.GetStates(rom.ID)
	if err != nil {
		return ListStatesResult{Reason: "offline"}
	}
	if len(states) == 0 {
		return ListStatesResult{Reason: "none"}
	}
	local := sc.tupleFor(info)
	// Ledger server-ids: content-based "seen it" signal (no clock trust) — the
	// launch card's definition of news is compat=1 AND known=0.
	knownIDs := map[int]bool{}
	for _, e := range StateLedgerEntries(rom.ID) {
		if e.ServerID != 0 {
			knownIDs[e.ServerID] = true
		}
	}
	now := time.Now()
	res := ListStatesResult{Reason: "ok"}
	for _, s := range states {
		o := StateOffer{
			ID: s.ID, Size: s.FileSizeBytes, FileName: s.FileName, Known: knownIDs[s.ID],
			Slot: slotFromFileName(s.FileName), AgeSeconds: int64(now.Sub(s.UpdatedAt).Seconds()),
		}
		if strings.HasPrefix(s.Emulator, "lodor/") {
			o.Origin = s.Emulator
			o.Compatible, o.Why = tuplesCompatible(local, s.Emulator)
		} else {
			o.Origin = "foreign:" + s.Emulator
			o.Why = "non-lodor origin (no tuple)"
		}
		res.Offers = append(res.Offers, o)
	}
	return res
}

// tuplesCompatible: base Tier-0 policy (core@version AND arch byte-equal;
// frontend may differ — normalization erases containers) WIDENED by the D8
// certification whitelist (statecompat.go), which only ever adds compatibility,
// never removes it. Tuple shape: lodor/<frontend>/<core[@ver]>/<arch>.
func tuplesCompatible(local, remote string) (bool, string) {
	lc, la, lok := tupleCoreArch(local)
	rc, ra, rok := tupleCoreArch(remote)
	if !lok || !rok {
		return false, "unparseable tuple"
	}
	// Tier-0: exact core@version + arch.
	if lc == rc && la == ra {
		return true, ""
	}
	// D8: a certification class covering both (core, arch) — crosses version and
	// architecture where the harness proved the state format interoperates.
	if certifiedCompatible(lc, la, rc, ra) {
		return true, ""
	}
	if coreName(lc) != coreName(rc) {
		return false, "different core (" + coreName(rc) + " vs " + coreName(lc) + ")"
	}
	if la != ra {
		return false, "different architecture (" + ra + " vs " + la + ") — not certified"
	}
	return false, "different core version — not certified"
}

func tupleCoreArch(t string) (core, arch string, ok bool) {
	parts := strings.Split(t, "/")
	if len(parts) != 4 || parts[0] != "lodor" {
		return "", "", false
	}
	return parts[2], parts[3], true
}

// slotFromFileName recovers the canonical slot from a state filename: our own
// D6 "(lodor s<slot> ...)" tag first, then frontend-convention suffixes.
// Belt-and-braces only — unknown shapes land in "auto" visibility-wise? No:
// they land as "?" and are still pullable to an explicit --state-slot.
func slotFromFileName(name string) string {
	if i := strings.Index(name, "(lodor s"); i >= 0 {
		rest := name[i+len("(lodor s"):]
		if j := strings.IndexByte(rest, ' '); j > 0 {
			return rest[:j]
		}
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".state.auto"):
		return "auto"
	case strings.HasSuffix(lower, ".state"):
		return "0"
	}
	if i := strings.LastIndex(lower, ".state"); i >= 0 {
		if n := lower[i+len(".state"):]; n != "" && len(n) <= 2 && isDigits(n) {
			return n
		}
	}
	if i := strings.LastIndex(lower, ".st"); i >= 0 {
		if n := lower[i+len(".st"):]; len(n) == 1 && isDigits(n) {
			if n == "9" {
				return "auto"
			}
			return n
		}
	}
	return "?"
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// PullStateResult is the cmd contract for --pull-state.
type PullStateResult struct {
	Placed bool
	Path   string
	Reason string // ok | no-manifest | no-system | not-found | incompatible |
	//               size-mismatch | parse | occupant-unsafe | bad-slot |
	//               offline | resolve | write
}

// PullState downloads one server state by id and places it at the local slot.
func PullState(client *romm.Client, cfg *config.Config, romPath string, stateID int, slotOverride string) PullStateResult {
	sc, ok := loadStateCores()
	if !ok {
		return PullStateResult{Reason: "no-manifest"}
	}
	rom, _, fail, okr := resolveRomAndLocalSavePath(client, cfg, romPath, "")
	if !okr {
		if fail.Outcome == PullError {
			return PullStateResult{Reason: "offline"}
		}
		return PullStateResult{Reason: "resolve"}
	}
	info, oks := sc.Systems[rom.PlatformFsSlug]
	if !oks {
		return PullStateResult{Reason: "no-system"}
	}
	states, err := client.GetStates(rom.ID)
	if err != nil {
		return PullStateResult{Reason: "offline"}
	}
	var rec *romm.State
	for i := range states {
		if states[i].ID == stateID {
			rec = &states[i]
			break
		}
	}
	if rec == nil {
		return PullStateResult{Reason: "not-found"}
	}

	// Compat gate — placement is exactly "offering with commitment", same policy.
	local := sc.tupleFor(info)
	if !strings.HasPrefix(rec.Emulator, "lodor/") {
		return PullStateResult{Reason: "incompatible"}
	}
	if okc, _ := tuplesCompatible(local, rec.Emulator); !okc {
		return PullStateResult{Reason: "incompatible"}
	}

	data, err := client.DownloadStateContent(*rec)
	if err != nil || len(data) == 0 {
		return PullStateResult{Reason: "offline"}
	}
	raw, _, err := statefmt.ExtractRaw(data)
	if err != nil {
		return PullStateResult{Reason: "parse"}
	}
	// D11 strict size: when the manifest declares this system's fixed serialize
	// size, a mismatched payload is refused (minarch's tolerant reader would
	// otherwise zero-pad garbage into a "loadable" state).
	if info.Size > 0 && int64(len(raw)) != info.Size {
		return PullStateResult{Reason: "size-mismatch"}
	}

	slot := slotOverride
	if slot == "" {
		slot = slotFromFileName(rec.FileName)
	}
	if slot == "?" {
		slot = "auto"
	}
	dir := platform.StateDirFor(info.Dir)
	target := platform.StateFileForSlot(dir, filepath.Base(romPath), slot)
	if target == "" {
		return PullStateResult{Reason: "bad-slot"}
	}

	// Invariant 7.1: an occupant the server doesn't verifiably have gets
	// uploaded BEFORE we touch it; upload failure aborts the placement.
	// bakPath remembers a renamed occupant so a FAILED write of the new state can
	// restore it — the slot must NEVER be left with only a .bak and no loadable
	// primary file (C1: placement never destroys, even on write failure).
	bakPath := ""
	if occ, err := os.ReadFile(target); err == nil && len(occ) > 0 {
		occRaw, _, perr := statefmt.ExtractRaw(occ)
		if perr != nil {
			// Unparseable occupant: we can't prove it's safe anywhere — preserve
			// via .bak and refuse to also upload garbage.
			occRaw = nil
		}
		if occRaw != nil {
			occSum := bytesMD5(occRaw)
			if !StateKnown(rom.ID, occSum) {
				up, uerr := client.UploadState(rom.ID, local,
					stateUploadName(filepath.Base(romPath), slot, deviceShort(cfg)), occRaw)
				if uerr != nil {
					return PullStateResult{Reason: "occupant-unsafe"}
				}
				_ = RecordState(rom.ID, StateLedgerEntry{
					MD5: occSum, Size: int64(len(occRaw)), ServerID: up.ID, Slot: slot, Origin: local, Own: true,
				})
			}
		}
		if os.Rename(target, target+".bak") == nil {
			bakPath = target + ".bak"
		}
	}

	out := raw
	if platform.StatesUseRANaming() {
		out = statefmt.WrapRASTATE(raw)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		if bakPath != "" {
			_ = os.Rename(bakPath, target) // restore occupant — never strand the slot
		}
		return PullStateResult{Reason: "write"}
	}
	if err := fsutil.WriteFileAtomic(target, out, 0o644); err != nil {
		if bakPath != "" {
			_ = os.Rename(bakPath, target) // restore occupant — never strand the slot
		}
		return PullStateResult{Reason: "write"}
	}
	_ = RecordState(rom.ID, StateLedgerEntry{
		MD5: bytesMD5(raw), Size: int64(len(raw)), ServerID: rec.ID, Slot: slot, Origin: rec.Emulator,
	})
	return PullStateResult{Placed: true, Path: target, Reason: "ok"}
}
