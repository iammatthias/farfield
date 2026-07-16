package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSiteURL pins the template substitution and the two "off" states: no
// SITE_URL_TEMPLATE configured, and an unpublished entry.
func TestSiteURL(t *testing.T) {
	s := &Server{siteURLTmpl: "https://example.com/{collection}/{slug}"}
	pub := &Entry{Collection: "blog", Slug: "hi", Published: true}
	if got, want := s.siteURL(pub), "https://example.com/blog/hi"; got != want {
		t.Errorf("siteURL(published) = %q, want %q", got, want)
	}
	if got := s.siteURL(&Entry{Collection: "blog", Slug: "wip"}); got != "" {
		t.Errorf("siteURL(draft) = %q, want empty (no public page yet)", got)
	}
	if got := (&Server{}).siteURL(pub); got != "" {
		t.Errorf("siteURL(no template) = %q, want empty (feature off)", got)
	}
}

// TestEditPageViewOnSite confirms the edit page renders the link for a
// published entry and omits it for a draft.
func TestEditPageViewOnSite(t *testing.T) {
	s, ids := readTestServer(t)
	s.siteURLTmpl = "https://example.com/{collection}/{slug}"
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	cookie := sessionCookie(t, s)

	code, page := adminGet(t, srv, cookie, "/entries/"+ids.pubSlug+"/edit")
	if code != 200 {
		t.Fatalf("edit page = %d, want 200", code)
	}
	if !strings.Contains(page, "https://example.com/blog/"+ids.pubSlug) ||
		!strings.Contains(page, "View on site") {
		t.Errorf("published edit page lacks the view-on-site link")
	}
	if _, draftPage := adminGet(t, srv, cookie, "/entries/"+ids.draftSlug+"/edit"); strings.Contains(draftPage, "View on site") {
		t.Errorf("draft edit page shows a view-on-site link, want none")
	}
}
