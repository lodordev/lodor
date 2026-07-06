package romm

// Contract tests for the on-device rom write-back (task #167), driven by a MOCK RomM
// that mirrors the verified wire shape: PUT /api/roms/{id}/props (partial/exclude-unset,
// rating/difficulty 0-10 + completion 0-100 range 422, status enum validation, scope
// 403 without roms.user.write) and the "Favourites" collection flow (GET discover, POST
// create WITH is_favorite, POST/DELETE roms with a body — INCLUDING the assertion that
// the DELETE body is actually received — 200-not-201, idempotency, 403 wrong-owner).
//
// RomM is unreachable from the build host, so this proves the WIRE SHAPE and the honest
// best-effort contract at the client layer — not a live round-trip. Reuses the shared
// scope helpers (scopesOf/requireScopes/deny/testClient) defined in devicesync_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

const writeBackScopes = "roms.user.write,collections.read,collections.write"

// ---- mock RomM (props + collections) --------------------------------------------------

type propsMock struct {
	// rom_user state per rom id (absolute values; rows auto-created on first write).
	rows map[int]*RomUserSchema
	// last PUT props: which keys the body carried (exclude_unset proof).
	lastKeys map[string]bool

	// collections state.
	cols      map[int]*Collection
	nextColID int
	// createEchoesFavorite=false makes POST /api/collections omit is_favorite from its
	// response body, forcing the client's GET-confirm fallback to carry the discovery.
	createEchoesFavorite bool
	wrongOwnerCol        int // a collection id that 403s add/remove (not the user's)

	// DELETE-with-body proof.
	deleteBodyReceived bool
	deleteBodyRomIDs   []int
}

func newPropsMock() *propsMock {
	return &propsMock{
		rows:                 map[int]*RomUserSchema{},
		lastKeys:             map[string]bool{},
		cols:                 map[int]*Collection{},
		nextColID:            10,
		createEchoesFavorite: true,
	}
}

func validStatus(s string) bool {
	switch RomUserStatus(s) {
	case StatusIncomplete, StatusFinished, StatusCompleted100, StatusRetired, StatusNeverPlaying:
		return true
	}
	return false
}

func unprocessable(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	_, _ = w.Write([]byte(`{"detail":"` + msg + `"}`))
}

