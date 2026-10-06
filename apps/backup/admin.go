package main

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

// The private admin API (web.PrivateAPI): the snapshot registry, read-only.
// It is observational on purpose — taking, pruning and deleting snapshots
// stay on the session console, where the one service that can restore or
// destroy every database in the fleet keeps its only write surface.

func (s *Server) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/snapshots", s.auth.PrivateAPI(s.handleAdminSnapshots))
	mux.HandleFunc(web.AdminPrefix, s.auth.PrivateAPI(web.AdminNotFound))
}

func (s *Server) handleAdminSnapshots(w http.ResponseWriter, r *http.Request) {
	backups, err := listBackups(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list snapshots")
		return
	}
	if backups == nil {
		backups = []Backup{}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"snapshots": backups})
}
