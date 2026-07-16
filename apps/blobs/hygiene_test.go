package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iammatthias/farfield/lib/web"
)

// hygieneFixture is the store the hygiene tests run against:
//
//	cidA — referenced by a content entry and a content series
//	cidB — referenced by nothing (the orphan)
//	cidC — referenced by a feed post; its generated thumbnail is cidT
//	cidT — cidC's thumbnail, also present as its own row (dedupe upload);
//	       derived, so never an orphan
//	cidM — referenced by the entry but absent from the store (missing)
var (
	cidA = BlobCID([]byte("blob a"))
	cidB = BlobCID([]byte("blob b"))
	cidC = BlobCID([]byte("blob c"))
	cidT = BlobCID([]byte("thumb of c"))
	cidM = BlobCID([]byte("missing"))
)

// newHygieneServer builds a Server over a fresh database seeded with the
// fixture rows, pointing its hygiene sources at the given service URLs.
func newHygieneServer(t *testing.T, contentURL, feedURL string) *Server {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "blobs.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, m := range []Meta{
		{CID: cidA, Mime: "image/png", Size: 10, CreatedAt: "2026-07-01T00:00:00Z"},
		{CID: cidB, Mime: "image/png", Size: 20, CreatedAt: "2026-07-02T00:00:00Z"},
		{CID: cidC, Mime: "image/png", Size: 30, ThumbCID: cidT, CreatedAt: "2026-07-03T00:00:00Z"},
		{CID: cidT, Mime: "image/jpeg", Size: 5, CreatedAt: "2026-07-04T00:00:00Z"},
	} {
		if err := upsertMeta(db, &m); err != nil {
			t.Fatalf("upsertMeta(%s): %v", m.CID, err)
		}
	}
	tmpl, err := web.ParseTemplates(assets, tmplFuncs)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	return &Server{
		db:   db,
		auth: &web.Auth{DB: db},
		rd:   &web.Renderer{Templates: tmpl, Funcs: tmplFuncs},
		sources: hygieneSources{
			ContentURL: contentURL,
			ContentKey: "write",
			FeedURL:    feedURL,
		},
	}
}

