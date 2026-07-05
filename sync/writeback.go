package sync

// On-device rom write-back orchestration (task #167). The launcher's per-game Y-menu
// pushes a user's favorite / rating / status / difficulty / completion / backlogged /
// now_playing / hidden back to RomM. Each entrypoint resolves a local ROM path to its
// rom_id via the SAME catalog index the save-sync modes use, then makes the best-effort
// write and returns a TYPED reason the cmd layer prints honestly.
//
// These are user-initiated actions, so a failure is reported (never a fake "saved!"),
// but the calls never panic and never touch ROM/save bytes — purely additive to the
// existing sync surface. No version gate: the props + favourites endpoints are byte-
// identical across every RomM the engine supports; an unreachable server or an ancient
// server that 404s the endpoint is surfaced honestly as unreachable / notfound.

import (
	"fmt"
	"os"
	"strings"

	"lodor/catalog"
	"lodor/config"
	"lodor/romm"
)

// WriteBackResult is the outcome of one write-back action.
type WriteBackResult struct {
	OK          bool   // the write landed
	RomID       int    // the resolved rom id (0 when the path didn't resolve)
	Reason      string // machine token: ok|resolve|notfound|forbidden|range|unreachable|autherr|error
	AuthExpired bool   // the token was rejected (re-pair) — surfaces PAIRING_EXPIRED
}

// SetFavoriteForRom favorites (favorite=true) or unfavorites (false) the ROM at
// romPath by adding/removing it in the user's "Favourites" collection (creating that
// collection on the first-ever favorite). Best-effort: any failure is a typed reason.
func SetFavoriteForRom(client *romm.Client, cfg *config.Config, romPath string, favorite bool) WriteBackResult {
	romID, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || romID == 0 {
		return WriteBackResult{Reason: "resolve"}
	}
	colID, err := client.GetFavouritesCollectionID()
	if err != nil {
		return classifyWriteBack(romID, err)
	}
	if favorite {
		_, err = client.AddRomToCollection(colID, romID)
	} else {
		_, err = client.RemoveRomFromCollection(colID, romID)
	}
	if err != nil {
		return classifyWriteBack(romID, err)
	}
	return WriteBackResult{OK: true, RomID: romID, Reason: "ok"}
}

// SetRomPropsForRom writes the set fields of data (rating/status/difficulty/completion/
// backlogged/now_playing/hidden) to the ROM at romPath via PUT /api/roms/{id}/props.
// Only the fields the caller set are sent (exclude_unset). Best-effort, typed reason.
func SetRomPropsForRom(client *romm.Client, cfg *config.Config, romPath string, data romm.RomUserData) WriteBackResult {
	romID, ok := catalog.ResolveRomID(cfg, romPath)
	if !ok || romID == 0 {
		return WriteBackResult{Reason: "resolve"}
	}
	if _, err := client.SetRomProps(romID, data); err != nil {
		return classifyWriteBack(romID, err)
	}
	return WriteBackResult{OK: true, RomID: romID, Reason: "ok"}
}

// classifyWriteBack maps a client error to an honest reason token and logs a host-free
// WARN. A scope/permission 403 is reported as forbidden (NOT auth-expired — re-pairing
// wouldn't fix a missing collections.write scope); a token-blaming 401/403 is autherr.
func classifyWriteBack(romID int, err error) WriteBackResult {
	res := WriteBackResult{RomID: romID}
	switch {
	case romm.IsAuthError(err):
		res.Reason, res.AuthExpired = "autherr", true
	case strings.Contains(err.Error(), "status 404"):
		res.Reason = "notfound"
	case strings.Contains(err.Error(), "status 403"):
		res.Reason = "forbidden"
	case strings.Contains(err.Error(), "status 422"):
		res.Reason = "range"
	case cleanErr(err) == "network error — try again":
		res.Reason = "unreachable"
	default:
		res.Reason = "error"
	}
	fmt.Fprintf(os.Stderr, "writeback: WARN rom=%d failed: %s\n", romID, cleanErr(err))
	return res
}
