package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// newProfileTestServer wires a profileServer to stub siblings. The stubs answer
// in each service's real response shape, and record whether the read key
// arrived — the keys staying server-side is the reason this endpoint exists.
func newProfileTestServer(t *testing.T, feedBody, contentBody, dailyBody string, status int) (*profileServer, *[]string) {
	t.Helper()
	var keyed []string
	stub := func(name, body string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") != "" {
				keyed = append(keyed, name)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}))
		t.Cleanup(s.Close)
		return s
	}
	p := &profileServer{
		feedURL: stub("feed", feedBody).URL, feedKey: "fk",
		contentURL: stub("content", contentBody).URL, contentKey: "ck",
		dailyURL: stub("daily", dailyBody).URL,
		rl:       web.NewRateLimiter(1000, time.Minute),
	}
	return p, &keyed
}

const (
	stubFeed    = `{"posts":[{"slug":"a-walk","body":"a long walk\n\n![](blob://bafyimg1)","createdAt":"2026-08-24T18:00:00Z"}]}`
	stubContent = `{"entries":[
		{"collection":"posts","slug":"new","title":"The [New] One","excerpt":"fresh","createdAt":"2026-08-20T00:00:00Z"},
		{"collection":"feed","slug":"noise","title":"never","createdAt":"2026-08-25T00:00:00Z"},
		{"collection":"art","slug":"plate","title":"A Plate","createdAt":"2026-08-19T00:00:00Z"},
		{"collection":"recipes","slug":"bread","title":"Bread","excerpt":"crusty","createdAt":"2026-08-18T00:00:00Z"},
		{"collection":"open-source","slug":"tool","title":"Tool","createdAt":"2026-08-17T00:00:00Z"}]}`
	stubDaily = `{"date":"2026-08-25","biome":"shoal","zone":{"name":"slate"}}`
)

func fetchProfile(t *testing.T, p *profileServer) (profileDoc, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	p.handle(w, httptest.NewRequest("GET", "/api/profile", nil))
	var doc profileDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, w.Body.String())
	}
	return doc, w
}

// TestProfileRendersAllSections pins the contract the README splices against.
func TestProfileRendersAllSections(t *testing.T) {
	p, keyed := newProfileTestServer(t, stubFeed, stubContent, stubDaily, 200)
	doc, w := fetchProfile(t, p)

	if len(doc.Sections) != 3 {
		t.Fatalf("sections = %v, want feed/writing/daily", doc.Sections)
	}

	feed := doc.Sections["feed"]
	if !strings.Contains(feed, "> a long walk") {
		t.Errorf("feed = %q, want the body quoted", feed)
	}
	if strings.Contains(feed, "blob://") {
		t.Errorf("feed leaked embed syntax: %q", feed)
	}
	if !strings.Contains(feed, "https://blobs.farfield.systems/blobs/bafyimg1") {
		t.Errorf("feed = %q, want the image resolved to a public URL", feed)
	}
	if !strings.Contains(feed, "https://iammatthias.com/feed/a-walk") {
		t.Errorf("feed = %q, want the permalink", feed)
	}

	writing := doc.Sections["writing"]
	// Newest three from the allowed collections, in order — and the feed
	// collection, though newest of all, must never appear.
	for _, want := range []string{"The \\[New\\] One", "A Plate", "Bread"} {
		if !strings.Contains(writing, want) {
			t.Errorf("writing = %q, want %q", writing, want)
		}
	}
	if strings.Contains(writing, "never") {
		t.Errorf("writing includes the feed collection: %q", writing)
	}
	if strings.Contains(writing, "Tool") {
		t.Errorf("writing = %q — the fourth entry should be cut at three", writing)
	}
	if !strings.Contains(writing, "https://iammatthias.com/posts/new") {
		t.Errorf("writing = %q, want site links", writing)
	}

	daily := doc.Sections["daily"]
	if !strings.Contains(daily, "art/2026-08-25.svg") || !strings.Contains(daily, "shoal · slate · 2026-08-25") {
		t.Errorf("daily = %q", daily)
	}

	// The read keys went to feed and content; daily is public and never sees one.
	got := strings.Join(*keyed, ",")
	if !strings.Contains(got, "feed") || !strings.Contains(got, "content") || strings.Contains(got, "daily") {
		t.Errorf("keys sent to: %v", *keyed)
	}

	if w.Header().Get("ETag") == "" {
		t.Error("no ETag")
	}
}

