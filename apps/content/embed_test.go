package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// TestEmbedListsSessionGated confirms the editor's gallery-read proxies require
// a session (so they never need a read token in the browser) and that the
// series picker reads content's own table.
func TestEmbedListsSessionGated(t *testing.T) {
	s, _ := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// No session → RequireSession redirects to login for both proxies.
	for _, p := range []string{"/embed/blobs", "/embed/series"} {
		resp, err := client.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("%s without session = %d, want 303 redirect", p, resp.StatusCode)
		}
	}

	// With a session, the series picker returns content's own series list.
	tok := auth.NewSessionToken()
	if err := store.InsertSession(s.db, tok, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/embed/series", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: tok})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/embed/series with session = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Series []Series `json:"series"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
