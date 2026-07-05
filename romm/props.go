package romm

// On-device rom write-back (task #167) — the launcher's per-game Y-menu pushing a
// user's favorite / rating / status / difficulty / completion / backlogged /
// now_playing / hidden back to RomM. Every call here is invoked BEST-EFFORT by the
// cmd layer: it touches no ROM or save bytes, and any error (403 scope, 404 rom,
// 422 range, offline) is surfaced HONESTLY (never a fake "saved!") but never panics.
//
// Wire contract (verified byte-identical across RomM 4.9.2 / master / 5.0.0-alpha.3,
// so NO version gate — the floor is far below our RomM >= 4.8 requirement):
//
//   - props → PUT /api/roms/{id}/props (scope roms.user.write). PATCH-like: the
//     server writes ONLY the keys present in the body (exclude_unset) and leaves the
//     rest alone. rating/difficulty are 0-10, completion 0-100 (422 out of range);
//     status is the RomUserStatus enum (nullable — send null to clear). The row is
//     auto-created on first call; the write is absolute (idempotent).
//   - favorite is NOT a rom prop — RomM has no is_favorite on rom_user. It is
//     membership in the per-user "Favourites" collection (is_favorite==true), added
//     / removed via POST|DELETE /api/collections/{id}/roms (scope collections.write).
//
// Stdlib only, CGO-free.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
)

// RomUserStatus is the play-status enum on a rom_user row (exact wire values).
type RomUserStatus string

const (
	StatusIncomplete   RomUserStatus = "incomplete"
	StatusFinished     RomUserStatus = "finished"
	StatusCompleted100 RomUserStatus = "completed_100"
	StatusRetired      RomUserStatus = "retired"
	StatusNeverPlaying RomUserStatus = "never_playing"
)

// ValidRomUserStatus reports whether s is one of the five wire-legal status values.
func ValidRomUserStatus(s RomUserStatus) bool {
	switch s {
	case StatusIncomplete, StatusFinished, StatusCompleted100, StatusRetired, StatusNeverPlaying:
		return true
	}
	return false
}

// RomUserData is the PATCH-like body of PUT /api/roms/{id}/props. It marshals with
// exclude_unset semantics: a nil pointer field is OMITTED entirely (the server keeps
// its current value), a non-nil pointer is sent (even a zero — &0 sends rating=0 to
// CLEAR). Status is three-state: nil + ClearStatus=false omits the key; a non-nil
// Status sends the enum; nil + ClearStatus=true sends `"status": null` (the only way
// to clear a nullable enum, since a bare omit means "leave unchanged"). Build it with
// the Ptr* helpers or literal &v.
type RomUserData struct {
	IsMainSibling *bool
	Backlogged    *bool
	NowPlaying    *bool
	Hidden        *bool
	Rating        *int // 0-10 (0 clears)
	Difficulty    *int // 0-10 (0 clears)
	Completion    *int // 0-100 (0 clears)
	Status        *RomUserStatus
	ClearStatus   bool // send status:null (ignored when Status != nil)
}

// IsEmpty reports whether the body would serialize to no keys — a no-op PUT the CLI
// rejects as a bad-arg rather than firing a pointless request.
func (d RomUserData) IsEmpty() bool {
	return d.IsMainSibling == nil && d.Backlogged == nil && d.NowPlaying == nil &&
		d.Hidden == nil && d.Rating == nil && d.Difficulty == nil &&
		d.Completion == nil && d.Status == nil && !d.ClearStatus
}

// MarshalJSON emits only the explicitly-set fields (exclude_unset), so the PATCH-like
// endpoint writes exactly what the user changed and nothing else. Object key order is
// irrelevant to the server (and tests decode into a map).
func (d RomUserData) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	if d.IsMainSibling != nil {
		m["is_main_sibling"] = *d.IsMainSibling
	}
	if d.Backlogged != nil {
		m["backlogged"] = *d.Backlogged
	}
	if d.NowPlaying != nil {
		m["now_playing"] = *d.NowPlaying
	}
	if d.Hidden != nil {
		m["hidden"] = *d.Hidden
	}
	if d.Rating != nil {
		m["rating"] = *d.Rating
	}
	if d.Difficulty != nil {
		m["difficulty"] = *d.Difficulty
	}
	if d.Completion != nil {
		m["completion"] = *d.Completion
	}
	if d.Status != nil {
		m["status"] = *d.Status
	} else if d.ClearStatus {
		m["status"] = nil
	}
	return json.Marshal(m)
}

// PtrBool / PtrInt / PtrStatus are small helpers so callers can build a partial
// RomUserData inline without scattering address-of temporaries.
func PtrBool(b bool) *bool                    { return &b }
func PtrInt(i int) *int                       { return &i }
func PtrStatus(s RomUserStatus) *RomUserStatus { return &s }

