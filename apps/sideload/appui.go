package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// The author's pages: the app list, one app's page and its editor, screenshot
// management, and build notes. Session-gated throughout — this is the side
// only the person shipping builds ever sees.

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.renderIndex(w, http.StatusOK, "")
}

func (s *Server) renderIndex(w http.ResponseWriter, status int, errMsg string) {
	apps, err := listApps(s.db)
	if err != nil {
		slog.Error("list apps", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total, _ := countBuilds(s.db)
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.rd.Render(w, "index.html", map[string]any{
		"Apps":  apps,
		"Total": total,
		"Error": errMsg,
	})
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	builds, err := listBuildsByBundle(s.db, bundle)
	if err != nil {
		slog.Error("list builds", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	meta, shots, err := s.loadAppContent(bundle)
	if err != nil {
		slog.Error("load app content", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	devs, err := s.appDevices(bundle)
	if err != nil {
		slog.Error("list devices", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	prov := provisionedSet(&builds[0])
	pending := 0
	for _, d := range devs {
		if !prov[strings.ToLower(d.UDID)] {
			pending++
		}
	}
	s.rd.Render(w, "app.html", map[string]any{
		"BundleID":    bundle,
		"AppName":     builds[0].AppName,
		"Latest":      builds[0],
		"Builds":      builds,
		"Meta":        meta,
		"Screenshots": shots,
		"DeviceCount": len(devs),
		"Pending":     pending,
	})
}

// loadAppContent fetches an app's optional rich-page metadata and screenshots.
// Both may be empty — the page renders fine without them.
func (s *Server) loadAppContent(bundle string) (*AppMeta, []Screenshot, error) {
	meta, err := getAppMeta(s.db, bundle)
	if err != nil {
		return nil, nil, err
	}
	shots, err := listScreenshots(s.db, bundle)
	if err != nil {
		return nil, nil, err
	}
	return meta, shots, nil
}

// handleAppEdit renders the rich-page editor: tagline + description, and the
// screenshot manager.
func (s *Server) handleAppEdit(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	builds, err := listBuildsByBundle(s.db, bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	meta, shots, err := s.loadAppContent(bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.rd.Render(w, "app_edit.html", map[string]any{
		"BundleID":    bundle,
		"AppName":     builds[0].AppName,
		"Meta":        meta,
		"Screenshots": shots,
		"Error":       r.URL.Query().Get("error"),
	})
}

// handleAppMetaSave stores the tagline and description (markdown). Clearing both
// removes the row, returning the app to the plain view.
func (s *Server) handleAppMetaSave(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	_ = r.ParseForm()
	if err := upsertAppMeta(s.db, &AppMeta{
		BundleID:    bundle,
		Tagline:     strings.TrimSpace(r.FormValue("tagline")),
		Description: strings.TrimSpace(r.FormValue("description")),
	}); err != nil {
		slog.Error("save app meta", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

// handleScreenshotUpload stores an uploaded image for the app's gallery.
func (s *Server) handleScreenshotUpload(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	f, _, err := r.FormFile("image")
	if err != nil {
		s.editError(w, r, bundle, "choose an image to upload")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxScreenshotBytes+1))
	if err != nil {
		s.editError(w, r, bundle, "could not read the upload")
		return
	}
	if int64(len(data)) > maxScreenshotBytes {
		s.editError(w, r, bundle, "image too large (max 12 MB)")
		return
	}
	mime, ext, width, height, err := imageInfo(data)
	if err != nil {
		s.editError(w, r, bundle, err.Error())
		return
	}
	cidStr, err := s.blobs.putBytes(data, ext)
	if err != nil {
		slog.Error("store screenshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := addScreenshot(s.db, &Screenshot{
		ID:       store.ShortID(),
		BundleID: bundle,
		CID:      cidStr,
		Ext:      ext,
		Mime:     mime,
		Width:    width,
		Height:   height,
		Caption:  strings.TrimSpace(r.FormValue("caption")),
	}); err != nil {
		slog.Error("add screenshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

// editError re-shows the edit page with a message.
func (s *Server) editError(w http.ResponseWriter, r *http.Request, bundle, msg string) {
	http.Redirect(w, r, "/app/"+bundle+"/edit?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) handleScreenshotCaption(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	_ = r.ParseForm()
	if err := setScreenshotCaption(s.db, r.PathValue("sid"), strings.TrimSpace(r.FormValue("caption"))); err != nil {
		slog.Error("caption", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

func (s *Server) handleScreenshotMove(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	dir := r.URL.Query().Get("dir")
	if dir != "up" && dir != "down" {
		dir = "down"
	}
	if err := moveScreenshot(s.db, r.PathValue("sid"), dir); err != nil {
		slog.Error("move screenshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

func (s *Server) handleScreenshotDelete(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	sh, err := deleteScreenshot(s.db, r.PathValue("sid"))
	if err != nil {
		slog.Error("delete screenshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sh != nil {
		// Drop the image file only when no other screenshot shares its bytes.
		if others, _ := screenshotsWithCID(s.db, sh.CID); others == 0 {
			if err := s.blobs.remove(sh.CID, sh.Ext); err != nil {
				slog.Warn("could not remove screenshot file", "cid", sh.CID, "err", err)
			}
		}
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

// handleScreenshot serves a screenshot image by id — public and immutable,
// since the bytes are content-addressed. It appears on the public share page.
func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	sh, err := getScreenshot(s.db, r.PathValue("sid"))
	if err != nil || sh == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", `"`+sh.CID+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if web.ETagMatch(r, sh.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f, _, err := s.blobs.open(sh.CID, sh.Ext)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", sh.Mime)
	modtime := time.Time{}
	http.ServeContent(w, r, "screenshot"+sh.Ext, modtime, f)
}

// handleBuildNotes updates a version's changelog ("what's new").
func (s *Server) handleBuildNotes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBuild(s.db, id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	if err := updateBuildNotes(s.db, id, strings.TrimSpace(r.FormValue("notes"))); err != nil {
		slog.Error("update notes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/b/"+id, http.StatusSeeOther)
}
