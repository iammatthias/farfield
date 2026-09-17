package main

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/web"
)

// The terminal API: list, delete, and share, keyed rather than session-gated,
// so the `sideload` CLI and an agent can drive distribution without a browser.

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	builds, err := listBuilds(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list builds")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"builds": builds})
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		web.WriteError(w, http.StatusNotFound, "build not found")
		return
	}
	b, err := getBuild(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if b == nil {
		web.WriteError(w, http.StatusNotFound, "build not found")
		return
	}
	if _, err := deleteBuild(s.db, id); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete build")
		return
	}
	if err := s.blobs.remove(b.CID, ".ipa"); err != nil {
		slog.Warn("could not remove blob", "cid", b.CID, "err", err)
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleAPIAppDelete removes an entire app (every version) by bundle id.
func (s *Server) handleAPIAppDelete(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	cids, shots, n, err := deleteApp(s.db, bundle)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete app")
		return
	}
	if n == 0 {
		web.WriteError(w, http.StatusNotFound, "app not found")
		return
	}
	s.removeAppFiles(cids, shots)
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": bundle, "versions": n})
}

func (s *Server) handleAPIShare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBuild(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if b == nil {
		web.WriteError(w, http.StatusNotFound, "build not found")
		return
	}
	q := r.URL.Query()
	ttl := parseTTL(q.Get("ttl"))
	max := parseMaxInstalls(q.Get("max"))
	tok, err := createShare(s.db, b.ID, ttl, max, strings.TrimSpace(q.Get("label")))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not create share")
		return
	}
	web.WriteJSON(w, http.StatusCreated, map[string]any{
		"token":       tok.Token,
		"shareURL":    s.publicURL + "/s/" + tok.Token,
		"expiresAt":   tok.ExpiresAt,
		"maxInstalls": max,
	})
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countBuilds(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "sideload",
		"ok":      true,
		"builds":  n,
	})
}
