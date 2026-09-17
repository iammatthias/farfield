package main

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// Series: reusable body fragments spliced into entries by series:// reference.
// Managed exactly like entries, with their own admin pages and API.

func (s *Server) handleSeriesList(w http.ResponseWriter, r *http.Request) {
	series, err := listSeries(s.db)
	if err != nil {
		s.fail(w, "list series", err)
		return
	}
	s.rd.Render(w, "series.html", map[string]any{"Series": series})
}

func (s *Server) handleNewSeries(w http.ResponseWriter, r *http.Request) {
	s.renderSeriesForm(w, r, &Series{}, true, "/series", "")
}

func (s *Server) handleCreateSeries(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	title := strings.TrimSpace(r.FormValue("title"))
	se := &Series{
		Slug:  slugify(web.FirstNonEmpty(r.FormValue("slug"), title)),
		Title: title,
		Body:  r.FormValue("body"),
	}
	if se.Slug == "" {
		s.seriesSaveError(w, r, se, true, "/series", "A series needs a slug or a title.")
		return
	}
	if existing, _ := getSeries(s.db, se.Slug); existing != nil {
		s.seriesSaveError(w, r, se, true, "/series", "That slug is already taken.")
		return
	}
	now := store.NowRFC3339()
	se.CreatedAt, se.UpdatedAt = now, now
	if err := upsertSeries(s.db, se); err != nil {
		s.fail(w, "create series", err)
		return
	}
	s.seriesSaved(w, r, se)
}

func (s *Server) handleEditSeries(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get series", err)
		return
	}
	if se == nil {
		http.NotFound(w, r)
		return
	}
	s.renderSeriesForm(w, r, se, false, "/series/"+se.Slug, "")
}

func (s *Server) handleUpdateSeries(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get series", err)
		return
	}
	if se == nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	se.Title = strings.TrimSpace(r.FormValue("title"))
	se.Body = r.FormValue("body")
	se.UpdatedAt = store.NowRFC3339()
	if err := upsertSeries(s.db, se); err != nil {
		s.fail(w, "update series", err)
		return
	}
	s.seriesSaved(w, r, se)
}

// seriesSaved and seriesSaveError mirror the entry save responses for the
// series form's async saves.
func (s *Server) seriesSaved(w http.ResponseWriter, r *http.Request, se *Series) {
	if web.WantsJSON(r) {
		web.WriteJSON(w, http.StatusOK, map[string]any{
			"slug":    se.Slug,
			"action":  "/series/" + se.Slug,
			"editURL": "/series/" + se.Slug + "/edit",
		})
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) seriesSaveError(w http.ResponseWriter, r *http.Request, se *Series, isNew bool, action, msg string) {
	if web.WantsJSON(r) {
		web.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	s.renderSeriesForm(w, r, se, isNew, action, msg)
}

func (s *Server) handleDeleteSeries(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteSeries(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "delete series", err)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) renderSeriesForm(w http.ResponseWriter, r *http.Request, se *Series, isNew bool, action, errMsg string) {
	s.rd.Render(w, "series_form.html", map[string]any{
		"Series": se, "IsNew": isNew, "Action": action, "Error": errMsg,
		"BlobsPublic": s.blobsPublic, "ContentPublic": s.contentPublic,
		"BodyHTML": s.bodyHTML(r, se.Body), "Words": wordCount(se.Body),
	})
}

// Series: reusable body fragments spliced into entries by series:// reference.
// Managed exactly like entries, with their own admin pages and API.

func (s *Server) handleAPISeries(w http.ResponseWriter, r *http.Request) {
	fp, err := seriesFingerprint(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list series")
		return
	}
	if listETagDone(w, r, "series|"+fp) {
		return
	}
	series, err := listSeries(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list series")
		return
	}
	if series == nil {
		series = []Series{}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"series": series})
}

func (s *Server) handleAPISeriesOne(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("slug"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read series")
		return
	}
	if se == nil {
		web.WriteError(w, http.StatusNotFound, "series not found")
		return
	}
	web.WriteRecord(w, r, se.CID, se)
}

// sweepLoop applies the retention promises hourly, not just at boot.
//
// purgeTrash and PruneSessions ran once at startup. A container that stays up
// for weeks — which is the normal case here, uptime was nine days at the last
// reboot — therefore stopped honouring them the moment it finished booting:
// the trash page says entries are purged after 30 days, and an entry trashed
// on day two of an uptime sat there indefinitely. Same for expired sessions.
// The retention window was real only for a process that kept restarting.
func (s *Server) sweepLoop() {
	for {
		if err := purgeTrash(s.db); err != nil {
			slog.Warn("trash purge failed", "err", err)
		}
		if err := store.PruneSessions(s.db); err != nil {
			slog.Warn("session prune failed", "err", err)
		}
		time.Sleep(time.Hour)
	}
}
