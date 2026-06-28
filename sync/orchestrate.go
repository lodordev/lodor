package sync

import (
	"lodor/config"
	"lodor/romm"
)

// ReconcileOpts carries the local-authority signals the per-rom reconciler can't infer
// from hashes alone: whether the rom is queued in pending-saves.txt and whether the
// user explicitly chose a restore. Both force KEEP_LOCAL (see Reconcile).
type ReconcileOpts struct {
	PendingUpload   bool
	ExplicitRestore bool
}

// ReconcileResult is the rich outcome of a reconciled single-rom sync. The cmd layer
// maps it onto the RESULT contract; Conflict is the never-clobber surface.
type ReconcileResult struct {
	Decision     Decision
	Pulled       bool
	Pushed       bool
	Conflict     bool
	HashCompared bool   // false => fell back to newest-wins (server exposed no content_hash)
	Resolved     bool   // rom mapped to a RomM game
	Reachable    bool   // server save list was fetched
	Reason       string // short, host-free
	LocalPath    string
}

// SyncSaveReconciled is the anchor-based 3-way replacement for the newest-wins pull in
// runSyncSave (research #1 — the save-sync MOAT). For ONE rom it gathers the local save
// hash(es), the server save history + newest, and the stored anchor (bootstrapping one
// from the server history when none is recorded), runs the pure Reconcile decision, and
// executes it: PULL the newest server save (KEEP_SERVER), PUSH the local save
// (KEEP_LOCAL), refresh the anchor (IN_SYNC), or surface a CONFLICT and touch nothing.
//
// REGRESSION SAFETY: when the server exposes no per-save content_hash (older RomM, where
// a hash 3-way is impossible), it falls back to the EXACT current behavior —
// PullSaveDirect (newest-wins) then PushSaveDirect — so older servers behave byte-for-
// byte as before and only modern, hash-bearing servers get the safe reconcile.
//
// This path is NOT yet validated against a live round-trip (deferred). It must be
// reviewed and round-trip tested before it ships.
func SyncSaveReconciled(client *romm.Client, cfg *config.Config, romPath string, opts ReconcileOpts) ReconcileResult {
	var res ReconcileResult

	rom, _, ok := resolveRomAndLocalSavePath(client, cfg, romPath, "")
	if !ok {
		res.Reason = "not matched to a RomM game"
		return res
	}
	res.Resolved = true

	saves, err := client.GetSaves(romm.SaveQuery{RomID: rom.ID})
	if err != nil {
		res.Reason = "couldn't reach server"
		return res
	}
	res.Reachable = true

	// Local save fingerprints (MD5 of the raw bytes — the same signal RomM stores as a
	// save's content_hash).
	var localHashes []string
	for _, sf := range findLocalSavesForRom(cfg, rom) {
		if sum, ok := fileMD5(sf.path); ok {
			localHashes = append(localHashes, sum)
		}
	}
	primaryLocal := ""
	if len(localHashes) > 0 {
		primaryLocal = localHashes[0]
	}

	// Server save history + newest.
	hasServer := len(saves) > 0
	var newest romm.Save
	serverHash := ""
	var serverHistory []string
	if hasServer {
		newest = newestSave(saves)
		for _, s := range saves {
			if s.ContentHash != nil && *s.ContentHash != "" {
				serverHistory = append(serverHistory, *s.ContentHash)
			}
		}
		if newest.ContentHash != nil {
			serverHash = *newest.ContentHash
		}
	}

	// REGRESSION-SAFE FALLBACK: a server that has saves but exposes no content_hash on
	// the newest one cannot be hash-reconciled. Preserve the deployed newest-wins
	// behavior exactly (PullSaveDirect then PushSaveDirect) rather than guess.
	if hasServer && serverHash == "" && !opts.ExplicitRestore {
		return legacyNewestWins(client, cfg, romPath, res)
	}
	res.HashCompared = true

	// Effective local hash: if ANY local file matches the server's newest content, the
	// two sides are in sync even when a different local file is "primary" (multi-file
	// rom). Otherwise the primary local hash represents the device.
	effLocal := primaryLocal
	if containsFold(localHashes, serverHash) {
		effLocal = serverHash
	}

	// Anchor: stored, else bootstrapped from the server history (promote the existing
	// already-on-server signal into the anchor model).
	anchorHash := ""
	if a, ok := LoadAnchor(rom.ID); ok {
		anchorHash = a.Hash
	}
	if anchorHash == "" {
		anchorHash = BootstrapAnchorHash(primaryLocal, serverHistory)
	}

	res.Decision = Reconcile(ReconcileInput{
		LocalHash:       effLocal,
		ServerHash:      serverHash,
		AnchorHash:      anchorHash,
		PendingUpload:   opts.PendingUpload,
		ExplicitRestore: opts.ExplicitRestore,
	})

	switch res.Decision {
	case DecisionInSync:
		h := serverHash
		if h == "" {
			h = primaryLocal
		}
		if h != "" {
			_ = SaveAnchor(rom.ID, Anchor{Hash: h, ServerSaveID: newest.ID, ServerUpdatedAt: newest.UpdatedAt})
		}

	case DecisionKeepServer:
		pr := RestoreSave(client, cfg, romPath, newest)
		if pr.Outcome == PullWritten {
			res.Pulled = true
			res.LocalPath = pr.LocalPath
			_ = SaveAnchor(rom.ID, Anchor{Hash: serverHash, ServerSaveID: newest.ID, ServerUpdatedAt: newest.UpdatedAt})
		} else {
			res.Reason = pr.Reason
		}

	case DecisionKeepLocal:
		pushes := PushSaveDirect(client, cfg, romPath)
		if allLanded(pushes) {
			res.Pushed = true
			if primaryLocal != "" {
				// The local bytes are now also on the server → they are the new anchor.
				_ = SaveAnchor(rom.ID, Anchor{Hash: primaryLocal})
			}
		} else {
			res.Reason = firstStuckReason(pushes)
		}

	case DecisionConflict:
		res.Conflict = true
	}

	return res
}

// legacyNewestWins reproduces the pre-anchor behavior for servers without per-save
// content_hash: pull the newest server save by mtime (PullSaveDirect), then push the
// local save (PushSaveDirect). It updates res in place and returns it.
func legacyNewestWins(client *romm.Client, cfg *config.Config, romPath string, res ReconcileResult) ReconcileResult {
	res.HashCompared = false
	pull := PullSaveDirect(client, cfg, romPath)
	if pull.Outcome == PullWritten {
		res.Pulled = true
		res.LocalPath = pull.LocalPath
	}
	pushes := PushSaveDirect(client, cfg, romPath)
	if allLanded(pushes) {
		res.Pushed = true
	}
	return res
}

// allLanded reports whether every push result is safely on the server.
func allLanded(results []PushResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.Outcome != OutcomePushed && r.Outcome != OutcomeAlreadyOnServer {
			return false
		}
	}
	return true
}

// firstStuckReason returns a short host-free reason from the first non-landed push.
func firstStuckReason(results []PushResult) string {
	for _, r := range results {
		switch r.Outcome {
		case OutcomeResolveFail:
			return "game no longer on server"
		case OutcomeNoLocalSave:
			return "no save file on the card"
		case OutcomeUploadError:
			return "upload failed"
		}
	}
	return ""
}
