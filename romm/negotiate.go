package romm

import (
	"strconv"
	"time"
)

// Server-side sync negotiation (Argosy research #3): on RomM >= 4.9.0 the device
// asks the server to compute the save-sync plan rather than reconciling client-side.
// The device POSTs its per-rom save fingerprints; the server 3-way reconciles against
// its own state + recorded sync anchors and returns an ordered list of operations
// (upload|download|conflict|no_op); the device executes them and POSTs a
// session-complete.
//
// WIRE SHAPE VERIFIED 2026-06-28 against a live RomM 4.9.2 server. Corrected from the
// original best-effort guess: session_id is an integer; the per-op key is "action"
// with vocabulary upload/download/conflict/no_op; each op also carries
// slot/emulator/server_updated_at/server_content_hash; the response carries total_*
// counters. The completion path is POST /api/sync/sessions/{id}/complete and there is
// no DELETE for sync sessions.

// Sync operation kinds returned in a negotiated plan (the server's "action" field).
// The string values are the live RomM 4.9.2 vocabulary. Names describe the device's
// action: upload pushes the local save, download pulls the server save.
const (
	SyncOpUpload   = "upload"   // upload the device's local save for this rom
	SyncOpDownload = "download" // download SaveID from the server to the device
	SyncOpConflict = "conflict" // both sides diverged — surface, never auto-resolve
	SyncOpNoOp     = "no_op"    // already in sync — nothing to do
)

// NegotiateSaveRef is one device-side save fingerprint in a negotiate request: the rom
// it belongs to and the MD5 content hash of the bytes currently on the card (empty
// Hash = no local save for that rom).
type NegotiateSaveRef struct {
	RomID    int    `json:"rom_id"`
	Hash     string `json:"hash,omitempty"`
	Emulator string `json:"emulator,omitempty"`
	Slot     string `json:"slot,omitempty"`
}

// NegotiateRequest is the body of POST /api/sync/negotiate: the device asking the
// server to compute a save-sync plan from its current per-rom save fingerprints.
type NegotiateRequest struct {
	DeviceID string             `json:"device_id"`
	Saves    []NegotiateSaveRef `json:"saves"`
}

// SyncOp is one operation in a negotiated sync plan. Op is one of the SyncOp* consts
// (wire key "action"). SaveID names the server save to download (action=download).
// ServerUpdatedAt / ServerContentHash describe the server's current save for this rom
// (used to record the post-op anchor without a second round-trip). Slot / Emulator
// scope the save. Reason is an optional server-supplied explanation, surfaced verbatim
// only in host-free diagnostics.
type SyncOp struct {
	Op                string    `json:"action"`
	RomID             int       `json:"rom_id"`
	SaveID            int       `json:"save_id,omitempty"`
	FileName          string    `json:"file_name,omitempty"`
	Slot              string    `json:"slot,omitempty"`
	Emulator          string    `json:"emulator,omitempty"`
	ServerUpdatedAt   time.Time `json:"server_updated_at,omitempty"`
	ServerContentHash string    `json:"server_content_hash,omitempty"`
	Reason            string    `json:"reason,omitempty"`
}

// NegotiateResponse is the server's plan: an integer session id (to complete after
// executing), the ordered operations the device must perform, and the server's own
// total_* counters for the plan (informational; the device recomputes its own tallies
// as it executes).
type NegotiateResponse struct {
	SessionID      int      `json:"session_id"`
	Operations     []SyncOp `json:"operations"`
	TotalUploads   int      `json:"total_uploads,omitempty"`
	TotalDownloads int      `json:"total_downloads,omitempty"`
	TotalConflicts int      `json:"total_conflicts,omitempty"`
	TotalNoOps     int      `json:"total_no_ops,omitempty"`
}

// Negotiate posts the device's save fingerprints to POST /api/sync/negotiate and
// returns the server-computed plan. Only valid against a server whose Capabilities
// report SupportsSyncNegotiate; the caller is responsible for that gate.
func (c *Client) Negotiate(req NegotiateRequest) (NegotiateResponse, error) {
	var out NegotiateResponse
	err := c.doJSON("POST", "/api/sync/negotiate", req, &out)
	return out, err
}

// CompleteSyncSession finalizes a negotiated session via
// POST /api/sync/sessions/{id}/complete after every operation has been executed. The
// integer session id is rendered into the path. An empty body is sent (the server keys
// off the id). There is no DELETE for sync sessions.
func (c *Client) CompleteSyncSession(sessionID int) error {
	path := "/api/sync/sessions/" + strconv.Itoa(sessionID) + "/complete"
	return c.doJSON("POST", path, struct{}{}, nil)
}
