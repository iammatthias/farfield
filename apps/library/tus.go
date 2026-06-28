package main

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// tus 1.0.0 resumable uploads. Large EPUBs exceed the per-request body limit
// the Cloudflare edge enforces, so a single POST can't carry them. tus splits
// one upload across many small PATCH requests — each well under the edge limit —
// which the server appends to a staging file and, once complete, hands to the
// normal EPUB ingest. Only the core + creation + termination extensions are
// implemented; that's all a chunking client needs.
const (
	tusVersion = "1.0.0"
	tusOctet   = "application/offset+octet-stream"
	// uploadTTL is how long a partial upload may sit untouched before a startup
	// prune reclaims its staging file.
	uploadTTL = 24 * time.Hour
)

// pruneStaleUploads drops upload rows older than uploadTTL and removes their
// staging files. Called once at startup so abandoned partial uploads don't
// accumulate on the data volume.
func (s *Server) pruneStaleUploads() {
	cutoff := time.Now().Add(-uploadTTL).UTC().Format(time.RFC3339)
	ids, err := pruneUploads(s.db, cutoff)
	if err != nil {
		slog.Warn("prune stale uploads", "err", err)
		return
	}
	for _, id := range ids {
		if err := os.Remove(s.stagingPath(id)); err != nil && !os.IsNotExist(err) {
			slog.Warn("remove stale staging file", "id", id, "err", err)
		}
	}
}

// stagingPath is the on-disk file holding the partial bytes for an upload.
func (s *Server) stagingPath(id string) string {
	return filepath.Join(s.tusDir, id)
}

