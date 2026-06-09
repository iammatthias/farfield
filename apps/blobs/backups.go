package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/web"
)

// handleBackupPut stores a database backup snapshot in R2. Snapshots are
// opaque, content-addressed bytes — deliberately not registered in the media
// index, so they never appear in the blob grid or the public /blobs API.
// The body is spooled to a temp file while being hashed, so a multi-hundred-MB
// database snapshot never sits in this process's memory.
func (s *Server) handleBackupPut(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	tmp, err := os.CreateTemp("", "snapshot-*.sqlite")
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not spool snapshot")
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	c, size, err := cid.OfReader(io.TeeReader(r.Body, tmp))
	if err != nil {
		web.WriteError(w, http.StatusRequestEntityTooLarge, "snapshot too large")
		return
	}
	if size == 0 {
		web.WriteError(w, http.StatusBadRequest, "empty snapshot")
		return
	}
	if err := s.store.PutFile(c, tmp.Name(), "application/x-sqlite3"); err != nil {
		slog.Error("store backup", "cid", c, "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not store snapshot")
		return
	}
	web.WriteJSON(w, http.StatusCreated, map[string]any{"cid": c, "size": size})
}

// handleBackupGet streams a backup snapshot's bytes from the store.
func (s *Server) handleBackupGet(w http.ResponseWriter, r *http.Request) {
	c := r.PathValue("cid")
	if !validCID(c) {
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	body, size, err := s.store.GetStream(c)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read snapshot")
		return
	}
	if body == nil {
		web.WriteError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/x-sqlite3")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	_, _ = io.Copy(w, body)
}

// handleBackupDelete removes a backup snapshot's bytes from R2.
func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	if err := s.store.Delete(cid); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete snapshot")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": cid})
}
