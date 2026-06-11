package main

import (
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// sparkChecks is how many recent checks feed each overview sparkline.
const sparkChecks = 50

// Server holds the running pulse service.
type Server struct {
	db   *sql.DB
	auth *web.Auth
	rd   *web.Renderer
}

// run wires up the service, starts the checker and collector loops, and
// serves until interrupted.
func run(host, port string) error {
	dbPath := store.Env("PULSE_DB_PATH", "pulse.sqlite")
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	tmpl, err := web.ParseTemplates(assets, nil)
	if err != nil {
		return err
	}

	s := &Server{
		db: db,
		auth: &web.Auth{
			DB:           db,
			Password:     store.Env("PASSWORD", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd: &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
	}

	startChecker(db)
	// PULSE_COLLECT_INTERVAL takes a Go duration; "0" disables the collector
	// (the checker has no traffic to miss, so it always runs under serve).
	rawInterval := store.Env("PULSE_COLLECT_INTERVAL", "5m")
	if interval, err := time.ParseDuration(rawInterval); err != nil {
		slog.Warn("invalid PULSE_COLLECT_INTERVAL, collector disabled", "value", rawInterval)
	} else if interval <= 0 {
		slog.Info("collector disabled", "interval", rawInterval)
	} else {
		startCollector(db, dbPath, interval)
	}

	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Console — session-gated. There is no public monitoring dashboard;
	// per-target data never leaves the session.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleOverview))
	mux.HandleFunc("GET /targets", s.auth.RequireSession(s.handleTargets))
	mux.HandleFunc("GET /targets/new", s.auth.RequireSession(s.handleNewTarget))
	mux.HandleFunc("POST /targets", s.auth.RequireSession(s.handleCreateTarget))
	mux.HandleFunc("GET /targets/{id}/edit", s.auth.RequireSession(s.handleEditTarget))
	mux.HandleFunc("POST /targets/{id}", s.auth.RequireSession(s.handleUpdateTarget))
	mux.HandleFunc("POST /targets/{id}/delete", s.auth.RequireSession(s.handleDeleteTarget))
	mux.HandleFunc("GET /traffic", s.auth.RequireSession(s.handleTraffic))

	// Gated JSON mirrors of the console pages.
	mux.HandleFunc("GET /api/overview", s.auth.RequireSession(s.handleAPIOverview))
	mux.HandleFunc("GET /api/traffic", s.auth.RequireSession(s.handleAPITraffic))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// Liveness — the compose healthcheck convention. Exposes a bare target
	// count and nothing about the targets themselves.
	mux.HandleFunc("GET /status", s.handleStatus)

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Everything pulse serves is text, so Gzip wraps the whole mux; logging
	// sits outside so the recorded status is the final one.
	return web.LogRequests(web.Gzip(mux))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countTargets(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "pulse",
		"ok":      true,
		"targets": n,
	})
}

// ── overview ───────────────────────────────────────────────────────────────

// targetRow is one overview line: the target, its latest check, uptime
// windows, and the open incident if any.
type targetRow struct {
	Target
	Last      *Check        `json:"last"`
	Up24      string        `json:"up24h"`
	Up7       string        `json:"up7d"`
	Up30      string        `json:"up30d"`
	Incident  *Incident     `json:"incident,omitempty"`
	Sparkline template.HTML `json:"-"`
}

func (s *Server) overviewRows() ([]targetRow, error) {
	targets, err := listTargets(s.db)
	if err != nil {
		return nil, err
	}
	rows := make([]targetRow, 0, len(targets))
	for _, t := range targets {
		row := targetRow{Target: t}
		if row.Last, err = latestCheck(s.db, t.ID); err != nil {
			return nil, err
		}
		row.Up24 = s.uptimeLabel(t.ID, 24*time.Hour)
		row.Up7 = s.uptimeLabel(t.ID, 7*24*time.Hour)
		row.Up30 = s.uptimeLabel(t.ID, 30*24*time.Hour)
		if row.Incident, err = openIncident(s.db, t.ID); err != nil {
			return nil, err
		}
		checks, err := recentChecks(s.db, t.ID, sparkChecks)
		if err != nil {
			return nil, err
		}
		row.Sparkline = sparkline(checks)
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Server) uptimeLabel(targetID int64, window time.Duration) string {
	pct, ok, err := uptime(s.db, targetID, window)
	if err != nil {
		slog.Warn("uptime query failed", "target", targetID, "err", err)
	}
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", pct)
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	rows, err := s.overviewRows()
	if err != nil {
		s.fail(w, "overview", err)
		return
	}
	incidents, err := recentIncidents(s.db, 20)
	if err != nil {
		s.fail(w, "incidents", err)
		return
	}
	s.rd.Render(w, "index.html", map[string]any{
		"Rows":      rows,
		"Incidents": incidents,
	})
}

func (s *Server) handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	rows, err := s.overviewRows()
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read overview")
		return
	}
	incidents, err := recentIncidents(s.db, 20)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read incidents")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"targets": rows, "incidents": incidents,
	})
}

