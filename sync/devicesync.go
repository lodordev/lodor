package sync

// device_save_sync integration at the sync layer (task #176). Two responsibilities:
//
//   1. confirmDownloaded — the download-side ledger write. Our GET /content runs
//      optimistic=false (the server won't advance the sync row until the bytes are
//      safely on the card), so AFTER a verified local write we confirm the download
//      with POST /api/saves/{id}/downloaded. BEST-EFFORT: it never affects the pull
//      outcome — a failed confirm just means the server's "which device is current"
//      column lags until the next sync; the bytes are already safe locally.
//
//   2. TrackSaveForRom / UntrackSaveForRom — the explicit "resume / stop syncing this
//      save on this device" user control, resolving a local ROM path to its target
//      server save and calling the endpoint. Reported honestly to the user (this is a
//      user-invoked action, not silent), but still NON-BLOCKING: it never touches save
//      bytes and never fails a launch.
//
// EVERYTHING here is gated on RomM >= 4.9.0 (client.SupportsDeviceSaveSync). The
// content-hash 3-way reconcile remains the SOLE conflict authority; these calls move
// no save data.

import (
	"fmt"
	"os"
	"strings"

	"lodor/catalog"
	"lodor/config"
	"lodor/romm"
)

// DeviceSyncResult is the outcome of an explicit track/untrack user action.
type DeviceSyncResult struct {
	OK          bool   // the ledger endpoint call succeeded
	SaveID      int    // the resolved target save id (0 when none resolved)
	Reason      string // machine token: ok|unsupported|nodevice|resolve|nosave|notracked|autherr|error
	AuthExpired bool   // the token was rejected (re-pair) — surfaces PAIRING_EXPIRED
}

// confirmDownloaded records that this device now holds save saveID, best-effort. Called
// by writeSave after a verified local write. Silent on the happy path; a failure is
// logged host-free to stderr and swallowed — the pull already succeeded. Gated on the
// server version and on a configured device.
func confirmDownloaded(client *romm.Client, cfg *config.Config, saveID int) {
	if saveID == 0 {
		return
	}
	dev := deviceID(cfg)
	if dev == "" {
		return // no registered device — nothing to attribute
	}
	if !client.SupportsDeviceSaveSync() {
		return // server too old (< 4.9.0) — the endpoint isn't there
	}
	if _, err := client.MarkSaveDownloaded(saveID, dev); err != nil {
		// Best-effort: the bytes are already written; a lagging ledger row is cosmetic.
		fmt.Fprintf(os.Stderr, "device-sync: WARN downloaded-confirm save=%d failed (ignored): %s\n", saveID, cleanErr(err))
	}
}

// TrackSaveForRom resumes syncing this device's save for romPath (is_untracked=false),
// via POST /api/saves/{id}/track on the newest real (non-ghost, non-meta) server save.
// ASYMMETRY: track only updates an EXISTING row — if this device never had a sync row
// for the save the server 404s and we report reason=notracked (there is nothing to
// resume; untrack-then-track, or a plain sync, would create one first).
func TrackSaveForRom(client *romm.Client, cfg *config.Config, romPath string) DeviceSyncResult {
	return runDeviceSyncAction(client, cfg, romPath, "track")
}

// UntrackSaveForRom stops syncing this device's save for romPath (is_untracked=true),
// via POST /api/saves/{id}/untrack on the newest real server save. Unlike track,
// untrack CREATES the row if absent, so it always takes effect on a real save.
func UntrackSaveForRom(client *romm.Client, cfg *config.Config, romPath string) DeviceSyncResult {
	return runDeviceSyncAction(client, cfg, romPath, "untrack")
}

// runDeviceSyncAction is the shared resolve-then-call body for track/untrack. It never
// panics and never touches save bytes; on any failure it returns a typed reason so the
// cmd layer can print an honest RESULT line.
func runDeviceSyncAction(client *romm.Client, cfg *config.Config, romPath, action string) DeviceSyncResult {
	if !client.SupportsDeviceSaveSync() {
		return DeviceSyncResult{Reason: "unsupported"}
	}
	dev := deviceID(cfg)
	if dev == "" {
		return DeviceSyncResult{Reason: "nodevice"}
	}
	target, ok, rerr := resolveTargetSave(client, cfg, romPath)
	if rerr != nil {
		return DeviceSyncResult{Reason: "resolve", AuthExpired: romm.IsAuthError(rerr)}
	}
	if !ok {
		return DeviceSyncResult{Reason: "nosave"}
	}

	var err error
	switch action {
	case "track":
		_, err = client.TrackSave(target.ID, dev)
	case "untrack":
		_, err = client.UntrackSave(target.ID, dev)
	}
	if err != nil {
		res := DeviceSyncResult{SaveID: target.ID}
		switch {
		case romm.IsAuthError(err):
			res.Reason, res.AuthExpired = "autherr", true
		case isNotFound(err) && action == "track":
			// track can't bootstrap a missing row — honest "nothing to resume".
			res.Reason = "notracked"
		default:
			res.Reason = "error"
		}
		fmt.Fprintf(os.Stderr, "device-sync: WARN %s save=%d failed: %s\n", action, target.ID, cleanErr(err))
		return res
	}
	return DeviceSyncResult{OK: true, SaveID: target.ID, Reason: "ok"}
}

