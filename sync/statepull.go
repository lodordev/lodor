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
	"strconv"
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
	// AuthExpired: a server call was rejected for a bad/expired token — mapped to
	// PAIRING_EXPIRED by the cmd layer instead of a silent "offline" (BUG4).
	AuthExpired bool
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
			return ListStatesResult{Reason: "offline", AuthExpired: fail.AuthExpired}
		}
		return ListStatesResult{Reason: "resolve"}
	}
	info, oks := sc.Systems[rom.PlatformFsSlug]
	if !oks {
		return ListStatesResult{Reason: "no-system"}
	}
	states, err := client.GetStates(rom.ID)
	if err != nil {
		return ListStatesResult{Reason: "offline", AuthExpired: romm.IsAuthError(err)}
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
	//               empty | offline | resolve | write
	// AuthExpired: a server call was rejected for a bad/expired token — mapped to
	// PAIRING_EXPIRED by the cmd layer instead of a silent "offline" (BUG4).
	AuthExpired bool
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
			return PullStateResult{Reason: "offline", AuthExpired: fail.AuthExpired}
		}
		return PullStateResult{Reason: "resolve"}
	}
	info, oks := sc.Systems[rom.PlatformFsSlug]
	if !oks {
		return PullStateResult{Reason: "no-system"}
	}
	states, err := client.GetStates(rom.ID)
	if err != nil {
		return PullStateResult{Reason: "offline", AuthExpired: romm.IsAuthError(err)}
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
	if err != nil {
		return PullStateResult{Reason: "offline", AuthExpired: romm.IsAuthError(err)}
	}
	// #10: an empty-but-successful download (HTTP 200, zero bytes) is NOT offline
	// — the server answered, it just has nothing loadable to place. Reporting it
	// as "offline" hid a real, distinct condition (a truncated/empty server
	// record) behind a transient-network reason and invited pointless retries.
	if len(data) == 0 {
		return PullStateResult{Reason: "empty"}
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

	// #10: map the slot to its REAL slot; never silently default an unparseable
	// slot to "auto". slotFromFileName returns "?" only when neither our D6 tag
	// nor any frontend suffix identifies the slot; auto-placing such a state into
	// the auto-resume slot would clobber the live auto-state with an unrelated
	// record. The record is still pullable — the caller must name a slot
	// explicitly via slotOverride (--state-slot); absent that, refuse.
	slot := slotOverride
	if slot == "" {
		slot = slotFromFileName(rec.FileName)
	}
	if slot == "?" {
		return PullStateResult{Reason: "bad-slot"}
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
			// #23: gate on whether the occupant's bytes are CURRENTLY on the server
			// (a live ServerID), not merely ever recorded. StateKnown stays true for
			// bytes that retention has already deleted+forgotten server-side (the MD5
			// entry survives with ServerID zeroed) — using it here would rename the
			// occupant to .bak WITHOUT re-uploading, leaving it alive only on disk and
			// one #8 clobber away from gone. StateOnServer requires the live id.
			if !StateOnServer(rom.ID, occSum) {
				up, uerr := client.UploadState(rom.ID, local,
					stateUploadName(filepath.Base(romPath), slot, deviceShort(cfg)), occRaw)
				if uerr != nil {
					return PullStateResult{Reason: "occupant-unsafe", AuthExpired: romm.IsAuthError(uerr)}
				}
				_ = RecordState(rom.ID, StateLedgerEntry{
					MD5: occSum, Size: int64(len(occRaw)), ServerID: up.ID, Slot: slot, Origin: local, Own: true,
				})
			}
		}
		// #8: never clobber an existing preserved .bak. A 2nd pull to the same slot
		// used to os.Rename(target, target+".bak") unconditionally, destroying the
		// first pull's occupant that the earlier .bak was preserving. Pick a
		// non-colliding name so every prior on-disk occupant survives. (The occupant
		// is also on the server by the gate above — this is the belt to that braces.)
		bakTarget := nonCollidingBakPath(target)
		if os.Rename(target, bakTarget) == nil {
			bakPath = bakTarget
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

// nonCollidingBakPath returns a preserved-occupant backup path that never
// clobbers an existing one (#8). The first occupant preserved for a slot takes
// "<target>.bak" (the historical name — restore-on-write-failure and any
// external tooling still find it); a second, third, … concurrent occupant take
// "<target>.bak.1", ".bak.2", … The scan is bounded belt-and-braces: if a very
// large number of stale .bak files somehow pile up, fall back to a timestamped
// name so the current occupant is still never destroyed.
func nonCollidingBakPath(target string) string {
	base := target + ".bak"
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}
	for i := 1; i < 10000; i++ {
		cand := base + "." + strconv.Itoa(i)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return base + "." + time.Now().UTC().Format("20060102T150405.000000000")
}
