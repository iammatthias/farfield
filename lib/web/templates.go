package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
)

// ParseTemplates parses every templates/*.html page against templates/base.html
// — the layout convention all farfield apps share. funcs may be nil.
func ParseTemplates(fsys fs.FS, funcs template.FuncMap) (map[string]*template.Template, error) {
	pages, err := fs.Glob(fsys, "templates/*.html")
	if err != nil {
		return nil, err
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
		t, err := t.ParseFS(fsys, "templates/base.html", page)
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

// Renderer renders parsed pages through the shared base layout, buffering
// first so a template error never produces a half-written response.
type Renderer struct {
	Templates map[string]*template.Template
	AssetVer  string // stamped into every page as .AssetVer for cache-busted asset URLs

	// Funcs mirrors the FuncMap the app passed to ParseTemplates — only
	// needed so dev-mode live reloads (FARFIELD_DEV_TEMPLATES) can re-parse
	// with the same functions. Apps with a nil FuncMap can ignore it.
	Funcs template.FuncMap
}

// Render writes a page. data may be nil; map data gets AssetVer injected.
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
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		slog.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
