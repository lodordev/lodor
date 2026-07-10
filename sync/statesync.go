package sync

// Handoff v1 push path (design lodor-statesync-design-2026-07-07.md §6.1).
//
// States are an APPEND-ONLY timeline: pushing can never destroy anything, on
// either side. All the risk lives in offering/placement (statesync pull, next
// step) — this file only ingests local states, normalizes them (statefmt),
// dedups against the engine's own ledger, and uploads with the producer tuple.
//
// The producer tuple and the lane-specific state-directory component come from
// the lane's statecores.json manifest (design D7), baked at assemble time next
// to config.json. NO manifest → the whole feature no-ops honestly ("dark
// launch": uploads nothing, offers nothing, breaks nothing).

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lodor/catalog"
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
	"lodor/statefmt"
)

// stateCoreInfo is one system's pinned-core record in statecores.json.
type stateCoreInfo struct {
	Core    string `json:"core"`              // core project ("gpsp")
	Version string `json:"version,omitempty"` // advisory only (prior-art: version strings lie)
	Dir     string `json:"dir"`               // lane-specific state-dir component
	Size    int64  `json:"size,omitempty"`    // fixed serialize size when known (D11)
}

// stateCores is the lane manifest: which core each system is pinned to, where
// its states live on THIS host, and the frontend/arch half of the tuple.
type stateCores struct {
	Version  int                      `json:"version"`
	Frontend string                   `json:"frontend"` // "lodoros"|"nextui"|"muos"|"knulli"|"onion"
	Arch     string                   `json:"arch"`     // "armhf"|"arm64"
	Systems  map[string]stateCoreInfo `json:"systems"`  // keyed by RomM platform fs_slug
}

func stateCoresPath() string {
	return filepath.Join(platform.PakDir(), "statecores.json")
}

// loadStateCores reads the manifest; ok=false (feature dark) when absent or
// structurally unusable — fail closed, never guess a tuple (design D7).
func loadStateCores() (stateCores, bool) {
	var sc stateCores
	data, err := os.ReadFile(stateCoresPath())
	if err != nil {
		return sc, false
	}
	if json.Unmarshal(data, &sc) != nil || sc.Frontend == "" || sc.Arch == "" || len(sc.Systems) == 0 {
		return sc, false
	}
	return sc, true
}

// tupleFor renders the producer tuple for a system (design D3):
// lodor/<frontend>/<core>@<version>/<arch>. Version may be empty → omitted.
func (sc stateCores) tupleFor(info stateCoreInfo) string {
	core := info.Core
	if info.Version != "" {
		core += "@" + info.Version
	}
	return "lodor/" + sc.Frontend + "/" + core + "/" + sc.Arch
}

// PushStatesResult is the cmd-layer contract for --push-states.
type PushStatesResult struct {
	Pushed  int
	Skipped int // already on server (ledger dedup) or over size cap
	Failed  int
	Retired int // own old uploads deleted server-side after a landed push (6.4)
	Reason  string // ok | no-manifest | no-system | no-states | resolve | offline
	// AuthExpired is set when a server call this run was rejected because the
	// client-token is invalid/expired/revoked (romm.AuthError). The cmd layer
	// maps it to the PAIRING_EXPIRED contract (stdout line + exit 6) — a 401 must
	// NEVER be swallowed as a per-file failure or "offline" (false success: the
	// user is never told to re-pair and states silently never upload).
	AuthExpired bool
}

// maxStateUpload caps a single state upload (PSX-class states are MBs; anything
// past this is misconfiguration, not a save state).
const maxStateUpload = 64 << 20