// ── target CRUD ────────────────────────────────────────────────────────────

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := listTargets(s.db)
	if err != nil {
		s.fail(w, "list targets", err)
		return
	}
	s.rd.Render(w, "targets.html", map[string]any{"Targets": targets})
}

func (s *Server) handleNewTarget(w http.ResponseWriter, r *http.Request) {
	t := &Target{Method: "GET", ExpectedStatus: 200, IntervalS: 60, Enabled: true}
	s.renderTargetForm(w, t, true, "/targets", "")
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	t, errMsg := targetFromForm(r)
	if errMsg != "" {
		s.renderTargetForm(w, t, true, "/targets", errMsg)
		return
	}
	if err := insertTarget(s.db, t); err != nil {
		s.fail(w, "create target", err)
		return
	}
	http.Redirect(w, r, "/targets", http.StatusSeeOther)
}

func (s *Server) handleEditTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.targetFromPath(w, r)
	if t == nil || err != nil {
		return
	}
	s.renderTargetForm(w, t, false, "/targets/"+strconv.FormatInt(t.ID, 10), "")
}

func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	existing, err := s.targetFromPath(w, r)
	if existing == nil || err != nil {
		return
	}
	t, errMsg := targetFromForm(r)
	t.ID = existing.ID
	if errMsg != "" {
		s.renderTargetForm(w, t, false, "/targets/"+strconv.FormatInt(t.ID, 10), errMsg)
		return
	}
	if _, err := updateTarget(s.db, t); err != nil {
		s.fail(w, "update target", err)
		return
	}
	http.Redirect(w, r, "/targets", http.StatusSeeOther)
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := deleteTarget(s.db, id); err != nil {
		s.fail(w, "delete target", err)
		return
	}
	http.Redirect(w, r, "/targets", http.StatusSeeOther)
}

// targetFromPath resolves the {id} path segment to a stored target, writing
// the 404 itself when absent.
func (s *Server) targetFromPath(w http.ResponseWriter, r *http.Request) (*Target, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil
	}
	t, err := getTarget(s.db, id)
	if err != nil {
		s.fail(w, "get target", err)
		return nil, err
	}
	if t == nil {
		http.NotFound(w, r)
	}
	return t, nil
}

// targetFromForm reads and validates a Target from a posted form. A non-empty
// second return is the user-facing validation error.
func targetFromForm(r *http.Request) (*Target, string) {
	_ = r.ParseForm()
	t := &Target{
		Name:    strings.TrimSpace(r.FormValue("name")),
		URL:     strings.TrimSpace(r.FormValue("url")),
		Method:  strings.ToUpper(strings.TrimSpace(r.FormValue("method"))),
		Enabled: r.FormValue("enabled") == "on",
	}
	t.ExpectedStatus, _ = strconv.Atoi(r.FormValue("expected_status"))
	t.IntervalS, _ = strconv.Atoi(r.FormValue("interval_s"))
	if t.Method == "" {
		t.Method = "GET"
	}
	if t.ExpectedStatus == 0 {
		t.ExpectedStatus = 200
	}
	if t.IntervalS == 0 {
		t.IntervalS = 60
	}
	switch {
	case t.Name == "":
		return t, "Name is required."
	case t.URL == "":
		return t, "URL is required."
	case t.IntervalS < 1:
		return t, "Interval must be at least 1 second."
	}
	if u, err := url.ParseRequestURI(t.URL); err != nil || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return t, "URL must be absolute http(s)."
	}
	return t, ""
}

