package main

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

// The private admin API (web.PrivateAPI): the session console's share table
// and its revoke button, for a native client on the tailnet.

func (s *Server) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/shares", s.auth.PrivateAPI(s.handleAdminShares))
	mux.HandleFunc("POST /api/admin/shares/{token}/revoke", s.auth.PrivateAPI(s.handleAdminShareRevoke))
	mux.HandleFunc(web.AdminPrefix, s.auth.PrivateAPI(web.AdminNotFound))
}

// shareView is a share link as the admin API reports it. installs is the
// delivered-install count; live is the console's "can a fresh install start
// on this link right now" — active, unexpired, and under its cap. The last
// installer's address and user agent stay out, as they do on the console.
type shareView struct {
	Token       string `json:"token"`
	BuildID     string `json:"buildId"`
	AppName     string `json:"appName"`
	Version     string `json:"version"`
	Label       string `json:"label"`
	State       string `json:"state"`
	ExpiresAt   string `json:"expiresAt"`
	MaxInstalls int    `json:"maxInstalls"`
	Installs    int    `json:"installs"`
	Revoked     bool   `json:"revoked"`
	Live        bool   `json:"live"`
	CreatedAt   string `json:"createdAt"`
	ConsumedAt  string `json:"consumedAt,omitempty"`
	ShareURL    string `json:"shareURL"`
}

func (s *Server) shareView(sh *shareRow) shareView {
	return shareView{
		Token:       sh.Token.Token,
		BuildID:     sh.BuildID,
		AppName:     sh.AppName,
		Version:     sh.Version,
		Label:       sh.Label,
		State:       sh.State,
		ExpiresAt:   sh.ExpiresAt,
		MaxInstalls: sh.MaxInstalls,
		Installs:    sh.UsedInstalls,
		Revoked:     sh.State == stateRevoked,
		Live:        sh.canStart(),
		CreatedAt:   sh.CreatedAt,
		ConsumedAt:  sh.ConsumedAt,
		ShareURL:    s.publicURL + "/s/" + sh.Token.Token,
	}
}

func (s *Server) handleAdminShares(w http.ResponseWriter, r *http.Request) {
	shares, err := listShares(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list shares")
		return
	}
	out := make([]shareView, 0, len(shares))
	for i := range shares {
		out = append(out, s.shareView(&shares[i]))
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"shares": out})
}

// handleAdminShareRevoke is the console's revoke: the same revokeToken, then
// the share as it now stands. Revoking a link that is already revoked,
// consumed, or expired changes nothing and still answers 200 with its state —
// the caller asked for it not to work, and it does not. Only a share token
// counts; a build's self token is not a share and is a 404 here.
func (s *Server) handleAdminShareRevoke(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sh, err := getShare(s.db, token)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read share")
		return
	}
	if sh == nil {
		web.WriteError(w, http.StatusNotFound, "share not found")
		return
	}
	if _, err := revokeToken(s.db, token); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not revoke share")
		return
	}
	if sh, err = getShare(s.db, token); err != nil || sh == nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read share")
		return
	}
	web.WriteJSON(w, http.StatusOK, s.shareView(sh))
}