// PushStates uploads every local state for romPath that the server doesn't
// already have (by the engine's own ledger). Additive only; never blocks on
// failures of individual files.
func PushStates(client *romm.Client, cfg *config.Config, romPath string) PushStatesResult {
	sc, ok := loadStateCores()
	if !ok {
		return PushStatesResult{Reason: "no-manifest"}
	}
	rom, _, fail, okr := resolveRomAndLocalSavePath(client, cfg, romPath, "")
	if !okr {
		if fail.Outcome == PullError {
			// A token-rejection during resolve is a dead pairing, not "offline":
			// surface it so the mode emits PAIRING_EXPIRED instead of a silent retry.
			return PushStatesResult{Reason: "offline", AuthExpired: fail.AuthExpired}
		}
		return PushStatesResult{Reason: "resolve"}
	}
	info, oks := sc.Systems[rom.PlatformFsSlug]
	if !oks {
		return PushStatesResult{Reason: "no-system"}
	}
	dir := platform.StateDirFor(info.Dir)
	if dir == "" {
		return PushStatesResult{Reason: "no-system"}
	}
	romBase := filepath.Base(romPath)
	files := platform.StateFilesForRom(dir, romBase)
	if len(files) == 0 {
		return PushStatesResult{Reason: "no-states"}
	}

	tuple := sc.tupleFor(info)
	device := deviceShort(cfg)
	res := PushStatesResult{Reason: "ok"}
	for _, sf := range files {
		data, err := os.ReadFile(sf.Path)
		if err != nil || len(data) == 0 {
			res.Failed++
			continue
		}
		if len(data) > maxStateUpload {
			res.Skipped++
			continue
		}
		raw, _, err := statefmt.ExtractRaw(data)
		if err != nil {
			// Invariant 7.4: unparseable artifacts are terminal per-file.
			res.Failed++
			continue
		}
		sum := bytesMD5(raw)
		if StateKnown(rom.ID, sum) {
			res.Skipped++
			continue
		}
		name := stateUploadName(romBase, sf.Slot, device)
		up, err := client.UploadState(rom.ID, tuple, name, raw)
		if err != nil {
			// A 401/expired-token upload is NOT a generic per-file failure: flag it
			// so the mode surfaces PAIRING_EXPIRED and the user re-pairs (//BUG4). The
			// file still counts as failed for the summary; the auth flag rides alongside.
			if romm.IsAuthError(err) {
				res.AuthExpired = true
			}
			res.Failed++
			continue
		}
		_ = RecordState(rom.ID, StateLedgerEntry{
			MD5: sum, Size: int64(len(raw)), ServerID: up.ID, Slot: sf.Slot, Origin: tuple, Own: true,
		})
		res.Pushed++
	}
	if res.Pushed > 0 {
		res.Retired = retireOwnOldStates(client, rom.ID, cfg.ResolvedStateRetain())
	}
	return res
}

// retireOwnOldStates deletes this engine's own oldest server uploads beyond
// the newest-`retain` per slot (retain = the user's state_retain knob, default
// 5 via ResolvedStateRetain). Victims come strictly from ledger entries marked
// Own with a live ServerID (invariant 7.9 — no deletion ever propagates to
// another device's records), cross-checked against a fresh server listing: an
// ID already gone server-side just heals the ledger; an ID whose server record
// isn't lodor-origin is a contested identity and is never deleted. Any server
// error skips the victim — retried on the next landed push.
//
// #9: "oldest" is ordered by the monotonic upload Seq (stable across MD5
// refresh), NOT RecordedAt. A pull that refreshes an existing MD5 restamps
// RecordedAt to "now", so the newest bytes could look oldest by clock time and
// be selected as the victim — deleting the freshest state. Seq is assigned once
// at first record and never moves, so it reflects true upload order. Legacy
// entries with Seq==0 (pre-#9 ledgers) fall back to RecordedAt as a tiebreak.
func retireOwnOldStates(client *romm.Client, romID, retain int) int {
	if retain < 1 {
		retain = 5 // belt-and-braces: retention trims history, never erases it
	}
	bySlot := map[string][]StateLedgerEntry{}
	over := false
	for _, e := range StateLedgerEntries(romID) {
		if !e.Own || e.ServerID == 0 {
			continue
		}
		bySlot[e.Slot] = append(bySlot[e.Slot], e)
		if len(bySlot[e.Slot]) > retain {
			over = true
		}
	}
	if !over {
		return 0
	}
	states, err := client.GetStates(romID)
	if err != nil {
		return 0
	}
	onServer := map[int]string{} // id → emulator field
	for _, s := range states {
		onServer[s.ID] = s.Emulator
	}
	deleted := 0
	for _, list := range bySlot {
		if len(list) <= retain {
			continue
		}
		// Newest-first by stable upload order (#9). Prefer Seq; both-legacy
		// (Seq==0) entries tiebreak on RecordedAt so pre-#9 ledgers still order.
		sort.Slice(list, func(i, j int) bool {
			if list[i].Seq != list[j].Seq {
				return list[i].Seq > list[j].Seq
			}
			return list[i].RecordedAt.After(list[j].RecordedAt)
		})
		for _, e := range list[retain:] {
			emu, exists := onServer[e.ServerID]
			if !exists {
				_ = ForgetStateServerID(romID, e.ServerID)
				continue
			}
			if !strings.HasPrefix(emu, "lodor/") {
				continue
			}
			if client.DeleteStates([]int{e.ServerID}) != nil {
				continue
			}
			_ = ForgetStateServerID(romID, e.ServerID)
			deleted++
		}
	}
	return deleted
}

// stateUploadName renders the D6 server filename:
// "<rom-stem> [UTC ts] (lodor s<slot> <device8>).state" — convention-compatible
// with Grout's timeline display, machine-parseable as belt-and-braces only
// (authoritative metadata = emulator field + ledger).
func stateUploadName(romBase, slot, device string) string {
	stem := strings.TrimSuffix(romBase, filepath.Ext(romBase))
	ts := time.Now().UTC().Format("2006-01-02_15-04-05")
	return fmt.Sprintf("%s [%s] (lodor s%s %s).state", stem, ts, slot, device)
}