func (s *Server) renderTargetForm(w http.ResponseWriter, t *Target, isNew bool, action, errMsg string) {
	s.rd.Render(w, "target_form.html", map[string]any{
		"Target": t, "IsNew": isNew, "Action": action, "Error": errMsg,
	})
}

// ── traffic ────────────────────────────────────────────────────────────────

// trafficData gathers everything the traffic page (and its JSON mirror)
// shows for one app/date-range selection.
type trafficData struct {
	App     string       `json:"app"`
	From    string       `json:"from"`
	To      string       `json:"to"`
	Apps    []string     `json:"apps"`
	Hits    []DayCount   `json:"hitsPerDay"`
	Uniques []DayCount   `json:"uniquesPerDay"`
	Paths   []PathStat   `json:"topPaths"`
	Mix     []BucketStat `json:"statusMix"`
	Refs    []RefStat    `json:"topReferrers"`
}

func (s *Server) trafficQuery(r *http.Request) (*trafficData, error) {
	q := r.URL.Query()
	today := time.Now().UTC()
	d := &trafficData{
		App:  q.Get("app"),
		From: dayParam(q.Get("from"), today.AddDate(0, 0, -13)),
		To:   dayParam(q.Get("to"), today),
	}
	if d.From > d.To {
		d.From, d.To = d.To, d.From
	}
	var err error
	if d.Apps, err = trafficApps(s.db); err != nil {
		return nil, err
	}
	if d.Hits, err = hitsPerDay(s.db, d.App, d.From, d.To); err != nil {
		return nil, err
	}
	if d.Uniques, err = uniquesPerDay(s.db, d.App, d.From, d.To); err != nil {
		return nil, err
	}
	d.Hits = fillDays(d.Hits, d.From, d.To)
	d.Uniques = fillDays(d.Uniques, d.From, d.To)
	if d.Paths, err = topPaths(s.db, d.App, d.From, d.To, 20); err != nil {
		return nil, err
	}
	if d.Mix, err = statusMix(s.db, d.App, d.From, d.To); err != nil {
		return nil, err
	}
	if d.Refs, err = topReferrers(s.db, d.App, d.From, d.To, 20); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	d, err := s.trafficQuery(r)
	if err != nil {
		s.fail(w, "traffic", err)
		return
	}
	totalHits := 0
	for _, dc := range d.Hits {
		totalHits += dc.N
	}
	s.rd.Render(w, "traffic.html", map[string]any{
		"D":            d,
		"TotalHits":    totalHits,
		"HitsChart":    barChart(d.Hits, "hits"),
		"UniquesChart": barChart(d.Uniques, "path-uniques"),
	})
}

func (s *Server) handleAPITraffic(w http.ResponseWriter, r *http.Request) {
	d, err := s.trafficQuery(r)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read traffic")
		return
	}
	web.WriteJSON(w, http.StatusOK, d)
}

// dayParam parses a YYYY-MM-DD query value, falling back to def.
func dayParam(v string, def time.Time) string {
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.Format("2006-01-02")
	}
	return def.Format("2006-01-02")
}

// fillDays expands sparse per-day counts into a contiguous [from, to] range
// (capped at a year) so the bar charts show gaps honestly.
func fillDays(in []DayCount, from, to string) []DayCount {
	start, err1 := time.Parse("2006-01-02", from)
	end, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil || end.Before(start) ||
		end.Sub(start) > 366*24*time.Hour {
		return in
	}
	byDay := make(map[string]int, len(in))
	for _, d := range in {
		byDay[d.Day] = d.N
	}
	var out []DayCount
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		day := d.Format("2006-01-02")
		out = append(out, DayCount{Day: day, N: byDay[day]})
	}
	return out
}

// ── login & misc ───────────────────────────────────────────────────────────

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
