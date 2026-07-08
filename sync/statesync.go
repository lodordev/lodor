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
			return PushStatesResult{Reason: "offline"}
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
		sort.Slice(list, func(i, j int) bool { return list[i].RecordedAt.After(list[j].RecordedAt) })
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
