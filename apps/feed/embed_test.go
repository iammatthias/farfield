package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// TestEmbedBlobsProxy confirms the editor's blob-gallery proxy forwards to the
// blobs service with the server-side key and is session-gated — so the browser
// never needs the now-token-gated blobs index directly.
func TestEmbedBlobsProxy(t *testing.T) {
	var gotKey, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"blobs":[{"cid":"bafkreiexample"}],"pages":1}`))
	}))
	defer upstream.Close()

	db, err := openDB(filepath.Join(t.TempDir(), "feed.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{
		db:       db,
		auth:     &web.Auth{DB: db, Password: "pw", APIKey: "write"},
		blobsURL: upstream.URL,
		blobsKey: "blobs-key",
	}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// No session → redirect to login.
	resp, err := client.Get(srv.URL + "/embed/blobs?page=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/embed/blobs without session = %d, want 303", resp.StatusCode)
	}

	// With a session, it proxies — key attached server-side, query forwarded.
	tok := auth.NewSessionToken()
	if err := store.InsertSession(db, tok, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/embed/blobs?page=2", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: tok})
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/embed/blobs with session = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "bafkreiexample") {
		t.Errorf("proxied body = %q, want upstream blobs", body)
	}
	if gotKey != "blobs-key" {
		t.Errorf("upstream X-API-Key = %q, want the server-side key", gotKey)
	}
	if gotQuery != "page=2" {
		t.Errorf("upstream query = %q, want page=2 forwarded", gotQuery)
	}
}
