package main

import (
	"io"
	"net/http"
	"strconv"

	"github.com/iammatthias/farfield/lib/qrenc"
	"github.com/iammatthias/farfield/lib/web"
)

// The private admin API (web.PrivateAPI): every code — private, disabled,
// admin notes and all — and its rendering regardless of flags, which is what
// the session console's list and preview show. The public scan, redirect and
// JSON read routes are unchanged.

func (s *Server) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/codes", s.auth.PrivateAPI(s.handleAdminList))
	mux.HandleFunc("GET /api/admin/codes/{id}", s.auth.PrivateAPI(s.handleAdminGet))
	mux.HandleFunc("GET /api/admin/codes/{id}/preview.svg", s.auth.PrivateAPI(s.handleAdminPreviewSVG))
	mux.HandleFunc("GET /api/admin/codes/{id}/preview.png", s.auth.PrivateAPI(s.handleAdminPreviewPNG))
	mux.HandleFunc(web.AdminPrefix, s.auth.PrivateAPI(web.AdminNotFound))
}

func (s *Server) handleAdminList(w http.ResponseWriter, r *http.Request) {
	cs, err := listCodes(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list codes")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"codes": cs})
}

func (s *Server) handleAdminGet(w http.ResponseWriter, r *http.Request) {
	c := s.adminCode(w, r)
	if c == nil {
		return
	}
	web.WriteRecord(w, r, c.CID, c)
}

// handleAdminPreviewSVG is the session console's preview over the admin API:
// the same memoized renderer, regardless of public/enabled.
func (s *Server) handleAdminPreviewSVG(w http.ResponseWriter, r *http.Request) {
	c := s.adminCode(w, r)
	if c == nil {
		return
	}
	svg, _, err := s.encodeFor(c)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not encode QR")
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	_, _ = io.WriteString(w, svg)
}

// Preview raster bounds, in pixels. The floor keeps a requested thumbnail
// scannable; the ceiling keeps one request from allocating a poster.
const (
	minPreviewPx     = 64
	maxPreviewPx     = 2048
	defaultPreviewPx = 512
)

// handleAdminPreviewPNG renders the code as a PNG about ?size= pixels square
// (clamped to 64..2048, default 512). The image is whole modules — scaled by
// the largest integer factor that fits, quiet zone included — so it comes out
// at or just under the requested size rather than resampled to it, and a code
// too dense to fit at one pixel per module comes out at that minimum instead.
func (s *Server) handleAdminPreviewPNG(w http.ResponseWriter, r *http.Request) {
	c := s.adminCode(w, r)
	if c == nil {
		return
	}
	size := defaultPreviewPx
	if n, err := strconv.Atoi(r.URL.Query().Get("size")); err == nil {
		size = min(max(n, minPreviewPx), maxPreviewPx)
	}
	ec, _ := qrenc.ParseECLevel(c.EC)
	mod, _, err := qrenc.Encode([]byte(s.payloadFor(c)), ec)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not encode QR")
		return
	}
	const quiet = 4 // RenderPNG's quiet zone, in modules, per side
	raster, err := qrenc.RenderPNG(mod, max(1, size/(len(mod)+2*quiet)))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not encode QR")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(raster)
}

// adminCode loads the {id} code for an admin route, writing the 404 or 500
// itself and returning nil when there is nothing to serve.
func (s *Server) adminCode(w http.ResponseWriter, r *http.Request) *Code {
	c, err := getCode(s.db, r.PathValue("id"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read code")
		return nil
	}
	if c == nil {
		web.WriteError(w, http.StatusNotFound, "code not found")
		return nil
	}
	return c
}