// fakeContent stands in for the content service. status=all demands the write
// key ("write"), like the real resolveStatus; without it only the published
// entry is listed and the draft's references go unseen.
func fakeContent(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/entries":
			all := r.URL.Query().Get("status") == "all"
			if all && r.Header.Get("X-API-Key") != "write" {
				web.WriteError(w, http.StatusForbidden, "drafts require the write API key")
				return
			}
			entries := []map[string]string{{
				"slug": "hello", "title": "Hello",
				"body": "![a](blob://" + cidA + ")\n\nblob://" + cidM,
			}}
			if all {
				entries = append(entries, map[string]string{
					"slug": "wip", "title": "WIP", "body": "blob://" + cidA,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
		case "/api/series":
			_ = json.NewEncoder(w).Encode(map[string]any{"series": []map[string]string{
				{"slug": "gallery", "title": "Gallery", "body": "![a](blob://" + cidA + ")"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeFeed stands in for the feed service: one post referencing cidC.
func fakeFeed(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/posts" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"posts": []map[string]string{
			{"slug": "20260701120000", "body": "![c](blob://" + cidC + ")", "createdAt": "2026-07-01T12:00:00Z"},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHygieneReport(t *testing.T) {
	s := newHygieneServer(t, fakeContent(t).URL, fakeFeed(t).URL)
	rep, err := s.buildHygiene(context.Background())
	if err != nil {
		t.Fatalf("buildHygiene: %v", err)
	}
	if !rep.SourcesOK() || rep.DraftsSkipped {
		t.Fatalf("sourcesOK=%v draftsSkipped=%v, want true/false (errs: %v)",
			rep.SourcesOK(), rep.DraftsSkipped, rep.SourceErrors)
	}

	// References grouped per CID, with kind, slug, and title carried through.
	refs := map[string][]blobRef{}
	for _, rb := range rep.Referenced {
		refs[rb.Meta.CID] = rb.Refs
	}
	if len(refs) != 2 {
		t.Fatalf("referenced = %d blobs, want 2 (%v)", len(refs), refs)
	}
	// cidA: published entry + draft entry + series.
	if got := len(refs[cidA]); got != 3 {
		t.Errorf("refs[cidA] = %d, want 3 (%v)", got, refs[cidA])
	}
	kinds := map[string]bool{}
	for _, ref := range refs[cidA] {
		kinds[ref.Kind] = true
	}
	if !kinds["entry"] || !kinds["series"] {
		t.Errorf("cidA ref kinds = %v, want entry and series", kinds)
	}
	if got := refs[cidC]; len(got) != 1 || got[0].Kind != "post" || got[0].Slug != "20260701120000" {
		t.Errorf("refs[cidC] = %v, want one post ref", got)
	}

	// cidB is the only orphan: cidT is derived (it is cidC's thumbnail).
	if len(rep.Orphans) != 1 || rep.Orphans[0].CID != cidB {
		t.Errorf("orphans = %v, want just cidB", rep.Orphans)
	}
	if rep.DerivedThumbs != 1 {
		t.Errorf("derivedThumbs = %d, want 1", rep.DerivedThumbs)
	}

	// cidM is referenced but not stored.
	if len(rep.Missing) != 1 || rep.Missing[0].CID != cidM {
		t.Fatalf("missing = %v, want just cidM", rep.Missing)
	}
	if got := rep.Missing[0].Refs; len(got) != 1 || got[0].Kind != "entry" || got[0].Slug != "hello" {
		t.Errorf("missing refs = %v, want the hello entry", got)
	}
}

func TestHygienePageOffersDeleteWhenSourcesOK(t *testing.T) {
	s := newHygieneServer(t, fakeContent(t).URL, fakeFeed(t).URL)
	rec := httptest.NewRecorder()
	s.handleHygiene(rec, httptest.NewRequest("GET", "/hygiene", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /hygiene = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/blobs/"+cidB+"/delete") {
		t.Errorf("page does not offer deleting the orphan %s", cidB)
	}
	if strings.Contains(body, "/blobs/"+cidA+"/delete") {
		t.Errorf("page offers deleting the referenced blob %s", cidA)
	}
}

func TestHygieneDraftsSkippedWithoutWriteKey(t *testing.T) {
	s := newHygieneServer(t, fakeContent(t).URL, fakeFeed(t).URL)
	s.sources.ContentKey = "" // content will 403 the status=all request
	rep, err := s.buildHygiene(context.Background())
	if err != nil {
		t.Fatalf("buildHygiene: %v", err)
	}
	if !rep.DraftsSkipped {
		t.Error("draftsSkipped = false, want true after the status=all 403")
	}
	if !rep.SourcesOK() {
		t.Errorf("published-only fallback should still succeed (errs: %v)", rep.SourceErrors)
	}
	// The published entry was still scanned.
	found := false
	for _, rb := range rep.Referenced {
		if rb.Meta.CID == cidA {
			found = true
		}
	}
	if !found {
		t.Error("published entry references were lost in the fallback")
	}
}

// TestHygieneFetchFailureHidesDeletion is the safety property: when a source
// cannot be scanned, everything it references would look orphaned, so the
// page must warn and must not render any delete form.
func TestHygieneFetchFailureHidesDeletion(t *testing.T) {
	deadFeed := httptest.NewServer(http.NotFoundHandler())
	deadFeed.Close() // keep the URL, kill the listener
	s := newHygieneServer(t, fakeContent(t).URL, deadFeed.URL)

	rec := httptest.NewRecorder()
	s.handleHygiene(rec, httptest.NewRequest("GET", "/hygiene", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /hygiene = %d, want 200 (partial data, not a 500)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="error"`) {
		t.Error("page does not warn about the unreachable source")
	}
	if strings.Contains(body, "/delete") {
		t.Error("page offers deletion although a source fetch failed")
	}
	// The report itself still carries the failure.
	rep, err := s.buildHygiene(context.Background())
	if err != nil {
		t.Fatalf("buildHygiene: %v", err)
	}
	if rep.SourcesOK() {
		t.Error("sourcesOK = true although feed is unreachable")
	}
}

// TestHygieneRouteIsSessionGated confirms /hygiene sits behind the admin
// session like the rest of the HTML UI.
func TestHygieneRouteIsSessionGated(t *testing.T) {
	s := newHygieneServer(t, "http://127.0.0.1:0", "http://127.0.0.1:0")
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv.URL + "/hygiene")
	if err != nil {
		t.Fatalf("GET /hygiene: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /hygiene without session = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("redirect target = %q, want /login", loc)
	}
}
