package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// Ingest: a build arrives as an upload, gets hashed, parsed for its bundle id
// and version, stored, and recorded. Both the browser form and the API land here.

// ingest stores an uploaded .ipa under its content address, parses its
// metadata, and records the build. It streams the upload to disk (never
// buffering the whole archive) and dedupes identical bytes.
func (s *Server) ingest(src io.Reader, filename, gitCommit, notes string) (*Build, error) {
	fullCID, size, err := s.blobs.spool(src, maxIPABytes, ".ipa")
	if err != nil {
		return nil, err
	}
	id := fullCID[:16]

	meta, err := parseIPA(s.blobs.path(fullCID, ".ipa"))
	if err != nil {
		// Not a valid .ipa. Drop the bytes unless a prior build already claims
		// this content address.
		if existing, gerr := getBuild(s.db, id); gerr == nil && existing == nil {
			_ = s.blobs.remove(fullCID, ".ipa")
		}
		return nil, err
	}

	expiry := ""
	if !meta.ProfileExpiry.IsZero() {
		expiry = meta.ProfileExpiry.UTC().Format(time.RFC3339)
	}
	b := &Build{
		ID:            id,
		CID:           fullCID,
		BundleID:      meta.BundleID,
		AppName:       meta.AppName,
		Version:       meta.Version,
		BuildNumber:   meta.BuildNumber,
		Team:          meta.Team,
		ProfileExpiry: expiry,
		DeviceCount:   len(meta.UDIDs),
		UDIDs:         strings.Join(meta.UDIDs, "\n"),
		SizeBytes:     size,
		Filename:      sanitizeFilename(filename),
		GitCommit:     strings.TrimSpace(gitCommit),
		Notes:         strings.TrimSpace(notes),
	}
	created, err := insertBuild(s.db, b)
	if err != nil {
		return nil, err
	}
	// Keep the author's own device in the whitelist for every app.
	s.ensureOwnerDevice(b.BundleID)
	if !created {
		// Identical bytes already stored — return the canonical existing row.
		if existing, err := getBuild(s.db, id); err == nil && existing != nil {
			return existing, nil
		}
	}
	slog.Info("build ingested", "id", b.ID, "bundle", b.BundleID,
		"version", b.Version, "build", b.BuildNumber, "size", b.SizeBytes, "new", created)
	return b, nil
}

// readUpload extracts the .ipa reader and filename from a request, handling both
// a raw body (agent: curl --data-binary @app.ipa) and a multipart form field
// named "ipa" (browser). The returned closer, if non-nil, must be closed.
func readUpload(r *http.Request) (src io.Reader, filename string, closer io.Closer, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		f, hdr, ferr := r.FormFile("ipa")
		if ferr != nil {
			return nil, "", nil, fmt.Errorf("no .ipa file in form: %w", ferr)
		}
		return f, hdr.Filename, f, nil
	}
	name := r.URL.Query().Get("filename")
	if name == "" {
		name = "app.ipa"
	}
	return r.Body, name, nil, nil
}

// handleUpload is the browser multipart upload from the index form.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	src, filename, closer, err := readUpload(r)
	if err != nil {
		s.renderIndex(w, http.StatusBadRequest, err.Error())
		return
	}
	if closer != nil {
		defer closer.Close()
	}
	b, err := s.ingest(src, filename, r.FormValue("commit"), r.FormValue("notes"))
	if err != nil {
		s.renderIndex(w, http.StatusBadRequest, err.Error())
		return
	}
	http.Redirect(w, r, "/b/"+b.ID, http.StatusSeeOther)
}

// handleAPIUpload is the agent upload endpoint.
func (s *Server) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	src, filename, closer, err := readUpload(r)
	if err != nil {
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if closer != nil {
		defer closer.Close()
	}
	q := r.URL.Query()
	b, err := s.ingest(src, filename, q.Get("commit"), q.Get("notes"))
	if err != nil {
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	web.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":            b.ID,
		"cid":           b.CID,
		"bundleId":      b.BundleID,
		"appName":       b.AppName,
		"version":       b.Version,
		"buildNumber":   b.BuildNumber,
		"profileExpiry": b.ProfileExpiry,
		"deviceCount":   b.DeviceCount,
		"sizeBytes":     b.SizeBytes,
		"installURL":    s.publicURL + "/b/" + b.ID,
	})
}