func (m *propsMock) server() *httptest.Server {
	mux := http.NewServeMux()

	// PUT /api/roms/{id}/props → roms.user.write. PATCH-like: apply only the keys sent.
	mux.HandleFunc("PUT /api/roms/{id}/props", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "roms.user.write") {
			return
		}
		id, _ := strconv.Atoi(r.PathValue("id"))
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			unprocessable(w, "bad body")
			return
		}
		row, ok := m.rows[id]
		if !ok {
			row = &RomUserSchema{RomID: id} // auto-create on first call
			m.rows[id] = row
		}
		m.lastKeys = map[string]bool{}
		for k := range body {
			m.lastKeys[k] = true
		}
		// range + enum validation → 422.
		if raw, ok := body["rating"]; ok {
			var n int
			_ = json.Unmarshal(raw, &n)
			if n < 0 || n > 10 {
				unprocessable(w, "rating out of range")
				return
			}
			row.Rating = n
		}
		if raw, ok := body["difficulty"]; ok {
			var n int
			_ = json.Unmarshal(raw, &n)
			if n < 0 || n > 10 {
				unprocessable(w, "difficulty out of range")
				return
			}
			row.Difficulty = n
		}
		if raw, ok := body["completion"]; ok {
			var n int
			_ = json.Unmarshal(raw, &n)
			if n < 0 || n > 100 {
				unprocessable(w, "completion out of range")
				return
			}
			row.Completion = n
		}
		if raw, ok := body["status"]; ok {
			if string(raw) == "null" {
				row.Status = nil
			} else {
				var s string
				if err := json.Unmarshal(raw, &s); err != nil || !validStatus(s) {
					unprocessable(w, "bad status")
					return
				}
				st := RomUserStatus(s)
				row.Status = &st
			}
		}
		if raw, ok := body["backlogged"]; ok {
			_ = json.Unmarshal(raw, &row.Backlogged)
		}
		if raw, ok := body["now_playing"]; ok {
			_ = json.Unmarshal(raw, &row.NowPlaying)
		}
		if raw, ok := body["hidden"]; ok {
			_ = json.Unmarshal(raw, &row.Hidden)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(row)
	})

	// GET /api/collections → collections.read.
	mux.HandleFunc("GET /api/collections", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "collections.read") {
			return
		}
		var out []Collection
		for _, c := range m.cols {
			out = append(out, *c)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	// POST /api/collections → collections.write. name/description are multipart form
	// fields; is_favorite is a QUERY parameter (real RomM 4.9.2/5.0 read it ONLY from
	// the query string — a form field is ignored). Read it from r.URL.Query() here (NOT
	// r.FormValue, which merges query+form and would mask the client sending it in the
	// wrong place) so this test fails if the client ever regresses to a form field.
	mux.HandleFunc("POST /api/collections", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "collections.write") {
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			unprocessable(w, "not multipart")
			return
		}
		fav := r.URL.Query().Get("is_favorite") == "true"
		id := m.nextColID
		m.nextColID++
		col := &Collection{ID: id, Name: r.FormValue("name"), IsFavorite: fav}
		m.cols[id] = col
		resp := *col
		if !m.createEchoesFavorite {
			resp.IsFavorite = false // server that omits it on create; GET-confirm still works
		}
		w.WriteHeader(http.StatusCreated) // 201
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	// POST /api/collections/{id}/roms → collections.write; add (dedupe). 200.
	mux.HandleFunc("POST /api/collections/{id}/roms", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "collections.write") {
			return
		}
		m.handleColRoms(w, r, false)
	})

	// DELETE /api/collections/{id}/roms → collections.write; remove (body ON DELETE). 200.
	mux.HandleFunc("DELETE /api/collections/{id}/roms", func(w http.ResponseWriter, r *http.Request) {
		if !requireScopes(w, r, "collections.write") {
			return
		}
		m.handleColRoms(w, r, true)
	})

	return httptest.NewServer(mux)
}

