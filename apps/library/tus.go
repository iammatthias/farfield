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

// handleTusHead reports how many bytes the server already holds, so a client can
// resume from the right offset.
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
	h.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	h.Set("Upload-Length", strconv.FormatInt(u.Length, 10))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// handleTusPatch appends one chunk at the declared offset. When the upload
// reaches its full length the bytes are finalized into a book and the resulting
// CID is returned in the X-Library-Cid response header.
func (s *Server) handleTusPatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tus-Resumable", tusVersion)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), tusOctet) {
		web.WriteError(w, http.StatusUnsupportedMediaType, "Content-Type must be "+tusOctet)
		return
	}
	u, err := getUpload(s.db, r.PathValue("id"))
	if err != nil {
		s.tusFail(w, "read upload", err)
		return
	}
	if u == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil {
		web.WriteError(w, http.StatusBadRequest, "missing or invalid Upload-Offset")
		return
	}
	cur, err := s.stagingSize(u.ID)
	if err != nil {
		s.tusFail(w, "stat staging file", err)
		return
	}
	if offset != cur {
		web.WriteError(w, http.StatusConflict, "Upload-Offset does not match server state")
		return
	}

	f, err := os.OpenFile(s.stagingPath(u.ID), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		s.tusFail(w, "open staging file", err)
		return
	}
	// Never store past the declared length, even if the client over-sends.
	written, copyErr := io.Copy(f, io.LimitReader(r.Body, u.Length-cur))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		s.tusFail(w, "write chunk", firstErr(copyErr, closeErr))
		return
	}
	newOffset := cur + written
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	if newOffset < u.Length {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Complete: turn the assembled bytes into a book.
	book, err := s.finalizeUpload(u)
	if err != nil {
		// All bytes arrived but they aren't a valid EPUB — drop the upload and
		// tell the client why, rather than leaving a dead staging file behind.
		s.discardUpload(u.ID)
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("X-Library-Cid", book.CID)
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

// finalizeUpload reads the completed staging file and ingests it as a book,
// then removes the staging file and upload row. The bytes are read into memory
// for metadata extraction and content addressing — the same path a direct
// POST /api/books takes.
func (s *Server) finalizeUpload(u *Upload) (*Book, error) {
	data, err := os.ReadFile(s.stagingPath(u.ID))
	if err != nil {
		return nil, err
	}
	b, err := s.storeUpload(data, u.Filename, u.Collection)
	if err != nil {
		return nil, err
	}
	s.discardUpload(u.ID)
	return b, nil
}

// discardUpload removes an upload's staging file and row, logging but not
// failing on cleanup errors.
func (s *Server) discardUpload(id string) {
	if err := os.Remove(s.stagingPath(id)); err != nil && !os.IsNotExist(err) {
		slog.Error("remove staging file", "id", id, "err", err)
	}
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
