package web

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

// shared holds the fleet-wide page shell — the layout, masthead, login body,
// and pager every app renders through. See templates/layout.html.
//
//go:embed templates/layout.html
var shared embed.FS

// ParseTemplates parses every templates/*.html page against templates/base.html
// — the layout convention all farfield apps share — with the shared farfield
// partials available to all of them. funcs may be nil.
//
// Parse order is shared-then-app, so an app that defines "base", "topbar", or
// any other shared name overrides it rather than colliding with it. An app
// with no base.html of its own simply uses the shared layout.
func ParseTemplates(fsys fs.FS, funcs template.FuncMap) (map[string]*template.Template, error) {
	pages, err := fs.Glob(fsys, "templates/*.html")
	if err != nil {
		return nil, err
	}
	hasBase := false
	for _, page := range pages {
		if path.Base(page) == "base.html" {
			hasBase = true
			break
		}
	}
	out := make(map[string]*template.Template)
	for _, page := range pages {
		name := path.Base(page)
		if name == "base.html" {
			continue
		}
		t := template.New(name)
		if funcs != nil {
			t = t.Funcs(funcs)
		}
		t, err := t.ParseFS(shared, "templates/layout.html")
		if err != nil {
			return nil, err
		}
		files := []string{page}
		if hasBase {
			files = append([]string{"templates/base.html"}, files...)
		}
		t, err = t.ParseFS(fsys, files...)
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

// NavItem is one link in the shared masthead.
type NavItem struct {
	Label string
	URL   string
	Class string // optional, e.g. "on" for the current section
}

// Renderer renders parsed pages through the shared base layout, buffering
// first so a template error never produces a half-written response.
type Renderer struct {
	Templates map[string]*template.Template
	AssetVer  string // stamped into every page as .AssetVer for cache-busted asset URLs

	// App is the service name shown in the title and masthead ("content"),
	// and Mark its two-letter glyph for the favicon and login plate ("co").
	// Mark defaults to the first two letters of App.
	App  string
	Mark string

	// Description is the app's meta description and og:description — one
	// sentence, plain text. Left empty the social tags are omitted entirely
	// rather than emitted blank, which renders as an empty card when a link
	// is shared. Only public apps need it; the gated ones are never unfurled.
	Description string

	// PublicURL is where this deployment is reachable ("https://x.farfield.systems"),
	// used for og:url and rel=canonical. Empty omits both: a canonical
	// pointing at the wrong origin is worse than none.
	PublicURL string

	// Nav is the masthead's link list. Apps with a session should end it with
	// a logout link; apps with no masthead can leave it nil.
	Nav []NavItem

	// Funcs mirrors the FuncMap the app passed to ParseTemplates — only
	// needed so dev-mode live reloads (FARFIELD_DEV_TEMPLATES) can re-parse
	// with the same functions. Apps with a nil FuncMap can ignore it.
	Funcs template.FuncMap
}

// mark returns the two-letter glyph for this app.
func (rd *Renderer) mark() string {
	if rd.Mark != "" {
		return rd.Mark
	}
	if len(rd.App) >= 2 {
		return rd.App[:2]
	}
	return rd.App
}

// favicon builds the app's tab icon: the two-letter mark on the farfield ink
// square, as an inline SVG data URI. Every app hand-wrote this string with
// only the glyph differing.
func (rd *Renderer) favicon() template.HTML {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">` +
		`<rect width="64" height="64" rx="8" fill="#17265c"/>` +
		`<text x="32" y="42" font-family="ui-monospace,Menlo,monospace" font-size="26" ` +
		`font-weight="600" text-anchor="middle" fill="#f7f3e8">` +
		template.HTMLEscapeString(rd.mark()) + `</text></svg>`
	// PathEscape leaves the quotes and angle brackets a data: URI needs
	// readable, and escapes the spaces and '#' that would break the attribute.
	return template.HTML(`<link rel="icon" href="data:image/svg+xml,` +
		strings.ReplaceAll(url.PathEscape(svg), "&", "%26") + `">`)
}

// Render writes a page. data may be nil; map data gets the shell's keys
// injected — AssetVer, App, Mark, Favicon, Nav, and FleetNav — so no handler
// has to remember to supply page furniture.
func (rd *Renderer) Render(w http.ResponseWriter, page string, data map[string]any) {
	t, ok := rd.Templates[page]
	if !ok {
		slog.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Dev mode: FARFIELD_DEV_TEMPLATES points at the app directory (the one
	// containing templates/) — pages re-parse from disk on every render, so
	// template edits show on reload without a rebuild. Parse errors log and
	// fall back to the embedded template.
	if dir := os.Getenv("FARFIELD_DEV_TEMPLATES"); dir != "" {
		if live, err := ParseTemplates(os.DirFS(dir), rd.Funcs); err != nil {
			slog.Warn("dev templates", "err", err)
		} else if lt, ok := live[page]; ok {
			t = lt
		}
	}
	if data == nil {
		data = map[string]any{}
	}
	data["AssetVer"] = rd.AssetVer
	data["FleetNav"] = fleetNav()
	data["App"] = rd.App
	data["Mark"] = rd.mark()
	data["Favicon"] = rd.favicon()
	// Set only when the handler has not, so a page can carry its own
	// description without the shell overwriting it.
	if _, set := data["Description"]; !set {
		data["Description"] = rd.Description
	}
	if _, set := data["PublicURL"]; !set {
		data["PublicURL"] = rd.PublicURL
	}
	if _, set := data["Nav"]; !set {
		data["Nav"] = rd.Nav
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		slog.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
