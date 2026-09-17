package main

import (
	"log/slog"
	"net/http"
	"strings"
)

// Share links: a revocable, expiring URL that hands someone a build without
// giving them the console.

func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBuild(s.db, id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	ttl := parseTTL(r.FormValue("ttl"))
	max := parseMaxInstalls(r.FormValue("max"))
	label := strings.TrimSpace(r.FormValue("label"))

	tok, err := createShare(s.db, b.ID, ttl, max, label)
	if err != nil {
		slog.Error("create share", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.rd.Render(w, "created.html", map[string]any{
		"Build":     b,
		"ShareURL":  s.publicURL + "/s/" + tok.Token,
		"ExpiresAt": tok.ExpiresAt,
		"Max":       max,
		"Label":     label,
	})
}

func (s *Server) handleShares(w http.ResponseWriter, r *http.Request) {
	shares, err := listShares(s.db)
	if err != nil {
		slog.Error("list shares", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Compute an effective live/dead label for the table.
	type row struct {
		shareRow
		Live bool
		URL  string
	}
	rows := make([]row, 0, len(shares))
	for _, sh := range shares {
		rows = append(rows, row{shareRow: sh, Live: sh.canStart(), URL: s.publicURL + "/s/" + sh.Token.Token})
	}
	s.rd.Render(w, "shares.html", map[string]any{"Shares": rows})
}

func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	if _, err := revokeToken(s.db, r.PathValue("token")); err != nil {
		slog.Error("revoke token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/shares", http.StatusSeeOther)
}

// handleShareLanding is the public share page — no session. It warns up front
// that only enrolled devices can install, then offers the one-tap link.
func (s *Server) handleShareLanding(w http.ResponseWriter, r *http.Request) {
	tok, code := s.loadToken(r)
	if code != http.StatusOK || tok.Kind != kindShare || !tok.canStart() {
		status := http.StatusGone
		if code == http.StatusNotFound || tok == nil || tok.Kind != kindShare {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		s.rd.Render(w, "gone.html", nil)
		return
	}
	b, err := getBuild(s.db, tok.BuildID)
	if err != nil || b == nil {
		w.WriteHeader(http.StatusGone)
		s.rd.Render(w, "gone.html", nil)
		return
	}
	// Rich content is best-effort on a public page — never fail the install over it.
	meta, shots, err := s.loadAppContent(b.BundleID)
	if err != nil {
		slog.Warn("share content", "err", err)
	}
	s.rd.Render(w, "share.html", map[string]any{
		"Build":       b,
		"Expiry":      expiryView(b),
		"InstallURL":  s.installLink(tok.Token),
		"Label":       tok.Label,
		"Meta":        meta,
		"Screenshots": shots,
	})
}
