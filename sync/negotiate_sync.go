package sync

import (
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// Server-side negotiated whole-library sync (research #3) — the modern strategy for
// RomM servers that report SupportsSyncNegotiate (>= 4.9.0). The device sends its
// per-rom save fingerprints, the server returns a plan (operations[]), the device
// executes it and completes the session. The per-rom anchor reconciler
// (SyncSaveReconciled) remains the fallback for older servers.
//
// WIRE SHAPE UNVERIFIED against a live >= 4.9.0 server (see romm/negotiate.go). This
// path must be validated end-to-end before it ships; until then the engine defaults to
// the legacy strategy.

// NegotiateOpResult records what happened for one operation in a negotiated plan.
type NegotiateOpResult struct {
	RomID  int
	Op     string
	OK     bool
	Reason string // host-free
}

// NegotiateSummary aggregates a negotiated sync run for the cmd RESULT line.
type NegotiateSummary struct {
	SessionID int
	Pulled    int
	Pushed    int
	Conflicts int
	Noops     int
	Errors    int
	Ops       []NegotiateOpResult
	Completed bool
}

// BuildNegotiateRequest assembles the device's per-rom save fingerprints across the
// mapped platforms: for every rom that has a local save on the card, one
// NegotiateSaveRef with the MD5 of the primary local save. deviceID keys the request.
func BuildNegotiateRequest(client *romm.Client, cfg *config.Config, deviceID string) (romm.NegotiateRequest, error) {
	req := romm.NegotiateRequest{DeviceID: deviceID}
	platforms, err := mappedPlatformsForSync(client, cfg)
	if err != nil {
		return req, err
	}
	for _, p := range platforms {
		roms, err := client.GetRoms(romm.GetRomsQuery{PlatformIDs: []int{p.ID}})
		if err != nil {
			continue
		}
		for _, rom := range roms.Items {
			saves := findLocalSavesForRom(cfg, rom)
			if len(saves) == 0 {
				continue
			}
			sum, ok := fileMD5(saves[0].path)
			if !ok {
				continue
			}
			req.Saves = append(req.Saves, romm.NegotiateSaveRef{
				RomID:    rom.ID,
				Hash:     sum,
				Emulator: saves[0].emuDir,
				Slot:     "autosave",
			})
		}
	}
	return req, nil
}

// NegotiateLibrarySync runs the full negotiated sync: build the request, ask the server
// for a plan, execute every operation, and complete the session. A failure to reach
// the negotiate endpoint is returned as an error so the caller can fall back to the
// legacy strategy.
func NegotiateLibrarySync(client *romm.Client, cfg *config.Config, deviceID string) (NegotiateSummary, error) {
	var sum NegotiateSummary
	req, err := BuildNegotiateRequest(client, cfg, deviceID)
	if err != nil {
		return sum, err
	}
	plan, err := client.Negotiate(req)
	if err != nil {
		return sum, err
	}
	sum.SessionID = plan.SessionID
	sum = ExecuteNegotiation(client, cfg, plan, sum)

	// Complete the session even when some ops errored — the server tracks which
	// operations the device acknowledged. A complete failure is recorded, not fatal.
	if plan.SessionID != 0 {
		if cerr := client.CompleteSyncSession(plan.SessionID); cerr == nil {
			sum.Completed = true
		}
	}
	return sum, nil
}

// ExecuteNegotiation performs each operation in a server plan, updating anchors so the
// anchor store stays consistent across strategies. It mutates and returns sum.
func ExecuteNegotiation(client *romm.Client, cfg *config.Config, plan romm.NegotiateResponse, sum NegotiateSummary) NegotiateSummary {
	for _, op := range plan.Operations {
		switch op.Op {
		case romm.SyncOpNoOp:
			sum.Noops++
			sum.Ops = append(sum.Ops, NegotiateOpResult{RomID: op.RomID, Op: op.Op, OK: true})

		case romm.SyncOpConflict:
			// Never auto-resolve — record and leave both copies untouched.
			sum.Conflicts++
			sum.Ops = append(sum.Ops, NegotiateOpResult{RomID: op.RomID, Op: op.Op, OK: true, Reason: op.Reason})

		case romm.SyncOpDownload:
			ok, reason := negotiatePull(client, cfg, op)
			if ok {
				sum.Pulled++
			} else {
				sum.Errors++
			}
			sum.Ops = append(sum.Ops, NegotiateOpResult{RomID: op.RomID, Op: op.Op, OK: ok, Reason: reason})

		case romm.SyncOpUpload:
			ok, reason := negotiatePush(client, cfg, op)
			if ok {
				sum.Pushed++
			} else {
				sum.Errors++
			}
			sum.Ops = append(sum.Ops, NegotiateOpResult{RomID: op.RomID, Op: op.Op, OK: ok, Reason: reason})

		default:
			sum.Errors++
			sum.Ops = append(sum.Ops, NegotiateOpResult{RomID: op.RomID, Op: op.Op, OK: false, Reason: "unknown op"})
		}
	}
	return sum
}

// negotiatePull resolves the rom, finds the named server save, writes it to the card,
// and records the anchor. The save id comes from the plan op.
func negotiatePull(client *romm.Client, cfg *config.Config, op romm.SyncOp) (bool, string) {
	rom, err := client.GetRom(op.RomID)
	if err != nil || rom.ID == 0 {
		return false, "rom not found"
	}
	romPath := platform.LocalRomPath(cfg, rom)
	if romPath == "" {
		return false, "no local path"
	}
	saves, err := client.GetSaves(romm.SaveQuery{RomID: rom.ID})
	if err != nil {
		return false, "couldn't reach server"
	}
	var chosen romm.Save
	found := false
	for _, s := range saves {
		if s.ID == op.SaveID {
			chosen = s
			found = true
			break
		}
	}
	if !found {
		// The plan named no save id (or it vanished) — fall back to the newest.
		if len(saves) == 0 {
			return false, "no server save"
		}
		chosen = newestSave(saves)
	}
	pr := RestoreSave(client, cfg, romPath, chosen)
	if pr.Outcome != PullWritten {
		return false, pr.Reason
	}
	h := ""
	if chosen.ContentHash != nil {
		h = *chosen.ContentHash
	}
	_ = SaveAnchor(rom.ID, Anchor{Hash: h, ServerSaveID: chosen.ID, ServerUpdatedAt: chosen.UpdatedAt})
	return true, ""
}

// negotiatePush resolves the rom, uploads its local save, and records the anchor.
func negotiatePush(client *romm.Client, cfg *config.Config, op romm.SyncOp) (bool, string) {
	rom, err := client.GetRom(op.RomID)
	if err != nil || rom.ID == 0 {
		return false, "rom not found"
	}
	romPath := platform.LocalRomPath(cfg, rom)
	if romPath == "" {
		return false, "no local path"
	}
	pushes := PushSaveDirect(client, cfg, romPath)
	if !allLanded(pushes) {
		return false, firstStuckReason(pushes)
	}
	if saves := findLocalSavesForRom(cfg, rom); len(saves) > 0 {
		if sum, ok := fileMD5(saves[0].path); ok {
			_ = SaveAnchor(rom.ID, Anchor{Hash: sum})
		}
	}
	return true, ""
}

// mappedPlatformsForSync returns the RomM platforms the user has a directory mapping
// for (the sync-package analog of cmd's mappedPlatforms — kept here so the sync layer
// owns no cmd dependency).
func mappedPlatformsForSync(client *romm.Client, cfg *config.Config) ([]romm.Platform, error) {
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
