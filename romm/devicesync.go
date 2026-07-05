package romm

// device_save_sync ledger endpoints (task #176) — making Lodor a first-class citizen
// of RomM's per-device save-tracking model. RomM keys a device_save_sync row on
// (save_id, device_id); the row records last_synced_at and is_untracked. These four
// id-scoped calls are the EXPLICIT controls over that row. Every one of them is
// invoked BEST-EFFORT by the sync/cmd layers — they touch no save bytes, the
// content-hash 3-way reconcile stays the sole conflict authority, and any error
// (403/404/409/500/offline) is logged and swallowed so a save/download/launch never
// fails on the ledger. All are gated on RomM >= 4.9.0 (SupportsDeviceSaveSync).
//
// Wire contract (verified against the 4.9.2 stable source read):
//   - body {"device_id":"<uuid>"}; scope devices.write (track/untrack/downloaded);
//   - response is the updated SaveSchema; idempotent; 404 if save/device missing,
//     422 if the body omits device_id.
//   - is_current is a SERVER-COMPUTED response field (last_synced_at >= updated_at),
//     never a column we set.
//
// Stdlib only, CGO-free.

import (
	"fmt"
	"net/url"
	"strconv"
)

// deviceIDBody is the shared request body for the track/untrack/downloaded endpoints.
type deviceIDBody struct {
	DeviceID string `json:"device_id"`
}

// TrackSave marks this device's sync row for save id as tracked (is_untracked=false)
// via POST /api/saves/{id}/track. ASYMMETRY (load-bearing): track only UPDATES an
// existing row — it cannot create one. On a save with no prior sync row for this
// device the server 404s; the caller treats that as "nothing to resume" and moves on.
// Returns the updated Save. deviceID must be non-empty (the server 422s an empty body).
func (c *Client) TrackSave(saveID int, deviceID string) (Save, error) {
	return c.postDeviceSave(saveID, "track", deviceID)
}

// UntrackSave marks this device's sync row for save id as untracked (is_untracked=true)
// via POST /api/saves/{id}/untrack — the "stop syncing this save on this device"
// control. Unlike track, untrack CREATES the row if absent, so it always has an effect
// on a real save. Returns the updated Save.
func (c *Client) UntrackSave(saveID int, deviceID string) (Save, error) {
	return c.postDeviceSave(saveID, "untrack", deviceID)
}

// MarkSaveDownloaded confirms this device downloaded save id via
// POST /api/saves/{id}/downloaded, upserting the sync row (last_synced_at =
// save.updated_at, is_untracked=false). This is the download-side ledger write we need
// precisely because our content download runs optimistic=false (the server does NOT
// advance the row on a non-optimistic GET /content, deliberately — the row is confirmed
// only AFTER the bytes are safely written locally). Returns the updated Save.
func (c *Client) MarkSaveDownloaded(saveID int, deviceID string) (Save, error) {
	return c.postDeviceSave(saveID, "downloaded", deviceID)
}

// postDeviceSave is the shared POST /api/saves/{id}/{action} call.
func (c *Client) postDeviceSave(saveID int, action, deviceID string) (Save, error) {
	if deviceID == "" {
		return Save{}, fmt.Errorf("device_save_sync %s: empty device_id", action)
	}
	var out Save
	path := fmt.Sprintf("/api/saves/%d/%s", saveID, action)
	err := c.doJSON("POST", path, deviceIDBody{DeviceID: deviceID}, &out)
	return out, err
}

// SaveSlotSummary is one slot's roll-up within a rom's save summary: the slot name,
// how many saves it holds, and the latest save in it.
type SaveSlotSummary struct {
	Slot   string `json:"slot"`
	Count  int    `json:"count"`
	Latest Save   `json:"latest"`
}

// SavesSummary is the GET /api/saves/summary response for one rom: a total count and a
// per-slot roll-up. Optional, read-only (scope assets.read) — drives the server-saves
// view without pulling every save row.
type SavesSummary struct {
	TotalCount int               `json:"total_count"`
	Slots      []SaveSlotSummary `json:"slots"`
}

// GetSavesSummary fetches GET /api/saves/summary?rom_id=<id> (scope assets.read). Like
// every #176 call it is gated on SupportsDeviceSaveSync and used best-effort; a failure
// simply means the caller falls back to the full GetSaves listing.
func (c *Client) GetSavesSummary(romID int) (SavesSummary, error) {
	v := url.Values{}
	v.Set("rom_id", strconv.Itoa(romID))
	var out SavesSummary
	err := c.doJSON("GET", "/api/saves/summary?"+v.Encode(), nil, &out)
	return out, err
}
