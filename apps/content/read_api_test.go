package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

// readTestServer builds a content Server over a temp database seeded with one
// published entry and one draft, with a read key and a write key configured.
func readTestServer(t *testing.T) (*Server, pubDraft) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "content.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	tmpl, err := web.ParseTemplates(assets, nil)
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	s := &Server{
		db: db,
		auth: &web.Auth{
			DB:       db,
			Password: "pw",
			APIKey:   "write-secret",
			ReadKey:  "read-secret",
		},
		rd: &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
	}
	if err := insertCollection(db, &Collection{Slug: "blog", Name: "Blog"}); err != nil {
		t.Fatalf("collection: %v", err)
	}
	pub := &Entry{Collection: "blog", Slug: "hello", Title: "Hello", Body: "live", Published: true}
	if err := insertEntry(db, pub); err != nil {
		t.Fatalf("insert published: %v", err)
	}
	draft := &Entry{Collection: "blog", Slug: "wip", Title: "WIP", Body: "draft", Published: false}
	if err := insertEntry(db, draft); err != nil {
		t.Fatalf("insert draft: %v", err)
	}
	return s, pubDraft{pubSlug: pub.Slug, draftSlug: draft.Slug}
}

type pubDraft struct{ pubSlug, draftSlug string }

func apiGet(t *testing.T, srv *httptest.Server, path, token string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func entrySlugs(t *testing.T, body []byte) []string {
	t.Helper()
	var out struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode entries: %v (%s)", err, body)
	}
	slugs := make([]string, len(out.Entries))
	for i, e := range out.Entries {
		slugs[i] = e.Slug
	}
	return slugs
}

func TestReadGate(t *testing.T) {
	s, _ := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// /status stays public.
	if code, _ := apiGet(t, srv, "/status", ""); code != http.StatusOK {
		t.Errorf("/status without token = %d, want 200", code)
	}
	// Reads require a token once a read key is configured.
	if code, _ := apiGet(t, srv, "/api/entries", ""); code != http.StatusUnauthorized {
		t.Errorf("/api/entries without token = %d, want 401", code)
	}
	if code, _ := apiGet(t, srv, "/api/collections", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("/api/collections wrong token = %d, want 401", code)
	}
	// The read token reads published content.
	if code, _ := apiGet(t, srv, "/api/entries", "read-secret"); code != http.StatusOK {
		t.Errorf("/api/entries read token = %d, want 200", code)
	}
	// The write token also reads.
	if code, _ := apiGet(t, srv, "/api/entries", "write-secret"); code != http.StatusOK {
		t.Errorf("/api/entries write token = %d, want 200", code)
	}
}

func TestReadGateDraftsHiddenFromReadKey(t *testing.T) {
	s, ids := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Default list with the read key: published only.
	code, body := apiGet(t, srv, "/api/entries", "read-secret")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if slugs := entrySlugs(t, body); len(slugs) != 1 || slugs[0] != ids.pubSlug {
		t.Errorf("read-key list = %v, want only published %q", slugs, ids.pubSlug)
	}
	// status=all and status=draft require the write key.
	if code, _ := apiGet(t, srv, "/api/entries?status=all", "read-secret"); code != http.StatusForbidden {
		t.Errorf("read-key status=all = %d, want 403", code)
	}
	if code, _ := apiGet(t, srv, "/api/entries?status=draft", "read-secret"); code != http.StatusForbidden {
		t.Errorf("read-key status=draft = %d, want 403", code)
	}
	// A draft fetched by slug with the read key is a 404 (indistinguishable
	// from missing).
	if code, _ := apiGet(t, srv, "/api/entries/"+ids.draftSlug, "read-secret"); code != http.StatusNotFound {
		t.Errorf("read-key draft fetch = %d, want 404", code)
	}
}

func TestReadGateDraftsVisibleToWriteKey(t *testing.T) {
	s, ids := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// status=all returns both; status=draft returns only the draft.
	_, allBody := apiGet(t, srv, "/api/entries?status=all", "write-secret")
	if got := len(entrySlugs(t, allBody)); got != 2 {
		t.Errorf("write-key status=all count = %d, want 2", got)
	}
	_, draftBody := apiGet(t, srv, "/api/entries?status=draft", "write-secret")
	if slugs := entrySlugs(t, draftBody); len(slugs) != 1 || slugs[0] != ids.draftSlug {
		t.Errorf("write-key status=draft = %v, want only %q", slugs, ids.draftSlug)
	}
	// A draft is fetchable by slug with the write key — the preview path.
	if code, _ := apiGet(t, srv, "/api/entries/"+ids.draftSlug, "write-secret"); code != http.StatusOK {
		t.Errorf("write-key draft preview = %d, want 200", code)
	}
	// An unknown status value is a 400.
	if code, _ := apiGet(t, srv, "/api/entries?status=bogus", "write-secret"); code != http.StatusBadRequest {
		t.Errorf("status=bogus = %d, want 400", code)
	}
}
