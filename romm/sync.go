package romm

// Managed sync — RomM 5.0.0's session-based save reconcile (POST /api/sync/negotiate
// + POST /api/sync/sessions/{id}/complete). This is the SUPPORTED replacement for
// Lodor's per-game raw POST /api/saves background reconcile (decision 2026-07-16). The
// client sends its whole-library local save inventory; the server returns a per-save
// operation plan (upload | download | conflict | no_op) keyed by rom_id; the client
// executes the plan against the verified push/pull primitives and reports counts back
// to close the session.
//
// Scope: negotiate reads assets.read + devices.read; complete writes devices.write —
// all three already present on every Lodor client_token (verified live 2026-07-16).
//
// SLOT SEMANTICS (load-bearing, from the 5.0.0 handler): saves are paired on
// (rom_id, slot). A NULL slot is treated as archival and ALWAYS negotiates as an
// upload even when the bytes already exist server-side under a slot — so the inventory
// MUST send a stable slot ("autosave", matching the push path) for a save to pair and
// reconcile instead of re-uploading every negotiation.
//
// This covers SAVES ONLY (negotiate is whole-library, no rom_id filter). States stay
// on the existing state-sync path. Stdlib only, CGO-free.

import (
	"fmt"
	"time"
)

// managedSyncMinVersion is the first RomM release carrying the /sync/negotiate +
// sync-session endpoints. Gated like every other server-feature call (fail closed on an
// unknown version).
var managedSyncMinVersion = [3]int{5, 0, 0}

// SupportsManagedSync reports whether the connected RomM is new enough (>= 5.0.0) to
// carry the managed sync-session endpoints. The gate every negotiate/complete call
// checks first; an unknown/unparseable version reads as false.
func (c *Client) SupportsManagedSync() bool {
	return versionAtLeast(c.ServerVersion(), managedSyncMinVersion)
}

// ClientSaveState is one element of the negotiate request's `saves` array — the
// server's ClientSaveState model. It is the client telling the server "here is a save I
// hold locally" so the server can diff it against its own store and plan an operation.
//
// Wire contract (RomM 5.0.0 source): RomID, FileName, UpdatedAt and FileSizeBytes are
// REQUIRED — omitting UpdatedAt or FileSizeBytes 422s the whole request. Slot, Emulator
// and ContentHash are nullable. Send a stable Slot ("autosave") or the save never
// pairs (see the slot-semantics note above).
type ClientSaveState struct {
	RomID         int       `json:"rom_id"`
	FileName      string    `json:"file_name"`
	Slot          string    `json:"slot,omitempty"`
	Emulator      string    `json:"emulator,omitempty"`
	ContentHash   string    `json:"content_hash,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
	FileSizeBytes int64     `json:"file_size_bytes"`
}

// syncNegotiatePayload is the POST /api/sync/negotiate request body. DeviceID is
// optional when the token is device-bound, but Lodor client tokens are NOT device-bound
// (their device_id column is NULL — verified 2026-07-16), so the caller MUST pass the
// device_id from config.
type syncNegotiatePayload struct {
	DeviceID string            `json:"device_id,omitempty"`
	Saves    []ClientSaveState `json:"saves"`
}

// SyncOperation is one planned operation in the negotiate response (the server's
// SyncOperationSchema). Action is one of "upload", "download", "conflict", "no_op".
// SaveID is null for uploads (no server save exists yet). ServerUpdatedAt /
// ServerContentHash are populated when a server save is involved.
type SyncOperation struct {
	Action            string  `json:"action"`
	RomID             int     `json:"rom_id"`
	SaveID            *int    `json:"save_id"`
	FileName          string  `json:"file_name"`
	Slot              *string `json:"slot"`
	Emulator          *string `json:"emulator"`
	Reason            string  `json:"reason"`
	ServerUpdatedAt   *string `json:"server_updated_at"`
	ServerContentHash *string `json:"server_content_hash"`
}

// SyncNegotiateResponse is the POST /api/sync/negotiate response (SyncNegotiateResponse).
// The totals are the server's own tally; TotalNoOp is NOT counted in the session's
// operations_planned (only upload+download+conflict are), which is why a complete with
// completed+failed < upload+download+conflict is tolerated as a partial completion.
type SyncNegotiateResponse struct {
	SessionID     int             `json:"session_id"`
	Operations    []SyncOperation `json:"operations"`
	TotalUpload   int             `json:"total_upload"`
	TotalDownload int             `json:"total_download"`
	TotalConflict int             `json:"total_conflict"`
	TotalNoOp     int             `json:"total_no_op"`
}

// Negotiate opens a managed sync session: POST /api/sync/negotiate with the device_id
// and the client's local save inventory. The server plans one operation per save and
// returns the session id + the plan. inv may be empty (the server then plans purely
// from its own device ledger). Gated on SupportsManagedSync by the caller.
func (c *Client) Negotiate(deviceID string, inv []ClientSaveState) (SyncNegotiateResponse, error) {
	if inv == nil {
		inv = []ClientSaveState{}
	}
	var out SyncNegotiateResponse
	err := c.doJSON("POST", "/api/sync/negotiate", syncNegotiatePayload{
		DeviceID: deviceID,
		Saves:    inv,
	}, &out)
	return out, err
}

// syncCompletePayload is the POST /api/sync/sessions/{id}/complete request body. All
// fields default server-side, so a bare {completed, failed} is valid; play_sessions is
// omitted (playtime stays on its own path).
type syncCompletePayload struct {
	OperationsCompleted int `json:"operations_completed"`
	OperationsFailed    int `json:"operations_failed"`
}

// SyncSession is the server's SyncSessionSchema — the session row after completion.
type SyncSession struct {
	ID                  int     `json:"id"`
	DeviceID            string  `json:"device_id"`
	Status              string  `json:"status"`
	OperationsPlanned   int     `json:"operations_planned"`
	OperationsCompleted int     `json:"operations_completed"`
	OperationsFailed    int     `json:"operations_failed"`
	ErrorMessage        *string `json:"error_message"`
}

// syncCompleteResponse is the POST .../complete response (SyncCompleteResponse); we only
// read the session block.
type syncCompleteResponse struct {
	Session SyncSession `json:"session"`
}

// CompleteSyncSession closes the session opened by Negotiate: POST
// /api/sync/sessions/{id}/complete with the executed counts. A partial completion
// (completed+failed < planned) is accepted by the server — conflicts we defer to the
// launch card are simply left out of both counts. Returns the finalized session row.
func (c *Client) CompleteSyncSession(sessionID, completed, failed int) (SyncSession, error) {
	if sessionID <= 0 {
		return SyncSession{}, fmt.Errorf("complete sync session: invalid session id %d", sessionID)
	}
	var out syncCompleteResponse
	path := fmt.Sprintf("/api/sync/sessions/%d/complete", sessionID)
	err := c.doJSON("POST", path, syncCompletePayload{
		OperationsCompleted: completed,
		OperationsFailed:    failed,
	}, &out)
	return out.Session, err
}