// TestProfileOmitsAFailedSection: a sibling being down loses its section, not
// the document — the splicer keeps the old content for an absent key.
func TestProfileOmitsAFailedSection(t *testing.T) {
	p, _ := newProfileTestServer(t, stubFeed, stubContent, stubDaily, 200)
	p.feedURL = "http://127.0.0.1:1" // nothing listens

	doc, _ := fetchProfile(t, p)
	if _, present := doc.Sections["feed"]; present {
		t.Error("a failed section should be absent, not empty")
	}
	if len(doc.Sections) != 2 {
		t.Errorf("sections = %v, want writing and daily to survive", doc.Sections)
	}
}

// TestProfileServesStaleOverNothing: once a document has been built, upstream
// failure serves the cached one rather than a blank.
func TestProfileServesStaleOverNothing(t *testing.T) {
	p, _ := newProfileTestServer(t, stubFeed, stubContent, stubDaily, 200)
	first, _ := fetchProfile(t, p)
	if len(first.Sections) != 3 {
		t.Fatalf("first build: %v", first.Sections)
	}

	// Everything goes down, and the cache ages out.
	p.feedURL, p.contentURL, p.dailyURL = "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1"
	p.renewed = time.Time{}

	again, _ := fetchProfile(t, p)
	if len(again.Sections) != 3 {
		t.Errorf("stale document not served: %v", again.Sections)
	}
}

// TestProfileWordlessPost: an image-only post is just the image and the
// permalink line. A blockquote asserts "these were the post's words", so no
// text is ever invented to fill one — a placeholder that did got read as his.
func TestProfileWordlessPost(t *testing.T) {
	feed := `{"posts":[{"slug":"pic","body":"![](blob://bafyonly)","createdAt":"2026-08-24T00:00:00Z"}]}`
	p, _ := newProfileTestServer(t, feed, stubContent, stubDaily, 200)
	doc, _ := fetchProfile(t, p)
	got := doc.Sections["feed"]
	if strings.Contains(got, ">") && strings.Contains(got, "transmission") {
		t.Errorf("feed = %q — invented words in a blockquote", got)
	}
	if strings.Contains(got, "> ") {
		t.Errorf("feed = %q — an image-only post must have no blockquote at all", got)
	}
	if !strings.Contains(got, "bafyonly") || !strings.Contains(got, "iammatthias.com/feed/pic") {
		t.Errorf("feed = %q, want the image and the permalink", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "<img") {
		t.Errorf("feed = %q, want it to open with the image", got)
	}
}

// TestProfileEmptyPostOmitsSection: no words and no image means no section,
// per the absent-not-empty contract.
func TestProfileEmptyPostOmitsSection(t *testing.T) {
	feed := `{"posts":[{"slug":"blank","body":"","createdAt":"2026-08-24T00:00:00Z"}]}`
	p, _ := newProfileTestServer(t, feed, stubContent, stubDaily, 200)
	doc, _ := fetchProfile(t, p)
	if _, present := doc.Sections["feed"]; present {
		t.Errorf("feed = %q, want the section absent", doc.Sections["feed"])
	}
}

// TestProfileCacheStampsETag: the same document answers 304 to its own tag.
func TestProfileCacheStampsETag(t *testing.T) {
	p, _ := newProfileTestServer(t, stubFeed, stubContent, stubDaily, 200)
	_, w := fetchProfile(t, p)
	etag := strings.Trim(w.Header().Get("ETag"), `"`)

	r := httptest.NewRequest("GET", "/api/profile", nil)
	r.Header.Set("If-None-Match", `"`+etag+`"`)
	w2 := httptest.NewRecorder()
	p.handle(w2, r)
	if w2.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", w2.Code)
	}
}
