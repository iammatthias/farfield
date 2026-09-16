package main

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

// The public side: viewing a paste, its raw bytes, and the unlock form a
// magic link lands on.

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, res := s.gate(r, id)
	switch res {
	case gateOK:
	case gateNotFound:
		http.NotFound(w, r)
		return
	case gateGone:
		w.WriteHeader(http.StatusGone)
		s.rd.Render(w, "gone.html", nil)
		return
	case gateNeedsLogin:
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case gateLocked:
		s.renderLocked(w, id, http.StatusUnauthorized, "")
		return
	case gateForbidden:
		s.renderLocked(w, id, http.StatusForbidden, "Wrong token.")
		return
	case gateLimited:
		s.renderLocked(w, id, http.StatusTooManyRequests,
			"Too many attempts. Wait a minute.")
		return
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	code, err := highlightHTML(p.Body, p.Lang)
	if err != nil {
		slog.Error("highlight", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := incrementViews(s.db, p.ID); err != nil {
		slog.Warn("could not count view", "err", err)
	} else {
		p.Views++
	}
	lang := p.Lang
	if lang == "" || !knownLang(lang) {
		lang = "plain"
	}
	s.rd.Render(w, "view.html", map[string]any{
		"Paste":     p,
		"Lang":      lang,
		"Code":      code,
		"ChromaCSS": s.chromaCSS,
		"Authed":    s.sessionValid(r),
	})
}

func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	p, res := s.gate(r, r.PathValue("id"))
	switch res {
	case gateOK:
	case gateNotFound:
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return
	case gateGone:
		web.WriteError(w, http.StatusGone, "paste expired")
		return
	case gateNeedsLogin:
		web.WriteError(w, http.StatusUnauthorized, "session required")
		return
	case gateLocked:
		web.WriteError(w, http.StatusUnauthorized, "token required")
		return
	case gateForbidden:
		web.WriteError(w, http.StatusForbidden, "wrong token")
		return
	case gateLimited:
		web.WriteError(w, http.StatusTooManyRequests, "too many token attempts")
		return
	default:
		web.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := incrementViews(s.db, p.ID); err != nil {
		slog.Warn("could not count view", "err", err)
	}
	w.Header().Set("ETag", `"`+p.CID+`"`)
	if web.ETagMatch(r, p.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, p.Body)
}

// handleUnlock accepts a typed (or magic-link-posted) token, sets a per-paste
// cookie on success, and bounces back to the view.
func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	p, err := getPaste(s.db, id)
	if err != nil {
		slog.Error("get paste", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.NotFound(w, r)
		return
	}
	if expired(p) {
		if _, err := deletePaste(s.db, p.ID); err != nil {
			slog.Warn("could not delete expired paste", "id", p.ID, "err", err)
		}
		w.WriteHeader(http.StatusGone)
		s.rd.Render(w, "gone.html", nil)
		return
	}
	if p.Visibility == VisPrivate && !s.sessionValid(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !p.HasToken {
		http.Redirect(w, r, "/"+id, http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	token := r.FormValue("token")
	key := web.ClientIP(r) + "|" + id
	if s.limiter.Blocked(key) {
		s.renderLocked(w, id, http.StatusTooManyRequests,
			"Too many attempts. Wait a minute.")
		return
	}
	ok, err := verifyToken(s.db, id, token)
	if err != nil {
		slog.Error("verify token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		s.limiter.Fail(key)
		s.renderLocked(w, id, http.StatusForbidden, "Wrong token.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "scrap_t",
		Value:    token,
		Path:     "/" + id,
		HttpOnly: true,
		Secure:   s.auth.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 7,
	})
	http.Redirect(w, r, "/"+id, http.StatusSeeOther)
}

// renderLocked serves the unlock shell. It deliberately receives only the id
// — no paste fields — so a locked page cannot leak title, lang, or body.
func (s *Server) renderLocked(w http.ResponseWriter, id string, status int, errMsg string) {
	w.WriteHeader(status)
	s.rd.Render(w, "locked.html", map[string]any{
		"ID":    id,
		"Error": errMsg,
	})
}

// handlePublicIndex lists public, unexpired pastes — title/lang/age only.
func (s *Server) handlePublicIndex(w http.ResponseWriter, r *http.Request) {
	ps, err := listPublicPastes(s.db)
	if err != nil {
		slog.Error("list public pastes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.rd.Render(w, "index.html", map[string]any{
		"Pastes": ps,
		"Authed": s.sessionValid(r),
	})
}
