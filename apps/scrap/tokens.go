package main

import (
	"log/slog"
	"net/http"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/web"
)

// Magic-link tokens: mint, roll, set, remove. A token is stored hashed, so the
// secret is shown once at creation and never again — rolling is the recovery
// path, not lookup.

// livePaste loads an unexpired paste for a token-lifecycle handler, or
// returns nil after writing the 404/410. Errors are JSON (web.WriteError) on
// both surfaces — these are POST/DELETE endpoints, not pages.
func (s *Server) livePaste(w http.ResponseWriter, id string) *Paste {
	if !validID(id) {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return nil
	}
	p, err := getPaste(s.db, id)
	if err != nil {
		slog.Error("get paste", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "internal error")
		return nil
	}
	if p == nil {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return nil
	}
	if expired(p) {
		if _, err := deletePaste(s.db, p.ID); err != nil {
			slog.Warn("could not delete expired paste", "id", p.ID, "err", err)
		}
		web.WriteError(w, http.StatusGone, "paste expired")
		return nil
	}
	return p
}

// freshToken generates a new 26-char secret and installs it as the paste's
// one token (cap-1 set semantics — any previous secret stops working
// immediately). A public paste is forced down to unlisted, same as create.
func (s *Server) freshToken(p *Paste) (string, error) {
	if p.Visibility == VisPublic {
		if err := setVisibility(s.db, p.ID, VisUnlisted); err != nil {
			return "", err
		}
		p.Visibility = VisUnlisted
	}
	secret := auth.NewSessionToken()
	if err := setToken(s.db, p.ID, secret, ""); err != nil {
		return "", err
	}
	p.HasToken = true
	return secret, nil
}

// renderTokenConfirmation reuses the shown-once create confirmation for a
// rolled/set token — the secret is hashed at rest, so this page is the only
// time it is ever visible.
func (s *Server) renderTokenConfirmation(w http.ResponseWriter, p *Paste, label, secret string) {
	s.rd.Render(w, "created.html", map[string]any{
		"Label":     label,
		"ID":        p.ID,
		"Title":     p.Title,
		"PasteURL":  s.publicURL + "/" + p.ID,
		"MagicLink": s.magicLink(p.ID, secret),
		"Token":     secret,
	})
}

// handleTokenRoll replaces an existing token with a fresh secret.
func (s *Server) handleTokenRoll(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	if !p.HasToken {
		web.WriteError(w, http.StatusConflict, "no token set — add one instead")
		return
	}
	secret, err := s.freshToken(p)
	if err != nil {
		slog.Error("roll token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not roll token")
		return
	}
	s.renderTokenConfirmation(w, p, "Token rolled", secret)
}

// handleTokenSet attaches a token to a paste that has none.
func (s *Server) handleTokenSet(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	if p.HasToken {
		web.WriteError(w, http.StatusConflict, "token already set — roll it instead")
		return
	}
	secret, err := s.freshToken(p)
	if err != nil {
		slog.Error("set token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not set token")
		return
	}
	s.renderTokenConfirmation(w, p, "Token set", secret)
}

// handleTokenRemove deletes the token row(s); the paste serves per its
// visibility again.
func (s *Server) handleTokenRemove(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	if _, err := deleteTokens(s.db, p.ID); err != nil {
		slog.Error("remove token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not remove token")
		return
	}
	http.Redirect(w, r, "/manage", http.StatusSeeOther)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}
