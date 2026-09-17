package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/iammatthias/farfield/lib/auth"
)

// Authoring: the compose form, creating a paste, and managing the ones that
// exist. Session-gated.

func (s *Server) handleCompose(w http.ResponseWriter, r *http.Request) {
	s.renderCompose(w, http.StatusOK, map[string]any{"Visibility": VisUnlisted})
}

func (s *Server) renderCompose(w http.ResponseWriter, status int, form map[string]any) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	form["Langs"] = composeLangs
	form["Expiries"] = expiryChoices
	s.rd.Render(w, "compose.html", form)
}

// handleCreate is the browser compose POST.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPasteBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := r.FormValue("body")
	form := map[string]any{
		"Body":       body,
		"Title":      strings.TrimSpace(r.FormValue("title")),
		"Lang":       strings.ToLower(strings.TrimSpace(r.FormValue("lang"))),
		"Visibility": r.FormValue("visibility"),
		"Expires":    r.FormValue("expires"),
	}
	if strings.TrimSpace(body) == "" {
		form["Error"] = "Body is required — paste something."
		s.renderCompose(w, http.StatusBadRequest, form)
		return
	}
	tokenSecret := strings.TrimSpace(r.FormValue("token"))
	if r.FormValue("token_generate") == "on" {
		tokenSecret = auth.NewSessionToken()
	}
	p, err := s.createPaste(body, form["Title"].(string), form["Lang"].(string),
		r.FormValue("visibility"), r.FormValue("expires"), tokenSecret)
	if err != nil {
		form["Error"] = err.Error()
		s.renderCompose(w, http.StatusBadRequest, form)
		return
	}
	if tokenSecret != "" {
		// The token is hashed at rest and unrecoverable, and the author's
		// session bypasses the gate on /{id} — a redirect would render the
		// paste normally and the secret would be gone. Surface it exactly
		// once, on a server-rendered confirmation, before sending them on.
		s.rd.Render(w, "created.html", map[string]any{
			"ID":        p.ID,
			"Title":     p.Title,
			"PasteURL":  s.publicURL + "/" + p.ID,
			"MagicLink": s.magicLink(p.ID, tokenSecret),
			"Token":     tokenSecret,
		})
		return
	}
	http.Redirect(w, r, "/"+p.ID, http.StatusSeeOther)
}

// magicLink builds the shareable unlock URL. The secret rides the fragment so
// it never reaches server logs; PathEscape (never QueryEscape, whose "+" for
// space survives decodeURIComponent as a literal plus) keeps the unlock
// shell's decode exact for typed passphrases.
func (s *Server) magicLink(id, secret string) string {
	return s.publicURL + "/" + id + "#t=" + url.PathEscape(secret)
}

// createPaste normalizes, upserts, and applies token + visibility rules —
// the shared core of the browser and API create paths.
func (s *Server) createPaste(body, title, lang, visibility, expires, tokenSecret string) (*Paste, error) {
	if !validVisibility(visibility) {
		visibility = VisUnlisted
	}
	expiresAt, err := parseExpiry(strings.TrimSpace(expires))
	if err != nil {
		return nil, fmt.Errorf("expiry must be one of %s", strings.Join(expiryChoices, ", "))
	}
	short, full := pasteID(body)

	// A token (newly set here, or already on the existing row for this same
	// content) forces visibility down to at least unlisted — a locked paste
	// must never advertise itself on the public index.
	hasToken := tokenSecret != ""
	if !hasToken {
		if existing, err := getPaste(s.db, short); err != nil {
			return nil, err
		} else if existing != nil && existing.HasToken {
			hasToken = true
		}
	}
	if hasToken && visibility == VisPublic {
		visibility = VisUnlisted
	}

	p := &Paste{
		ID:         short,
		CID:        full,
		Title:      title,
		Lang:       lang,
		Body:       body,
		Visibility: visibility,
		ExpiresAt:  expiresAt,
	}
	if err := upsertPaste(s.db, p); err != nil {
		return nil, err
	}
	if tokenSecret != "" {
		if err := setToken(s.db, p.ID, tokenSecret, ""); err != nil {
			return nil, err
		}
	}
	p.HasToken = hasToken
	return p, nil
}

func (s *Server) handleManage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	visibility := q.Get("visibility")
	if !validVisibility(visibility) {
		visibility = ""
	}
	lang := strings.ToLower(strings.TrimSpace(q.Get("lang")))
	search := strings.TrimSpace(q.Get("q"))

	ps, err := listManagePastes(s.db, visibility, lang, search)
	if err != nil {
		slog.Error("list pastes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	langs, err := distinctLangs(s.db)
	if err != nil {
		slog.Error("list langs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total, err := countPastes(s.db)
	if err != nil {
		slog.Error("count pastes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.rd.Render(w, "manage.html", map[string]any{
		"Pastes":     ps,
		"Langs":      langs,
		"Total":      total,
		"Shown":      len(ps),
		"Visibility": visibility,
		"Lang":       lang,
		"Q":          search,
	})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := deletePaste(s.db, r.PathValue("id")); err != nil {
		slog.Error("delete paste", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/manage", http.StatusSeeOther)
}

func (s *Server) handleDeleteExpired(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteExpiredPastes(s.db); err != nil {
		slog.Error("delete expired", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/manage", http.StatusSeeOther)
}