// RomUserSchema is the subset of the PUT /api/roms/{id}/props 200 response the engine
// reads back to confirm the write landed. Status is a pointer (nullable on the wire).
type RomUserSchema struct {
	RomID      int            `json:"rom_id"`
	Rating     int            `json:"rating"`
	Difficulty int            `json:"difficulty"`
	Completion int            `json:"completion"`
	Status     *RomUserStatus `json:"status"`
	Backlogged bool           `json:"backlogged"`
	NowPlaying bool           `json:"now_playing"`
	Hidden     bool           `json:"hidden"`
}

// SetRomProps writes the set fields of data to rom romID via PUT /api/roms/{id}/props
// (scope roms.user.write), returning the updated rom_user row. Only the non-nil fields
// are transmitted (exclude_unset) so unrelated props are never clobbered. Errors are
// returned verbatim for the cmd layer to classify (403/404/422/offline → honest reason).
func (c *Client) SetRomProps(romID int, data RomUserData) (RomUserSchema, error) {
	var out RomUserSchema
	err := c.doJSON("PUT", fmt.Sprintf("/api/roms/%d/props", romID), data, &out)
	return out, err
}

// collectionRomsBody is the {"rom_ids":[...]} body shared by the add/remove-roms
// endpoints. The DELETE (unfavorite) call sends this body ON A DELETE — our http
// client transmits it (http.NewRequest wires a non-nil body for any method); the
// mock test asserts the server actually receives it.
type collectionRomsBody struct {
	RomIDs []int `json:"rom_ids"`
}

// GetFavouritesCollectionID returns the id of this user's "Favourites" collection
// (the one with is_favorite==true), creating it if it does not exist yet. Discovery
// and (if needed) creation both require scope collections.read + collections.write.
// After a create it RE-READS the collection list and confirms a favourites collection
// is present (the contract's GET-confirm), so a server that doesn't echo is_favorite
// on the create response is still handled. A discovery/create failure is returned.
func (c *Client) GetFavouritesCollectionID() (int, error) {
	if id, ok, err := c.findFavouriteCollection(); err != nil {
		return 0, err
	} else if ok {
		return id, nil
	}
	// First-ever favorite: create the special collection (multipart form, is_favorite
	// sent explicitly), then GET-confirm it now exists as the favourites collection.
	if _, err := c.createFavouritesCollection(); err != nil {
		return 0, err
	}
	id, ok, err := c.findFavouriteCollection()
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("favourites collection not present after create")
	}
	return id, nil
}

// findFavouriteCollection scans GET /api/collections for the is_favorite==true entry.
func (c *Client) findFavouriteCollection() (int, bool, error) {
	cols, err := c.GetCollections()
	if err != nil {
		return 0, false, err
	}
	for _, col := range cols {
		if col.IsFavorite && col.ID > 0 {
			return col.ID, true, nil
		}
	}
	return 0, false, nil
}

// createFavouritesCollection POSTs a new "Favourites" collection via
// multipart/form-data with name AND is_favorite=true sent explicitly (the documented
// create shape). Returns the created CollectionSchema (201).
func (c *Client) createFavouritesCollection() (Collection, error) {
	var out Collection
	err := c.doMultipartForm(http.MethodPost, "/api/collections", map[string]string{
		"name":        "Favourites",
		"is_favorite": "true",
	}, &out)
	return out, err
}

// AddRomToCollection favorites romID by adding it to collection colID via
// POST /api/collections/{id}/roms body {"rom_ids":[romID]} (scope collections.write).
// Idempotent server-side (a re-add dedupes). Returns the updated collection (200).
func (c *Client) AddRomToCollection(colID, romID int) (Collection, error) {
	var out Collection
	err := c.doJSON(http.MethodPost, fmt.Sprintf("/api/collections/%d/roms", colID),
		collectionRomsBody{RomIDs: []int{romID}}, &out)
	return out, err
}

// RemoveRomFromCollection unfavorites romID by removing it from collection colID via
// DELETE /api/collections/{id}/roms body {"rom_ids":[romID]} (scope collections.write).
// This sends a JSON body ON A DELETE — verified transmitted by our client. Idempotent
// (removing an absent rom is a no-op). Returns the updated collection (200, NOT 201).
func (c *Client) RemoveRomFromCollection(colID, romID int) (Collection, error) {
	var out Collection
	err := c.doJSON(http.MethodDelete, fmt.Sprintf("/api/collections/%d/roms", colID),
		collectionRomsBody{RomIDs: []int{romID}}, &out)
	return out, err
}

// doMultipartForm POSTs a multipart/form-data body of plain text fields (no file part)
// and decodes a JSON response into out. Used by the favourites-collection create, whose
// documented shape is multipart form (name + is_favorite), not JSON.
func (c *Client) doMultipartForm(method, path string, fields map[string]string, out any) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return fmt.Errorf("write form field %s: %w", k, err)
		}
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequest(method, c.baseURL+path, &buf)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		if aerr := authErrorFromStatus(resp.StatusCode, raw); aerr != nil {
			return aerr
		}
		return fmt.Errorf("API error: status %d, body: %s", resp.StatusCode, string(raw))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