func bytesMD5(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

// deviceShort is the first 8 chars of this device's id ("nodev" when unpaired —
// the upload still works; attribution is best-effort).
func deviceShort(cfg *config.Config) string {
	id := deviceID(cfg)
	if id == "" {
		return "nodev"
	}
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// PushAllLocalStatesResult aggregates a bulk push across the whole mirrored
// library (the manual "Sync Now" state path). RomsWithStates counts ROMs that
// had at least one local state file (i.e. the ones we actually hit the network
// for); the per-ROM counters sum the underlying PushStates results. Queued
// counts ROMs whose per-ROM push landed offline and were auto-queued into
// pending-states.txt by the cmd layer (statesync itself never writes the queue).
type PushAllLocalStatesResult struct {
	Pushed          int
	Skipped         int
	Failed          int
	Retired         int
	Queued          int // eligible ROMs that landed offline (cmd layer queues them)
	RomsWithStates  int
	Reason          string // ok | no-manifest
	AuthExpired     bool   // any per-ROM push hit a rejected token (PAIRING_EXPIRED)
}

// sdcardRootForStates mirrors catalog's sdcardRoot / the cmd sdRoot(): the
// SDCARD_PATH env, defaulting to /mnt/SDCARD. Used to turn the catalog index's
// SDCARD-relative paths back into the absolute paths PushStates keys on.
func sdcardRootForStates() string {
	if sd := os.Getenv("SDCARD_PATH"); sd != "" {
		return sd
	}
	return "/mnt/SDCARD"
}

// PushAllLocalStates pushes EVERY mirrored ROM's local save states in one call —
// the bulk backing for a manual "Sync Now" (per-ROM --push-states covers the
// on-exit hooks). It is LOCAL-FIRST by construction so a library of thousands of
// ROMs with states on only a handful never hammers the slow radio:
//
//	manifest (loadStateCores) ── absent → no-op honestly (reason=no-manifest)
//	catalog index BY SLUG (catalog.LoadIndexBySlug) ── the platform fs_slug is
//	    the index's own top-level key, so a ROM's platform (needed for the
//	    statecores.json lookup) is derived WITHOUT any network resolve. This is
//	    the design choice that keeps the pre-filter truly local: PushStates gets
//	    the platform by resolving the ROM against RomM, but here we already know
//	    it from the on-disk index grouping.
//	per ROM: manifest lookup → StateDirFor → StateFilesForRom (all on-disk) ──
//	    ZERO local state files → SKIPPED with no network call (the common case).
//	only ROMs that DO have local state files fall through to the existing
//	    sync.PushStates (statesync.go), which resolves, normalizes, dedups vs the
//	    ledger, uploads, auto-retains, and — offline — signals reason=offline so
//	    the cmd layer queues the ROM.
//
// Best-effort and additive: individual ROM failures never abort the sweep.
// report (may be nil) is invoked once per ROM that had local state files, with
// its per-ROM PushStates result — the cmd layer uses it to print per-ROM lines
// and to enqueue offline ROMs into pending-states.txt (the same split as
// runPushStates: statesync uploads, the cmd owns the queue file).
func PushAllLocalStates(client *romm.Client, cfg *config.Config,
	report func(romPath string, res PushStatesResult)) PushAllLocalStatesResult {
	sc, ok := loadStateCores()
	if !ok {
		return PushAllLocalStatesResult{Reason: "no-manifest"}
	}
	bySlug := catalog.LoadIndexBySlug(cfg)
	sd := sdcardRootForStates()

	res := PushAllLocalStatesResult{Reason: "ok"}
	for slug, byID := range bySlug {
		// Platform → manifest system is a PURE LOCAL lookup (index key = fs_slug).
		// A platform absent from the manifest (unsupported on this host, or no
		// pinned core) can never have pushable states here — skip the whole group
		// without touching disk or network.
		info, oks := sc.Systems[slug]
		if !oks {
			continue
		}
		dir := platform.StateDirFor(info.Dir)
		if dir == "" {
			continue // states unsupported on this host for this system
		}
		// Deterministic order (map iteration is random) so progress/output is
		// stable and tests can assert reliably.
		ids := make([]int, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			rel := byID[id]
			if rel == "" {
				continue
			}
			// LOCAL PRE-FILTER: does this ROM have any state files on disk? This
			// is the gate that MUST precede any per-ROM network call — checked by
			// walking the manifest's on-disk state dir with the ROM's basename.
			romBase := filepath.Base(rel)
			if len(platform.StateFilesForRom(dir, romBase)) == 0 {
				continue // no local states → never a network call (the common case)
			}
			res.RomsWithStates++
			// Reuse the full per-ROM push (resolve+normalize+dedup+upload+retain).
			romPath := filepath.Join(sd, rel)
			pr := PushStates(client, cfg, romPath)
			if report != nil {
				report(romPath, pr)
			}
			res.Pushed += pr.Pushed
			res.Skipped += pr.Skipped
			res.Failed += pr.Failed
			res.Retired += pr.Retired
			if pr.AuthExpired {
				res.AuthExpired = true // sticky: one dead-pairing taints the sweep
			}
			if pr.Reason == "offline" {
				res.Queued++
			}
		}
	}
	return res
}
