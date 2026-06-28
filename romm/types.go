package romm

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Platform is the subset of a RomM platform the engine consumes (BLUEPRINT §1).
type Platform struct {
	ID            int    `json:"id"`
	Slug          string `json:"slug"`
	FsSlug        string `json:"fs_slug"`
	Name          string `json:"name"`
	CustomName    string `json:"custom_name"`
	RomCount      int    `json:"rom_count"`
	FirmwareCount int    `json:"firmware_count"`
	HasBios       bool   `json:"has_bios"`
}

// RomFile is one constituent file of a (possibly multi-file) ROM. For a multi-disc
// game each disc is one RomFile; the per-file ID drives the single-file content
// fetch (GET /api/roms/{id}/content/{fs_name}?file_ids=<file_id>, verified live
// 2026-06-25 — a single file_ids returns that file's raw bytes, while two or more
// fall back to a mod_zip archive), and the per-file hashes let each disc be verified
// independently the same way a single-file ROM is.
type RomFile struct {
	ID       int    `json:"id"`
	FileName string `json:"file_name"`
	Md5Hash  string `json:"md5_hash"`
	Sha1Hash string `json:"sha1_hash"`
}

// Rom is the subset of a RomM ROM the engine consumes (BLUEPRINT §1).
type Rom struct {
	ID                  int       `json:"id"`
	PlatformID          int       `json:"platform_id"`
	PlatformFsSlug      string    `json:"platform_fs_slug"`
	PlatformDisplayName string    `json:"platform_display_name"`
	FsName              string    `json:"fs_name"`
	FsNameNoExt         string    `json:"fs_name_no_ext"`
	FsExtension         string    `json:"fs_extension"`
	Name                string    `json:"name"`
	Md5Hash             string    `json:"md5_hash"`
	Sha1Hash            string    `json:"sha1_hash"`
	HasMultipleFiles    bool      `json:"has_multiple_files"`
	Files               []RomFile `json:"files"`
	RomIDs              []int     `json:"rom_ids"`

	// Box-art cover paths (BLUEPRINT §11 — box art). RomM exposes the rom's cover as
	// two server-relative asset paths, e.g.
	//   /assets/romm/resources/roms/5/10803/cover/small.png?ts=2026-06-25 03:00:51
	// PathCoverSmall is ~282x280, PathCoverLarge ~705x700. An UNIDENTIFIED rom (no
	// metadata match) has BOTH empty — the engine treats empty as "no cover, skip",
	// never as an error. The trailing "?ts=" cache-buster carries a space and MUST be
	// re-escaped before use (the romm client's DownloadCover handles that). url_cover
	// is an EXTERNAL screenscraper URL embedding a third-party devpassword and is
	// deliberately NOT consumed — covers come from RomM's own /assets only.
	PathCoverSmall string `json:"path_cover_small"`
	PathCoverLarge string `json:"path_cover_large"`
}

// CoverPath returns the preferred server-relative cover asset path for this rom, or
// "" when the rom has no cover (unidentified). The SMALL variant is preferred: it is
// already a thumbnail (~282x280, ~48KB) suited to a 640x480 handheld panel, where the
// large variant (~705x700, ~213KB) would be wasteful over 6,000 ROMs. Returns large
// only as a fallback when small is somehow absent but large is present.
func (r *Rom) CoverPath() string {
	if s := strings.TrimSpace(r.PathCoverSmall); s != "" {
		return s
	}
	return strings.TrimSpace(r.PathCoverLarge)
}

// CanonicalLocalBasename returns the extension-less basename this ROM occupies on
// disk once downloaded — the identity used to match local ROM and save files back
// to this ROM. Single-file ROMs (including RomM "nested single file" entries, where
// FsName is the containing folder) use the individual file's name; multi-file ROMs
// use FsNameNoExt (the m3u stem).
func (r *Rom) CanonicalLocalBasename() string {
	if !r.HasMultipleFiles && len(r.Files) > 0 {
		fn := r.Files[0].FileName
		return strings.TrimSuffix(fn, filepath.Ext(fn))
	}
	return r.FsNameNoExt
}

