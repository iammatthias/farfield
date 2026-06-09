package main

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

// handleBackupPut stores a database backup snapshot in R2. Snapshots are
// opaque, content-addressed bytes — deliberately not registered in the media
// index, so they never appear in the blob grid or the public /blobs API.
func (s *Server) handleBackupPut(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		web.WriteError(w, http.StatusRequestEntityTooLarge, "snapshot too large")
		return
	}
	if len(data) == 0 {
		web.WriteError(w, http.StatusBadRequest, "empty snapshot")
		return
	}
	cid := BlobCID(data)
	if err := s.store.Put(cid, data, "application/x-sqlite3"); err != nil {
		slog.Error("store backup", "cid", cid, "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not store snapshot")
		return
	}
	web.WriteJSON(w, http.StatusCreated, map[string]any{"cid": cid, "size": len(data)})
}

// handleBackupGet serves a backup snapshot's bytes from R2.
func (s *Server) handleBackupGet(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	data, err := s.store.Get(cid)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read snapshot")
		return
	}
	if data == nil {
		web.WriteError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	w.Header().Set("Content-Type", "application/x-sqlite3")
	_, _ = w.Write(data)
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
