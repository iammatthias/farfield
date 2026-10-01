package main

// A key's permalink: its facts, its usage rollups, and the controls to
// rename, re-date, rotate, revoke or delete it.

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/keys"
)

const (
	usageDays   = 30 // the per-day strip
	usageRecent = 15 // the recent-routes table
)

func keyURL(id string) string { return "/keys/" + url.PathEscape(id) }

// dayView is one day of the strip, with bar heights as percentages of the
// busiest day.
type dayView struct {
	keys.DayUsage
	Total      int64
	OKPct      int
	RefusedPct int
}

func dayViews(days []keys.DayUsage) []dayView {
	var peak int64
	for _, d := range days {
		peak = max(peak, d.OK+d.Refused)
	}
	out := make([]dayView, len(days))
	for i, d := range days {
		v := dayView{DayUsage: d, Total: d.OK + d.Refused}
		if peak > 0 {
			v.OKPct = int(d.OK * 100 / peak)
			v.RefusedPct = int(d.Refused * 100 / peak)
			// keep any nonzero count visible
			if d.OK > 0 && v.OKPct < 4 {
				v.OKPct = 4
			}
			if d.Refused > 0 && v.RefusedPct < 4 {
				v.RefusedPct = 4
			}
		}
		out[i] = v
	}
	return out
}

func (s *Server) handleKey(w http.ResponseWriter, r *http.Request) {
	s.renderKey(w, r, r.PathValue("id"), "")
}

// renderKey renders a key's page, with errMsg above the controls when a
// form on it was refused.
func (s *Server) renderKey(w http.ResponseWriter, r *http.Request, id, errMsg string) {
	k, err := s.ks.Get(id)
	if err != nil {
		s.fail(w, "get key", err)
		return
	}
	if k == nil {
		http.NotFound(w, r)
		return
	}
	u, err := s.ks.Usage(k.ID, usageDays, usageRecent)
	if err != nil {
		s.fail(w, "key usage", err)
		return
	}
	var active []dayView
	for _, d := range dayViews(u.Days) {
		if d.Total > 0 {
			active = append(active, d)
		}
	}
	// newest first for the table
	for i, j := 0, len(active)-1; i < j; i, j = i+1, j-1 {
		active[i], active[j] = active[j], active[i]
	}
	s.rd.Render(w, "key.html", map[string]any{
		"Key":        keyView(*k),
		"Usage":      u,
		"Days":       dayViews(u.Days),
		"ActiveDays": active,
		"Error":      errMsg,
		"Today":      time.Now().UTC().Format("2006-01-02"),
	})
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.renderKey(w, r, id, "A key needs a name.")
		return
	}
	ok, err := s.ks.Rename(id, name)
	if err != nil {
		s.fail(w, "rename key", err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, keyURL(id), http.StatusSeeOther)
}

// handleExpiry sets the expiry to the end of the chosen UTC day, or clears
// it. A date in the past is refused — revoke is the way to stop a key now.
func (s *Server) handleExpiry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var expires time.Time
	if r.FormValue("action") != "clear" {
		day, err := time.Parse("2006-01-02", r.FormValue("expires"))
		if err != nil {
			s.renderKey(w, r, id, "Pick an expiry date, or choose Never.")
			return
		}
		expires = day.Add(24*time.Hour - time.Second)
		if !expires.After(time.Now()) {
			s.renderKey(w, r, id, "The expiry must be today or later — to stop the key now, revoke it.")
			return
		}
	}
	ok, err := s.ks.SetExpiry(id, expires)
	if err != nil {
		s.fail(w, "set expiry", err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, keyURL(id), http.StatusSeeOther)
}

// handleRotate swaps the key for a fresh one on the same terms and reveals
// the new token exactly once, the way the create flow does.
func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, err := s.ks.Get(id)
	if err != nil {
		s.fail(w, "get key", err)
		return
	}
	token, k, err := s.ks.Rotate(id)
	if errors.Is(err, keys.ErrNotActive) {
		s.renderKey(w, r, id, "Only an active key can be rotated — extend its expiry or issue a new one.")
		return
	}
	if err != nil {
		s.fail(w, "rotate key", err)
		return
	}
	if k == nil || old == nil {
		http.NotFound(w, r)
		return
	}
	s.rd.Render(w, "created.html", map[string]any{
		"Token":    token,
		"Key":      keyView(*k),
		"Replaced": keyView(*old),
	})
}