// DeviceSaveSync records which devices hold a copy of a save and which one is
// current.
type DeviceSaveSync struct {
	DeviceID     string    `json:"device_id"`
	DeviceName   string    `json:"device_name"`
	LastSyncedAt time.Time `json:"last_synced_at"`
	IsCurrent    bool      `json:"is_current"`
}

// Save is the subset of a RomM save the engine consumes (BLUEPRINT §1). Slot and
// ContentHash are pointers because the server omits them for older records.
type Save struct {
	ID            int              `json:"id"`
	RomID         int              `json:"rom_id"`
	FileName      string           `json:"file_name"`
	FileNameNoExt string           `json:"file_name_no_ext"`
	FileExtension string           `json:"file_extension"`
	FileSizeBytes int64            `json:"file_size_bytes"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Emulator      string           `json:"emulator"`
	Slot          *string          `json:"slot,omitempty"`
	ContentHash   *string          `json:"content_hash,omitempty"`
	DeviceSyncs   []DeviceSaveSync `json:"device_syncs,omitempty"`
}

// Firmware is the subset of a RomM firmware/BIOS entry the engine consumes.
type Firmware struct {
	ID       int    `json:"id"`
	FileName string `json:"file_name"`
	Md5Hash  string `json:"md5_hash"`
	Sha1Hash string `json:"sha1_hash"`
}

// Collection is a named RomM collection and its member ROM ids.
type Collection struct {
	Name   string `json:"name"`
	RomIDs []int  `json:"rom_ids"`
}

// PaginatedRoms is the paged response envelope for GET /api/roms.
type PaginatedRoms struct {
	Items  []Rom `json:"items"`
	Total  int   `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

// ConflictError represents a 409 from POST /api/saves, returned when an upload
// conflicts with a newer server save. Both the direct shape and the FastAPI
// {"detail":{...}} wrapper are parsed.
type ConflictError struct {
	ErrorType       string    `json:"error"`
	Message         string    `json:"message"`
	SaveID          int       `json:"save_id"`
	CurrentSaveTime time.Time `json:"current_save_time"`
	DeviceSyncTime  time.Time `json:"device_sync_time"`
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflict: %s (save_id=%d)", e.Message, e.SaveID)
}

// parseConflictError decodes a 409 body into a *ConflictError, accepting either
// the direct object or a FastAPI detail wrapper, with a generic fallback.
func parseConflictError(body []byte) error {
	var direct ConflictError
	if err := json.Unmarshal(body, &direct); err == nil && direct.ErrorType != "" {
		return &direct
	}
	var wrapper struct {
		Detail ConflictError `json:"detail"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Detail.ErrorType != "" {
		return &wrapper.Detail
	}
	return &ConflictError{ErrorType: "conflict", Message: string(body)}
}

// PlaySessionEntry is one play session reported to RomM (POST /api/play-sessions).
// The wire shape mirrors the server's PlaySessionEntry pydantic model
// (backend/endpoints/play_sessions.py): StartTime/EndTime are RFC3339 datetimes the
// server truncates to whole seconds, DurationMs is the played time in MILLISECONDS
// (>=0), and the server REJECTS an entry whose EndTime is not strictly after
// StartTime (422). SaveSlot is optional and omitted by the engine. rom_id is an int
// here because the engine only ever reports a session for a resolved ROM.
type PlaySessionEntry struct {
	RomID      int    `json:"rom_id"`
	SaveSlot   string `json:"save_slot,omitempty"`
	StartTime  string `json:"start_time"`
	EndTime    string `json:"end_time"`
	DurationMs int64  `json:"duration_ms"`
}

// PlaySessionIngestPayload is the POST /api/play-sessions request body: an optional
// top-level device_id plus a batch of sessions (server cap: 100 per request).
type PlaySessionIngestPayload struct {
	DeviceID string             `json:"device_id,omitempty"`
	Sessions []PlaySessionEntry `json:"sessions"`
}

// PlaySessionIngestResponse is the subset of the 201 response the engine consumes:
// how many sessions the server created vs skipped (deduped).
type PlaySessionIngestResponse struct {
	CreatedCount int `json:"created_count"`
	SkippedCount int `json:"skipped_count"`
}

