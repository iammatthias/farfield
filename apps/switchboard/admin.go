package main

import (
	"net/http"
	"strconv"

	"github.com/iammatthias/farfield/lib/web"
)

// The private admin API (web.PrivateAPI): read-only inspection of the message
// log and the agent's jobs, for a native client on the tailnet. Nothing here
// acts — no replay, no cancel — and nothing exposes what the console does not:
// the webhook secret and Photon credentials never leave the process, and the
// thread identifiers (chat GUID, webhook delivery id) are projected out. The
// sender is the allowlisted handle the console already shows in full.

func (s *Server) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/messages", s.auth.PrivateAPI(s.handleAdminMessages))
	mux.HandleFunc("GET /api/admin/jobs", s.auth.PrivateAPI(s.handleAdminJobs))
	mux.HandleFunc(web.AdminPrefix, s.auth.PrivateAPI(web.AdminNotFound))
}

// adminMaxLimit caps one admin read. The log is bounded by retention anyway;
// this bounds the response.
const adminMaxLimit = 500

// adminLimit reads ?limit=, defaulting to the console's page size.
func adminLimit(r *http.Request) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		return min(n, adminMaxLimit)
	}
	return logPageSize
}

// messageView is one logged exchange. Switchboard records a message per
// inbound text, with the reply it sent back on the same row, so every entry
// is direction "inbound" and the outbound side is its reply.
type messageView struct {
	ID         string `json:"id"`
	Direction  string `json:"direction"`
	Sender     string `json:"sender"`
	Body       string `json:"body"`
	Route      string `json:"route"`
	Ref        string `json:"ref"`
	Reply      string `json:"reply"`
	Status     string `json:"status"`
	ReceivedAt string `json:"receivedAt"`
}

func (s *Server) handleAdminMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := listMessages(s.db, adminLimit(r))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list messages")
		return
	}
	out := make([]messageView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageView{
			ID: m.ID, Direction: "inbound", Sender: m.Sender, Body: m.Body,
			Route: m.Route, Ref: m.Ref, Reply: m.Reply, Status: m.Status,
			ReceivedAt: m.ReceivedAt,
		})
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"messages": out})
}

// jobView is one agent turn, without the chat GUID it replies into.
type jobView struct {
	ID         string `json:"id"`
	MessageID  string `json:"messageId"`
	Sender     string `json:"sender"`
	Prompt     string `json:"prompt"`
	Status     string `json:"status"`
	Result     string `json:"result"`
	Error      string `json:"error"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
}

func (s *Server) handleAdminJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := listRecentJobs(s.db, adminLimit(r))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list jobs")
		return
	}
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobView{
			ID: j.ID, MessageID: j.MessageID, Sender: j.Sender, Prompt: j.Prompt,
			Status: j.Status, Result: j.Result, Error: j.Error,
			StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		})
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"jobs": out})
}
