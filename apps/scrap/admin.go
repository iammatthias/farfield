package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/web"
)

// The private admin API (web.PrivateAPI): the manage table over JSON. Unlike
// the public view paths it reads a paste as stored — no token needed, no
// view counted, and an expired paste is reported (with its expiresAt), not
// lazily deleted, since an admin reading the list is not a reader.

func (s *Server) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/pastes", s.auth.PrivateAPI(s.handleAdminList))
	mux.HandleFunc("GET /api/admin/pastes/{id}", s.auth.PrivateAPI(s.handleAdminGet))
	mux.HandleFunc("PUT /api/admin/pastes/{id}", s.auth.PrivateAPI(s.handleAdminUpdate))
	mux.HandleFunc(web.AdminPrefix, s.auth.PrivateAPI(web.AdminNotFound))
}

// Admin list paging: a default page that fits a screen, and a cap so one
// request cannot ask for the whole table.
const (
	adminPageDefault = 50
	adminPageMax     = 200
)

func (s *Server) handleAdminList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := adminPageDefault
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, adminPageMax)
	}
	page := 1
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 1 {
		page = n
	}
	ps, total, err := listAdminPastes(s.db, limit, page)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list pastes")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"pastes": ps, "total": total})
}

// handleAdminGet returns a paste with its body. The ETag hashes the whole
// record, not the paste's CID: the CID covers the body alone, and the
// metadata the admin edits (title, visibility, expiry) must move the tag too.
func (s *Server) handleAdminGet(w http.ResponseWriter, r *http.Request) {
	p := s.adminPaste(w, r)
	if p == nil {
		return
	}
	web.WriteRecord(w, r, cid.OfValue(p), p)
}

// pasteUpdate is the admin PUT body. Every field is optional; an absent one
// keeps its stored value.
type pasteUpdate struct {
	Title      *string `json:"title"`
	Lang       *string `json:"lang"`
	Visibility *string `json:"visibility"`
	Expires    *string `json:"expires"`
}

// handleAdminUpdate edits a paste's metadata under the rules create applies:
// title and lang are trimmed (lang lowercased), an unknown visibility falls
// back to unlisted, an unknown expiry is refused, and a paste with a view
// token can never be public — it is forced to unlisted, as at create. An
// expiry is relative to now, the same choices the compose form offers.
func (s *Server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	p := s.adminPaste(w, r)
	if p == nil {
		return
	}
	var in pasteUpdate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.Title != nil {
		p.Title = strings.TrimSpace(*in.Title)
	}
	if in.Lang != nil {
		p.Lang = strings.ToLower(strings.TrimSpace(*in.Lang))
	}
	if in.Visibility != nil {
		p.Visibility = *in.Visibility
		if !validVisibility(p.Visibility) {
			p.Visibility = VisUnlisted
		}
	}
	if in.Expires != nil {
		at, err := parseExpiry(strings.TrimSpace(*in.Expires))
		if err != nil {
			web.WriteError(w, http.StatusBadRequest,
				fmt.Sprintf("expiry must be one of %s", strings.Join(expiryChoices, ", ")))
			return
		}
		p.ExpiresAt = at
	}
	if p.HasToken && p.Visibility == VisPublic {
		p.Visibility = VisUnlisted
	}
	found, err := updatePasteMeta(s.db, p)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not update paste")
		return
	}
	if !found {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return
	}
	web.WriteSaved(w, http.StatusOK, cid.OfValue(p), p)
}

// adminPaste loads the {id} paste as stored, writing the 404 or 500 itself
// and returning nil when there is nothing to serve.
func (s *Server) adminPaste(w http.ResponseWriter, r *http.Request) *Paste {
	id := r.PathValue("id")
	if !validID(id) {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return nil
	}
	p, err := getPaste(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read paste")
		return nil
	}
	if p == nil {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return nil
	}
	return p
}
