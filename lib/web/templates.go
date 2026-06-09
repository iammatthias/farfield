package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
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
}

// Render writes a page. data may be nil; map data gets AssetVer injected.
func (rd *Renderer) Render(w http.ResponseWriter, page string, data map[string]any) {
	t, ok := rd.Templates[page]
	if !ok {
		slog.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["AssetVer"] = rd.AssetVer
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		slog.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
