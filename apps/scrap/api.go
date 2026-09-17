package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/web"
)

// The terminal API — create, delete, and token lifecycle over an API key, so
// `ff-scrap` and agents can paste without a browser.

// handleAPICreate accepts a raw text body (any Content-Type) and returns the
// paste URL as text/plain — pipe-clean:
//
//	cat x.go | curl --data-binary @- -H "X-API-Key: $K" \
//	    "https://scrap.../api/pastes?lang=go&expires=1d&token=generate"
func (s *Server) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPasteBytes))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(string(body)) == "" {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	tokenParam := q.Get("token")
	tokenSecret := tokenParam
	generated := tokenParam == "generate"
	if generated {
		tokenSecret = auth.NewSessionToken()
	}
	p, err := s.createPaste(string(body),
		strings.TrimSpace(q.Get("title")),
		strings.ToLower(strings.TrimSpace(q.Get("lang"))),
		q.Get("visibility"), q.Get("expires"), tokenSecret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintln(w, s.publicURL+"/"+p.ID)
	if generated {
		fmt.Fprintln(w, "token: "+tokenSecret)
	}
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return
	}
	existed, err := deletePaste(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete paste")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleAPITokenRoll is the terminal twin of the manage-view roll: replace
// the existing token and print the fresh secret, pipe-clean.
func (s *Server) handleAPITokenRoll(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	if !p.HasToken {
		web.WriteError(w, http.StatusConflict, "no token set")
		return
	}
	secret, err := s.freshToken(p)
	if err != nil {
		slog.Error("roll token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not roll token")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "token: "+secret)
}

// handleAPITokenRemove deletes a paste's token(s) over the terminal API.
func (s *Server) handleAPITokenRemove(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	existed, err := deleteTokens(s.db, p.ID)
	if err != nil {
		slog.Error("remove token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not remove token")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "no token set")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"tokenRemoved": p.ID})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countPastes(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "scrap",
		"ok":      true,
		"pastes":  n,
	})
}
