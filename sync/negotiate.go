package sync

import (
	"fmt"

	"lodor/catalog"
	"lodor/config"
	"lodor/romm"
)

// ReconcileResult is the outcome of one managed-sync reconcile pass (the whole-library
// background job, decision 2026-07-16). Counts are per OPERATION, not per ROM, so they
// line up with the session's operations_planned tally.
//
//   - Completed — upload/download ops that succeeded (bytes moved or already in sync).
//   - Failed    — ops that errored (upload/download/reachability failure).
//   - Conflicts — ops the server flagged 'conflict', PLUS downloads we refused because
//     the local save is unpushed progress. These are DEFERRED, never auto-resolved:
//     the launch card is the selection surface (decision 2026-07-16). Not reported to
//     the server as completed or failed — a partial completion is tolerated.
//   - Skipped   — ops for a ROM not present in this device's mirror (nothing to act on
//     locally) or whose local save vanished between inventory and execution.
type ReconcileResult struct {
	SessionID int
	Planned   int // server total_upload+total_download+total_conflict
	Completed int
	Failed    int
	Conflicts int
	Skipped   int
	Notes     []string // short host-free lines for the runlog / --reconcile output
}

// romOps buckets one ROM's operations by action while preserving the operation COUNT in
// each bucket (a ROM can carry several upload ops — e.g. coexist twins — and the
// per-rom push acts on all of them at once, so the verdict applies to the whole count).
type romOps struct {
	uploads   int
	downloads int
	conflicts int
}

// bucketOps groups a negotiated plan's operations by rom_id, preserving first-sighting
// order for stable output, and returns the per-ROM action counts plus the planned total
// (upload+download+conflict — no_op is excluded exactly as the server excludes it from
// operations_planned). Unknown actions from a newer server are ignored, not miscounted.
// Pure — the network-free half of ExecuteNegotiatedPlan, unit-tested directly.
func bucketOps(ops []romm.SyncOperation) (order []int, byRom map[int]*romOps, planned int) {
	byRom = make(map[int]*romOps)
	for _, op := range ops {
		switch op.Action {
		case "upload", "download", "conflict":
			planned++
		default:
			continue // no_op or an unknown future action — nothing to plan
		}
		b, ok := byRom[op.RomID]
		if !ok {
			b = &romOps{}
			byRom[op.RomID] = b
			order = append(order, op.RomID)
		}
		switch op.Action {
		case "upload":
			b.uploads++
		case "download":
			b.downloads++
		case "conflict":
			b.conflicts++
		}
	}
	return order, byRom, planned
}

// ExecuteNegotiatedPlan runs a negotiated operation plan against the verified push/pull
// primitives and returns per-operation counts. It groups ops by rom_id so the
// whole-ROM PushSaveDirect (pushes every local save for the ROM) and PullSaveDirect
// (pulls the newest, lose-proof) each run at most ONCE per ROM regardless of how many
// ops that ROM carries — then applies the single verdict to every op in the matching
// bucket.
//
// rom_id -> local ROM path is resolved from the catalog index (the reverse of the
// path -> rom_id resolution the primitives do internally). An op for a ROM not in the
// mirror is Skipped: there is no local file to push or pull.
//
// A token that dies mid-run (AuthExpired) aborts the pass immediately with an error —
// every remaining op would fail identically, and the caller must re-pair before
// retrying rather than burn the session.
func ExecuteNegotiatedPlan(client *romm.Client, cfg *config.Config, ops []romm.SyncOperation) (ReconcileResult, error) {
	order, byRom, planned := bucketOps(ops)
	res := ReconcileResult{Planned: planned}
	idPath := catalog.LoadIndexIDPath(cfg)

	for _, romID := range order {
		b := byRom[romID]
		res.Conflicts += b.conflicts // conflicts are deferred to the launch card

		romPath := idPath[romID]
		if romPath == "" {
			res.Skipped += b.uploads + b.downloads
			continue
		}

		if b.uploads > 0 {
			ok, skip, authExpired := applyPush(PushSaveDirect(client, cfg, romPath))
			if authExpired {
				return res, fmt.Errorf("sync aborted: pairing expired mid-reconcile (rom %d)", romID)
			}
			switch {
			case skip:
				res.Skipped += b.uploads
			case ok:
				res.Completed += b.uploads
			default:
				res.Failed += b.uploads
				res.Notes = append(res.Notes, fmt.Sprintf("upload failed rom %d", romID))
			}
		}

		if b.downloads > 0 {
			verdict := PullSaveDirect(client, cfg, romPath)
			if verdict.AuthExpired {
				return res, fmt.Errorf("sync aborted: pairing expired mid-reconcile (rom %d)", romID)
			}
			switch verdict.Outcome {
			case PullWritten, PullInSync:
				res.Completed += b.downloads
			case PullLocalUnpushed:
				// Server planned a download but the local bytes are unpushed progress —
				// a real divergence. Defer to the launch card, don't overwrite.
				res.Conflicts += b.downloads
			case PullNoServerSave:
				res.Skipped += b.downloads
			default:
				res.Failed += b.downloads
				res.Notes = append(res.Notes, fmt.Sprintf("download failed rom %d: %s", romID, verdict.Reason))
			}
		}
	}

	return res, nil
}

