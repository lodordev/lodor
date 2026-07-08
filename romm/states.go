package romm

// Save-STATE endpoints (Handoff v1 — design lodor-statesync-design-2026-07-07.md).
//
// Contract verified live against production RomM 4.9.2 openapi (2026-07-07):
//   GET  /api/states?rom_id=          → []StateSchema
//   POST /api/states?rom_id=&emulator= multipart stateFile (+ screenshotFile)
//   POST /api/states/delete           {"states":[ids]}
//   GET  /api/raw/assets/{path}       ← 4.9.2 content download (path from
//                                        StateSchema.download_path; carries SPACES
//                                        and a ?timestamp= query — must be encoded)
// RomM 5.0 REMOVES raw/assets and ADDS GET /api/states/{id}/content — the D1
// version gate below picks per server (heartbeat pattern, fail-closed to the
// 4.9.2 path on unknown versions since that's the deployed reality).
//
// 4.9.2 StateSchema has NO content_hash, NO slot, NO device attribution — the
// engine's own state ledger owns identity (design D2); the producer tuple rides
// the emulator field (D3); slot + origin ride the filename (D6).

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// State is the subset of a RomM state record the engine consumes (4.9.2 schema;
// re-check at 5.0 stable for content_hash — design O5).
type State struct {
	ID            int       `json:"id"`
	RomID         int       `json:"rom_id"`
	FileName      string    `json:"file_name"`
	FileExtension string    `json:"file_extension"`
	FileSizeBytes int64     `json:"file_size_bytes"`
	DownloadPath  string    `json:"download_path"`
	Emulator      string    `json:"emulator"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	MissingFromFS bool      `json:"missing_from_fs"`
}

// statesContentMinVersion is the first RomM with GET /api/states/{id}/content
// (and without /api/raw/assets). 5.0.0 per the tag-level source read.
var statesContentMinVersion = [3]int{5, 0, 0}

// GetStates lists the server's state records for one rom, all users' visible set.
func (c *Client) GetStates(romID int) ([]State, error) {
	v := url.Values{}
	v.Set("rom_id", strconv.Itoa(romID))
	var out []State
	err := c.doJSON("GET", "/api/states?"+v.Encode(), nil, &out)
	return out, err
}

// UploadState uploads one normalized (RAW-payload) state. emulator carries the
// producer tuple string; fileName carries slot+origin per the D6 convention.
// screenshot is optional (nil = none) — cosmetic, for picker UIs.
func (c *Client) UploadState(romID int, emulator, fileName string, data []byte) (State, error) {
	v := url.Values{}
	v.Set("rom_id", strconv.Itoa(romID))
	if emulator != "" {
		v.Set("emulator", emulator)
	}
	var out State
	err := c.doMultipart("/api/states", v, "stateFile", fileName, data, &out)
	return out, err
}

// DownloadStateContent fetches a state's bytes via the version-correct route.
func (c *Client) DownloadStateContent(s State) ([]byte, error) {
	if versionAtLeast(c.ServerVersion(), statesContentMinVersion) {
		return c.doRaw("GET", fmt.Sprintf("/api/states/%d/content", s.ID))
	}
	enc, err := encodeRawAssetPath(s.DownloadPath)
	if err != nil {
		return nil, err
	}
	return c.doRaw("GET", enc)
}

// DeleteStates removes state records by id (POST /api/states/delete,
// {"states":[ids]}). Handoff uses this ONLY for the engine's own retention of
// its OWN prior uploads (design 6.4) and test cleanup — never another device's
// or client's records (invariant 7.9: no deletion ever propagates).
func (c *Client) DeleteStates(ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	body := map[string][]int{"states": ids}
	return c.doJSON("POST", "/api/states/delete", body, nil)
}

// encodeRawAssetPath prepares a 4.9.2 download_path for doRaw/buildURL, whose
// contract is RAW path + PRE-ENCODED query (buildURL escapes the path itself —
// pre-escaping it double-encodes on the wire, caught by the wire test). The
// download_path's ?timestamp= value carries a literal space, which would be an
// illegal request-URI byte, so ONLY the query gets encoded here.
func encodeRawAssetPath(dp string) (string, error) {
	if dp == "" {
		return "", fmt.Errorf("romm: state has empty download_path")
	}
	path, query, _ := strings.Cut(dp, "?")
	if query == "" {
		return path, nil
	}
	k, val, ok := strings.Cut(query, "=")
	if !ok {
		return "", fmt.Errorf("romm: unparseable download_path query %q", query)
	}
	return path + "?" + k + "=" + url.QueryEscape(val), nil
}
