package sync

// Auto-state retirement after a landed restore-save (Android agent's fix, moved
// into the engine so all lanes inherit it — 2026-07-08).
//
// THE TRAP: a user restores a NEWER battery save (--restore-save), but the
// frontend's auto-load-state (RA's "<stem>.state.auto", minarch's ".st9") is a
// snapshot of the OLD session. On the next launch the frontend silently
// auto-loads that stale state, MASKING the freshly-restored save — the user's
// restore appears to have done nothing. muOS/Knulli/minarch/RA all share it.
//
// THE FIX: after a restore lands, push any local states (so the auto-state's
// content is preserved on the server first), and ONLY if that push is clean
// (reason ok|no-states) retire the auto-resume file by RENAMING it to
// "<auto>.pre-sync" — never deleting it, never touching manual numbered slots.
// The frontend no longer finds an auto-state to load, so the restored save wins;
// the retired snapshot is preserved on disk (and on the server) if ever wanted.
//
// FAIL-SAFE: if states did NOT push cleanly (offline / failure), we do NOT
// retire — losing an unpushed auto-state is worse than the mask. Gated on the
// statecores manifest (the engine can only locate the auto-state dir when
// statesync is live); dark lanes keep the pre-existing behavior.

import (
	"os"
	"path/filepath"
	"strconv"
	"time"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// RetireAutoStateAfterRestore runs the fix. Returns whether an auto-state was
// retired and a short reason for the RESULT line:
//
//	retired          — auto-state renamed to <auto>.pre-sync
//	no-auto-state    — nothing to retire (no auto-state on disk)
//	no-manifest      — statesync dark here (no statecores.json)
//	no-system        — rom's system not in the manifest
//	resolve          — could not resolve the rom
//	<push reason>     — states did not push cleanly; retire SKIPPED (fail-safe)
//	rename-failed     — the rename itself failed
func RetireAutoStateAfterRestore(client *romm.Client, cfg *config.Config, romPath string) (bool, string) {
	// 1. Preserve first: push local states. Only a clean push (states are safely
	// on the server, or there are none) lets us retire the local auto-state.
	push := PushStates(client, cfg, romPath)
	if push.Reason != "ok" && push.Reason != "no-states" {
		return false, push.Reason
	}

	// 2. Locate the auto-state via the same manifest push-states used.
	sc, ok := loadStateCores()
	if !ok {
		return false, "no-manifest"
	}
	rom, _, _, okr := resolveRomAndLocalSavePath(client, cfg, romPath, "")
	if !okr {
		return false, "resolve"
	}
	info, oks := sc.Systems[rom.PlatformFsSlug]
	if !oks {
		return false, "no-system"
	}
	dir := platform.StateDirFor(info.Dir)
	if dir == "" {
		return false, "no-system"
	}
	auto := platform.StateFileForSlot(dir, filepath.Base(romPath), "auto")
	if auto == "" {
		return false, "no-auto-state"
	}
	if fi, err := os.Stat(auto); err != nil || fi.IsDir() {
		return false, "no-auto-state"
	}

	// 3. Retire by rename — NEVER delete, and manual slots (.state1/.st0..8) are
	// untouched because we only ever name the "auto" slot.
	//
	// #24: version the retired name so a SECOND restore doesn't clobber the FIRST
	// restore's retired snapshot. os.Rename(auto, auto+".pre-sync") atomically
	// REPLACES an existing ".pre-sync" — the header promises retired snapshots are
	// "never deleted", but the earlier one was being destroyed on every re-restore.
	// The first retirement keeps the plain "<auto>.pre-sync" name (compat with any
	// tooling that looks for it); later ones get a timestamp-versioned suffix.
	retired := nonCollidingRetiredPath(auto)
	if err := os.Rename(auto, retired); err != nil {
		return false, "rename-failed"
	}
	return true, "retired"
}

// nonCollidingRetiredPath returns a retired auto-state path that never clobbers
// an existing one (#24). The first retirement takes the plain "<auto>.pre-sync";
// each later one takes "<auto>.pre-sync.<UTC-timestamp>" (nanosecond precision),
// with a bounded numeric disambiguator if two restores land in the same instant,
// so every prior retired snapshot survives.
func nonCollidingRetiredPath(auto string) string {
	base := auto + ".pre-sync"
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}
	stamp := base + "." + time.Now().UTC().Format("20060102T150405.000000000")
	if _, err := os.Stat(stamp); os.IsNotExist(err) {
		return stamp
	}
	for i := 1; i < 10000; i++ {
		cand := stamp + "." + strconv.Itoa(i)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return stamp
}