func (m *propsMock) handleColRoms(w http.ResponseWriter, r *http.Request, isDelete bool) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var body struct {
		RomIDs []int `json:"rom_ids"`
	}
	// A DELETE that dropped its body decodes to an empty struct — the received flag +
	// ids prove the body actually arrived on the wire.
	dec := json.NewDecoder(r.Body)
	_ = dec.Decode(&body)
	if isDelete {
		m.deleteBodyReceived = len(body.RomIDs) > 0
		m.deleteBodyRomIDs = body.RomIDs
	}
	col, ok := m.cols[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if id == m.wrongOwnerCol {
		deny(w) // 403: not the user's collection
		return
	}
	for _, rid := range body.RomIDs {
		if isDelete {
			col.RomIDs = removeInt(col.RomIDs, rid)
		} else if !containsInt(col.RomIDs, rid) {
			col.RomIDs = append(col.RomIDs, rid) // dedupe
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // 200, NOT 201
	_ = json.NewEncoder(w).Encode(col)
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func removeInt(xs []int, v int) []int {
	out := xs[:0]
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// propsClient builds a scoped client via the shared testClient helper (devicesync_test.go).
func propsClient(t *testing.T, url, scopes string) *Client {
	t.Helper()
	return testClient(t, url, scopes)
}

// ---- tests ----------------------------------------------------------------------------

func TestSetRomPropsPartialExcludeUnset(t *testing.T) {
	m := newPropsMock()
	srv := m.server()
	defer srv.Close()
	c := propsClient(t, srv.URL, writeBackScopes)

	// (1) set ONLY rating — the body must carry exactly one key.
	if _, err := c.SetRomProps(7, RomUserData{Rating: PtrInt(8)}); err != nil {
		t.Fatalf("set rating: %v", err)
	}
	if len(m.lastKeys) != 1 || !m.lastKeys["rating"] {
		t.Fatalf("exclude_unset broken — body keys = %v, want {rating}", m.lastKeys)
	}
	if m.rows[7].Rating != 8 {
		t.Fatalf("rating not applied: %+v", m.rows[7])
	}
	// (2) set ONLY status — rating from (1) must remain untouched (PATCH semantics).
	if _, err := c.SetRomProps(7, RomUserData{Status: PtrStatus(StatusFinished)}); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if len(m.lastKeys) != 1 || !m.lastKeys["status"] {
		t.Fatalf("second write should carry only {status}, got %v", m.lastKeys)
	}
	if m.rows[7].Rating != 8 || m.rows[7].Status == nil || *m.rows[7].Status != StatusFinished {
		t.Fatalf("partial write clobbered prior state: %+v", m.rows[7])
	}
	// (3) clear status back to null.
	if _, err := c.SetRomProps(7, RomUserData{ClearStatus: true}); err != nil {
		t.Fatalf("clear status: %v", err)
	}
	if m.rows[7].Status != nil {
		t.Fatalf("ClearStatus should null the status, got %v", *m.rows[7].Status)
	}
	// clearing rating = send 0 (non-nullable).
	if _, err := c.SetRomProps(7, RomUserData{Rating: PtrInt(0)}); err != nil {
		t.Fatalf("clear rating: %v", err)
	}
	if m.rows[7].Rating != 0 {
		t.Fatalf("rating 0 not applied: %+v", m.rows[7])
	}
}

func TestSetRomPropsRangeValidation(t *testing.T) {
	m := newPropsMock()
	srv := m.server()
	defer srv.Close()
	c := propsClient(t, srv.URL, writeBackScopes)

	for _, tc := range []struct {
		name string
		data RomUserData
	}{
		{"rating>10", RomUserData{Rating: PtrInt(11)}},
		{"difficulty>10", RomUserData{Difficulty: PtrInt(11)}},
		{"completion>100", RomUserData{Completion: PtrInt(101)}},
	} {
		if _, err := c.SetRomProps(7, tc.data); err == nil {
			t.Fatalf("%s: expected 422, got nil", tc.name)
		} else if IsAuthError(err) {
			t.Fatalf("%s: 422 must not read as auth error", tc.name)
		}
	}
	// in-range values are accepted.
	if _, err := c.SetRomProps(7, RomUserData{Rating: PtrInt(10), Completion: PtrInt(100), Difficulty: PtrInt(0)}); err != nil {
		t.Fatalf("in-range write rejected: %v", err)
	}
}

func TestSetRomPropsStatusEnumRejected(t *testing.T) {
	m := newPropsMock()
	srv := m.server()
	defer srv.Close()
	c := propsClient(t, srv.URL, writeBackScopes)

	// The client's own guard should never send a bad status (ValidRomUserStatus), but a
	// raw hand-built body must be rejected by the server as 422 — prove the wire rule.
	if _, err := c.SetRomProps(7, RomUserData{Status: PtrStatus(RomUserStatus("bogus"))}); err == nil {
		t.Fatalf("bogus status should 422, got nil")
	}
	for _, s := range []RomUserStatus{StatusIncomplete, StatusFinished, StatusCompleted100, StatusRetired, StatusNeverPlaying} {
		if _, err := c.SetRomProps(7, RomUserData{Status: PtrStatus(s)}); err != nil {
			t.Fatalf("valid status %q rejected: %v", s, err)
		}
	}
}

func TestSetRomPropsScope403(t *testing.T) {
	m := newPropsMock()
	srv := m.server()
	defer srv.Close()
	// token WITHOUT roms.user.write.
	c := propsClient(t, srv.URL, "roms.user.read,collections.read")
	if _, err := c.SetRomProps(7, RomUserData{Rating: PtrInt(5)}); err == nil {
		t.Fatalf("props without roms.user.write should 403, got nil")
	} else if IsAuthError(err) {
		t.Fatalf("scope 403 must not read as an auth/pairing-expired error: %v", err)
	}
}

func TestFavouritesDiscoverExisting(t *testing.T) {
	m := newPropsMock()
	m.cols[10] = &Collection{ID: 10, Name: "Favourites", IsFavorite: true}
	m.cols[11] = &Collection{ID: 11, Name: "RPGs"}
	srv := m.server()
	defer srv.Close()
	c := propsClient(t, srv.URL, writeBackScopes)

	id, err := c.GetFavouritesCollectionID()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if id != 10 {
		t.Fatalf("discovered wrong favourites id: %d", id)
	}
	if len(m.cols) != 2 {
		t.Fatalf("discovery must NOT create a collection: %d present", len(m.cols))
	}
}

func TestFavouritesCreateWhenAbsent(t *testing.T) {
	m := newPropsMock()
	srv := m.server()
	defer srv.Close()
	c := propsClient(t, srv.URL, writeBackScopes)

	id, err := c.GetFavouritesCollectionID()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	col, ok := m.cols[id]
	if !ok || !col.IsFavorite || col.Name != "Favourites" {
		t.Fatalf("create did not make an is_favorite Favourites collection: %+v", m.cols)
	}
}

func TestFavouritesCreateGetConfirmFallback(t *testing.T) {
	m := newPropsMock()
	m.createEchoesFavorite = false // server omits is_favorite on the create response
	srv := m.server()
	defer srv.Close()
	c := propsClient(t, srv.URL, writeBackScopes)

	// The create-response is_favorite is false, so the client MUST fall back to the
	// GET-confirm re-read to resolve the id (the contract's confirm step).
	id, err := c.GetFavouritesCollectionID()
	if err != nil {
		t.Fatalf("create+confirm: %v", err)
	}
	if col := m.cols[id]; col == nil || !col.IsFavorite {
		t.Fatalf("GET-confirm fallback failed to resolve the favourites collection")
	}
}

func TestAddAndRemoveRomFavorite(t *testing.T) {
	m := newPropsMock()
	m.cols[10] = &Collection{ID: 10, Name: "Favourites", IsFavorite: true}
	srv := m.server()
	defer srv.Close()
	c := propsClient(t, srv.URL, writeBackScopes)

	// add (idempotent — add twice, one member).
	if _, err := c.AddRomToCollection(10, 42); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := c.AddRomToCollection(10, 42); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if got := m.cols[10].RomIDs; len(got) != 1 || got[0] != 42 {
		t.Fatalf("add not idempotent: %v", got)
	}

	// remove — the DELETE MUST transmit its body.
	if _, err := c.RemoveRomFromCollection(10, 42); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !m.deleteBodyReceived {
		t.Fatalf("DELETE body was NOT received — the client dropped the request body")
	}
	if len(m.deleteBodyRomIDs) != 1 || m.deleteBodyRomIDs[0] != 42 {
		t.Fatalf("DELETE body rom_ids wrong: %v", m.deleteBodyRomIDs)
	}
	if len(m.cols[10].RomIDs) != 0 {
		t.Fatalf("remove did not drop the member: %v", m.cols[10].RomIDs)
	}
	// remove-absent is a no-op (still succeeds).
	if _, err := c.RemoveRomFromCollection(10, 999); err != nil {
		t.Fatalf("remove-absent should be a no-op, got: %v", err)
	}
}

func TestFavoriteWrongOwner403(t *testing.T) {
	m := newPropsMock()
	m.cols[10] = &Collection{ID: 10, Name: "Favourites", IsFavorite: true}
	m.wrongOwnerCol = 10
	srv := m.server()
	defer srv.Close()
	c := propsClient(t, srv.URL, writeBackScopes)

	if _, err := c.AddRomToCollection(10, 42); err == nil {
		t.Fatalf("add to a collection that isn't the user's should 403")
	} else if IsAuthError(err) {
		t.Fatalf("ownership 403 must not read as an auth error: %v", err)
	}
}

func TestFavouritesScope403(t *testing.T) {
	m := newPropsMock()
	m.cols[10] = &Collection{ID: 10, Name: "Favourites", IsFavorite: true}
	srv := m.server()
	defer srv.Close()
	// token WITHOUT collections.write.
	c := propsClient(t, srv.URL, "collections.read,roms.user.write")
	if _, err := c.AddRomToCollection(10, 42); err == nil {
		t.Fatalf("add without collections.write should 403, got nil")
	} else if IsAuthError(err) {
		t.Fatalf("scope 403 must not read as an auth error: %v", err)
	}
}