// resolveTargetSave maps romPath to the newest REAL server save (ghosts and meta
// sidecars excluded) for its ROM. ok=false with a nil error means the ROM resolved but
// has no eligible save; a non-nil error is a reachability/auth failure.
func resolveTargetSave(client *romm.Client, cfg *config.Config, romPath string) (romm.Save, bool, error) {
	romID, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || romID == 0 {
		return romm.Save{}, false, nil
	}
	saves, err := client.GetSaves(romm.SaveQuery{RomID: romID})
	if err != nil {
		return romm.Save{}, false, err
	}
	var real []romm.Save
	for _, s := range saves {
		if IsGhostSave(s) || IsMetaSave(s) {
			continue
		}
		real = append(real, s)
	}
	if len(real) == 0 {
		return romm.Save{}, false, nil
	}
	return newestSave(real), true, nil
}

// isNotFound reports whether err is the client's generic 404 (used only to tell the
// track-can't-bootstrap case apart from a real error, for an honest reason token). The
// client surfaces a 404 as "API error: status 404, body: ..."; a match is advisory.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status 404")
}

// GetSavesAttributed fetches saves for q WITH this device's device_id set, so RomM
// (>= 4.9.0) populates each save's device_syncs — the per-device attribution the
// listing/feed/Continue UIs display. This is the READ mirror of the #176 write-side
// work: RomM's device_syncs field is CALLER-SCOPED — GET /api/saves returns an EMPTY
// device_syncs on every save UNLESS the request carries a device_id (the server skips
// the sync query entirely with no device in context). Without this the engine's
// "which device" column was ALWAYS blank even though the attribution rows exist.
//
// BEST-EFFORT and non-regressing: attribution can only ADD a device column, never
// break a working listing. We only attach device_id when a device is configured and
// the server is new enough; and on ANY error from the attributed call we fall back to
// the plain (un-attributed) query — so a token missing devices.read, a stale device
// row (404), or an older server all degrade to today's behavior instead of failing.
// The returned save SET is identical either way (device_id drives attribution only,
// never row filtering — verified against the 4.9.2 get_saves handler).
func GetSavesAttributed(client *romm.Client, cfg *config.Config, q romm.SaveQuery) ([]romm.Save, error) {
	dev := deviceID(cfg)
	if dev != "" && client.SupportsDeviceSaveSync() {
		aq := q
		aq.DeviceID = dev
		if saves, err := client.GetSaves(aq); err == nil {
			return saves, nil
		}
		// fall through: attribution is a bonus, not a gate — never fail the listing on it
	}
	q.DeviceID = ""
	return client.GetSaves(q)
}

// AttributedDeviceName picks the best "which device" label for a save from its
// device_syncs, given THIS device's id. It exists because RomM's caller-scoped
// projection sorts the CALLER'S OWN device FIRST and synthesizes a placeholder entry
// for it when the caller has never synced the save — so a naive device_syncs[0] would
// mislabel EVERY foreign save as this device. We therefore prefer a device OTHER than
// self (a genuine cross-device signal), favouring one that holds the current bytes
// (is_current), and fall back to self only when it is the sole entry. Empty string
// when there is no attribution at all (no device_syncs) — the caller keeps its own
// fallback (emulator name / blank).
func AttributedDeviceName(save romm.Save, selfDeviceID string) string {
	var currentOther, anyOther, selfName string
	for _, d := range save.DeviceSyncs {
		if d.DeviceName == "" {
			continue
		}
		if d.DeviceID == selfDeviceID {
			if selfName == "" {
				selfName = d.DeviceName
			}
			continue
		}
		if d.IsCurrent && currentOther == "" {
			currentOther = d.DeviceName
		}
		if anyOther == "" {
			anyOther = d.DeviceName
		}
	}
	switch {
	case currentOther != "":
		return currentOther
	case anyOther != "":
		return anyOther
	default:
		return selfName
	}
}

// DeviceName returns this device's configured display name (or "" if none) — the
// self-identity AttributedDeviceName excludes so foreign saves aren't mislabeled.
func DeviceName(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.ActiveHost().DeviceName
}

// SelfDeviceID exposes this device's configured device_id to the cmd layer (which
// needs it to call AttributedDeviceName). Empty when no device is registered.
func SelfDeviceID(cfg *config.Config) string {
	return deviceID(cfg)
}
