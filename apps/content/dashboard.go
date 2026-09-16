package main

import "net/http"

// The dashboard: collections and their counts, the first thing an author sees.

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	recent, err := listEntries(s.db, "", statusAll, 12)
	if err != nil {
		s.fail(w, "list entries", err)
		return
	}
	total, err := countEntries(s.db, "", statusAll)
	if err != nil {
		s.fail(w, "count entries", err)
		return
	}
	s.rd.Render(w, "dashboard.html", map[string]any{
		"Collections": collections,
		"Entries":     recent,
		"TotalCount":  total,
	})
}