// stagingSize is the number of bytes received so far — the tus Upload-Offset —
// read straight from the staging file so it is always the source of truth.
func (s *Server) stagingSize(id string) (int64, error) {
	fi, err := os.Stat(s.stagingPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return fi.Size(), nil
}

// tusOptionsShim answers the tus capability probe (OPTIONS on the upload path)
// before the shared CORS middleware can swallow it with a generic 204. Every
// other request passes straight through.
func (s *Server) tusOptionsShim(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions && strings.HasPrefix(r.URL.Path, "/api/upload/tus") {
			h := w.Header()
			h.Set("Tus-Resumable", tusVersion)
			h.Set("Tus-Version", tusVersion)
			h.Set("Tus-Extension", "creation,termination")
			h.Set("Tus-Max-Size", strconv.FormatInt(s.maxUpload, 10))
			h.Set("Access-Control-Allow-Origin", "*")
			h.Set("Access-Control-Allow-Methods", "POST, HEAD, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, X-API-Key, Content-Type, Tus-Resumable, Upload-Length, Upload-Offset, Upload-Metadata")
			h.Set("Access-Control-Expose-Headers", "Location, Upload-Offset, Upload-Length, Tus-Resumable, X-Library-Cid")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleTusCreate begins a resumable upload: it records the declared length and
// metadata and creates an empty staging file. The client then PATCHes chunks to
// the returned Location.
func (s *Server) handleTusCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tus-Resumable", tusVersion)
	length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || length <= 0 {
		web.WriteError(w, http.StatusBadRequest, "missing or invalid Upload-Length")
		return
	}
	if length > s.maxUpload {
		w.Header().Set("Tus-Max-Size", strconv.FormatInt(s.maxUpload, 10))
		web.WriteError(w, http.StatusRequestEntityTooLarge, "upload exceeds maximum size")
		return
	}
	filename, collection := parseTusMetadata(r.Header.Get("Upload-Metadata"))

	if err := os.MkdirAll(s.tusDir, 0o755); err != nil {
		s.tusFail(w, "make staging dir", err)
		return
	}
	id := store.ShortID()
	f, err := os.OpenFile(s.stagingPath(id), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		s.tusFail(w, "create staging file", err)
		return
	}
	f.Close()

	u := &Upload{ID: id, Length: length, Filename: filename, Collection: collection, CreatedAt: store.NowRFC3339()}
	if err := createUpload(s.db, u); err != nil {
		_ = os.Remove(s.stagingPath(id))
		s.tusFail(w, "record upload", err)
		return
	}
	w.Header().Set("Location", "/api/upload/tus/"+id)
	w.Header().Set("Upload-Offset", "0")
	w.WriteHeader(http.StatusCreated)
}

// handleTusHead reports how many bytes the server holds and the finalize status,
// so a client can resume an interrupted upload and poll for the eventual result.
// Once the upload is "done" the new book's cid rides in X-Library-Cid; a failed
// finalize reports "error" with the reason in X-Library-Error.
func (s *Server) handleTusHead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tus-Resumable", tusVersion)
	u, err := getUpload(s.db, r.PathValue("id"))
	if err != nil {
		s.tusFail(w, "read upload", err)
		return
	}
	if u == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	offset, err := s.stagingSize(u.ID)
	if err != nil {
		s.tusFail(w, "stat staging file", err)
		return
	}
	h := w.Header()
	// After a successful finalize the staging file is gone; report the full
	// length so a polling client sees a consistent offset.
	if u.Status == uploadDone {
		offset = u.Length
		h.Set("X-Library-Cid", u.BookCID)
	}
	if u.Status == uploadError && u.Error != "" {
		h.Set("X-Library-Error", u.Error)
	}
	h.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	h.Set("Upload-Length", strconv.FormatInt(u.Length, 10))
	h.Set("X-Library-Status", u.Status)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// handleTusPatch appends one chunk at the declared offset. When the upload
// reaches its full length, finalization (the potentially minutes-long R2 upload
// and EPUB ingest) runs in the BACKGROUND — the PATCH returns immediately with
// X-Library-Status: finalizing, so the response never blocks past the edge's
// origin-response timeout. The client polls HEAD for the result (done + cid, or
// error). PATCH is idempotent against an already-complete upload.
func (s *Server) handleTusPatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tus-Resumable", tusVersion)
	u, err := getUpload(s.db, r.PathValue("id"))
	if err != nil {
		s.tusFail(w, "read upload", err)
		return
	}
	if u == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Settled or in flight: report state without touching bytes.
	switch u.Status {
	case uploadDone:
		w.Header().Set("Upload-Offset", strconv.FormatInt(u.Length, 10))
		w.Header().Set("X-Library-Status", uploadDone)
		w.Header().Set("X-Library-Cid", u.BookCID)
		w.WriteHeader(http.StatusNoContent)
		return
	case uploadFinalizing:
		w.Header().Set("Upload-Offset", strconv.FormatInt(u.Length, 10))
		w.Header().Set("X-Library-Status", uploadFinalizing)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if !strings.HasPrefix(r.Header.Get("Content-Type"), tusOctet) {
		web.WriteError(w, http.StatusUnsupportedMediaType, "Content-Type must be "+tusOctet)
		return
	}
	cur, err := s.stagingSize(u.ID)
	if err != nil {
		s.tusFail(w, "stat staging file", err)
		return
	}
	// Append only while bytes remain. A retry after a failed finalize (status
	// "error", staging already full) falls through straight to re-finalizing.
	if cur < u.Length {
		offset, perr := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
		if perr != nil {
			web.WriteError(w, http.StatusBadRequest, "missing or invalid Upload-Offset")
			return
		}
		if offset != cur {
			web.WriteError(w, http.StatusConflict, "Upload-Offset does not match server state")
			return
		}
		f, oerr := os.OpenFile(s.stagingPath(u.ID), os.O_APPEND|os.O_WRONLY, 0o644)
		if oerr != nil {
			s.tusFail(w, "open staging file", oerr)
			return
		}
		// Never store past the declared length, even if the client over-sends.
		written, copyErr := io.Copy(f, io.LimitReader(r.Body, u.Length-cur))
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			s.tusFail(w, "write chunk", firstErr(copyErr, closeErr))
			return
		}
		cur += written
	}

	w.Header().Set("Upload-Offset", strconv.FormatInt(cur, 10))
	if cur < u.Length {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// All bytes received — ingest off the request path.
	s.beginFinalize(u.ID)
	w.Header().Set("X-Library-Status", uploadFinalizing)
	w.WriteHeader(http.StatusNoContent)
}

// handleTusDelete terminates an in-progress upload, removing its staging file.
func (s *Server) handleTusDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tus-Resumable", tusVersion)
	u, err := getUpload(s.db, r.PathValue("id"))
	if err != nil {
		s.tusFail(w, "read upload", err)
		return
	}
	if u == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.discardUpload(u.ID)
	w.WriteHeader(http.StatusNoContent)
}

// beginFinalize transitions the upload to "finalizing" and, if this caller won
// that transition, ingests it in the background. markFinalizing makes the win
// exclusive, so concurrent or repeated PATCHes never start two finalizes.
func (s *Server) beginFinalize(id string) {
	won, err := markFinalizing(s.db, id)
	if err != nil {
		slog.Error("tus mark finalizing", "id", id, "err", err)
		return
	}
	if won {
		go s.runFinalize(id)
	}
}

// runFinalize ingests a fully-received upload into a book, off the request path.
// A bad EPUB is a permanent failure (staging dropped). A storage/db failure is
// transient: the status is set to "error" but the staging bytes are KEPT, so the
// client can retry by re-issuing the final PATCH rather than re-uploading.
func (s *Server) runFinalize(id string) {
	u, err := getUpload(s.db, id)
	if err != nil || u == nil {
		slog.Error("tus finalize: load upload", "id", id, "err", err)
		return
	}
	data, err := os.ReadFile(s.stagingPath(id))
	if err != nil {
		_ = setUploadError(s.db, id, "could not read staged upload")
		slog.Error("tus finalize: read staging", "id", id, "err", err)
		return
	}
	if _, _, _, perr := parseEPUB(data); perr != nil {
		_ = setUploadError(s.db, id, perr.Error())
		s.removeStaging(id) // permanent: the bytes will never be a valid EPUB
		return
	}
	b, err := s.storeUpload(data, u.Filename, u.Collection)
	if err != nil {
		_ = setUploadError(s.db, id, err.Error()) // transient: keep staging for retry
		slog.Error("tus finalize: store", "id", id, "err", err)
		return
	}
	if err := setUploadDone(s.db, id, b.CID); err != nil {
		slog.Error("tus finalize: mark done", "id", id, "err", err)
		return
	}
	s.removeStaging(id)
}

// resumeFinalizing re-launches finalizes that a restart interrupted — their
// staging bytes are complete on the data volume, only the ingest needs redoing.
func (s *Server) resumeFinalizing() {
	ids, err := uploadIDsByStatus(s.db, uploadFinalizing)
	if err != nil {
		slog.Warn("resume finalizing uploads", "err", err)
		return
	}
	for _, id := range ids {
		go s.runFinalize(id)
	}
}

// removeStaging deletes one upload's staging file, tolerating an already-absent
// file.
func (s *Server) removeStaging(id string) {
	if err := os.Remove(s.stagingPath(id)); err != nil && !os.IsNotExist(err) {
		slog.Error("remove staging file", "id", id, "err", err)
	}
}

// discardUpload removes an upload's staging file and row, logging but not
// failing on cleanup errors.
func (s *Server) discardUpload(id string) {
	s.removeStaging(id)
	if err := deleteUpload(s.db, id); err != nil {
		slog.Error("delete upload row", "id", id, "err", err)
	}
}

// tusFail logs an internal error and returns a 500 with the protocol header.
func (s *Server) tusFail(w http.ResponseWriter, what string, err error) {
	slog.Error("tus: "+what, "err", err)
	web.WriteError(w, http.StatusInternalServerError, "internal error")
}

// parseTusMetadata decodes the tus Upload-Metadata header — comma-separated
// "key base64(value)" pairs — pulling out the filename and collection a client
// attaches to an upload. Unknown keys are ignored.
func parseTusMetadata(header string) (filename, collection string) {
	for _, pair := range strings.Split(header, ",") {
		fields := strings.Fields(pair)
		if len(fields) == 0 {
			continue
		}
		val := ""
		if len(fields) >= 2 {
			if dec, err := base64.StdEncoding.DecodeString(fields[1]); err == nil {
				val = string(dec)
			}
		}
		switch fields[0] {
		case "filename":
			filename = val
		case "collection":
			collection = val
		}
	}
	return sanitizeFilename(filename), strings.TrimSpace(collection)
}

// firstErr returns the first non-nil error.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