// applyPush reduces a per-save PushResult slice to a single ROM-level verdict: ok when
// any save landed or was already on the server; skip when the only outcomes are "no
// local save"/"empty" (the file vanished or is a stub — nothing to move, not a
// failure); fail otherwise. authExpired surfaces a dead token to abort the pass.
func applyPush(results []PushResult) (ok, skip, authExpired bool) {
	sawActionable := false
	for _, r := range results {
		if r.AuthExpired {
			authExpired = true
		}
		switch r.Outcome {
		case OutcomePushed, OutcomeAlreadyOnServer:
			ok = true
			sawActionable = true
		case OutcomeNoLocalSave, OutcomeEmptyLocalSave:
			// nothing to move — not a failure
		default:
			sawActionable = true
		}
	}
	if ok {
		return true, false, authExpired
	}
	// No success. If we never saw an actionable save (only "no local save"), it's a
	// skip, not a failure.
	return false, !sawActionable, authExpired
}

// NegotiatePlan builds the local save inventory and opens a sync session (POST
// /api/sync/negotiate) WITHOUT executing or completing it — the read/plan-only half of
// a reconcile. Returns the server's plan plus the number of local saves sent, so a
// dry run can show exactly what a full pass would do (and the caller can close the
// opened session with a 0/0 complete). Gated on SupportsManagedSync.
func NegotiatePlan(client *romm.Client, cfg *config.Config) (romm.SyncNegotiateResponse, int, error) {
	if !client.SupportsManagedSync() {
		return romm.SyncNegotiateResponse{}, 0, fmt.Errorf("managed sync unsupported: RomM %s < 5.0.0", client.ServerVersion())
	}
	inv := BuildLocalSaveInventory(cfg)
	resp, err := client.Negotiate(deviceID(cfg), inv)
	if err != nil {
		return romm.SyncNegotiateResponse{}, len(inv), fmt.Errorf("negotiate: %w", err)
	}
	return resp, len(inv), nil
}

// ReconcileLibrary runs one full managed-sync pass: build the local save inventory,
// negotiate a session, execute the plan, and report the executed counts back to close
// the session. This is the on-power background reconcile (decision 2026-07-16),
// replacing Lodor's raw per-game POST /api/saves background loop. Saves only — states
// stay on their own path.
//
// Gated on SupportsManagedSync (RomM >= 5.0.0); on an older server it returns a nil
// result and a non-nil error so the caller can fall back. The session is ALWAYS
// completed when negotiate succeeded (even on an execution error) so a half-run session
// never dangles server-side.
func ReconcileLibrary(client *romm.Client, cfg *config.Config) (ReconcileResult, error) {
	resp, _, err := NegotiatePlan(client, cfg)
	if err != nil {
		return ReconcileResult{}, err
	}

	res, execErr := ExecuteNegotiatedPlan(client, cfg, resp.Operations)
	res.SessionID = resp.SessionID

	// Close the session regardless of execErr — negotiate already opened it, so leaving
	// it open would strand a session row. Report the counts we actually achieved.
	if _, cerr := client.CompleteSyncSession(resp.SessionID, res.Completed, res.Failed); cerr != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("session complete failed: %s", firstLine(cerr.Error())))
	}
	return res, execErr
}
