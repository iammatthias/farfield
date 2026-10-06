package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

func writeDo(t *testing.T, srv *httptest.Server, method, path, ifMatch, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-API-Key", "write-secret")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// getETag is the ETag the single-record GET sends — the value every If-Match
// below is built from, so the tests pin write tags to read tags.
func getETag(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	resp, _ := writeDo(t, srv, "GET", path, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, resp.StatusCode)
	}
	return resp.Header.Get("ETag")
}

func TestEntryIfMatch(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		draft   bool
		ifMatch func(etag string, e *Entry) string
		want    int
	}{
		{"put absent", "PUT", false, func(string, *Entry) string { return "" }, 200},
		{"put current", "PUT", false, func(e string, _ *Entry) string { return e }, 200},
		{"put current draft", "PUT", true, func(e string, _ *Entry) string { return e }, 200},
		{"put stale", "PUT", false, func(string, *Entry) string { return `"bafystale"` }, 412},
		// The entry tag is not the bare CID: it covers publishedAt too, so a
		// client that kept only the CID holds no valid validator.
		{"put bare cid", "PUT", false, func(_ string, e *Entry) string { return `"` + e.CID + `"` }, 412},
		{"delete current", "DELETE", false, func(e string, _ *Entry) string { return e }, 200},
		{"delete stale", "DELETE", true, func(string, *Entry) string { return `"bafystale"` }, 412},
		{"delete absent", "DELETE", false, func(string, *Entry) string { return "" }, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ids := readTestServer(t)
			srv := httptest.NewServer(s.routes())
			defer srv.Close()
			slug := ids.pubSlug
			if tc.draft {
				slug = ids.draftSlug
			}
			stored, _ := getEntry(s.db, slug)
			etag := getETag(t, srv, "/api/entries/"+slug)

			body := `{"collection":"blog","title":"Edited","body":"new body","published":` +
				map[bool]string{true: "false", false: "true"}[tc.draft] + `}`
			resp, rb := writeDo(t, srv, tc.method, "/api/entries/"+slug, tc.ifMatch(etag, stored), body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, rb)
			}
			after, _ := getEntry(s.db, slug)
			switch {
			case tc.want == 412:
				var pf struct {
					Error   string
					Current Entry
				}
				_ = json.Unmarshal(rb, &pf)
				if pf.Error != "precondition failed" || pf.Current.Slug != slug || pf.Current.Body != stored.Body ||
					resp.Header.Get("ETag") != etag {
					t.Errorf("412 = %s ETag %q, want the stored entry under %s", rb, resp.Header.Get("ETag"), etag)
				}
				if after == nil || after.Body != stored.Body {
					t.Error("a refused write changed the entry")
				}
			case tc.method == "PUT":
				if resp.Header.Get("ETag") == etag {
					t.Error("PUT kept the old ETag")
				}
				if got := getETag(t, srv, "/api/entries/"+slug); got != resp.Header.Get("ETag") {
					t.Errorf("PUT ETag %q != GET ETag %q", resp.Header.Get("ETag"), got)
				}
			case tc.method == "DELETE":
				if after != nil {
					t.Error("delete left the entry live")
				}
			}
		})
	}
}

func TestEntryIfMatchMissing(t *testing.T) {
	s, _ := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	for _, m := range []string{"PUT", "DELETE"} {
		if resp, _ := writeDo(t, srv, m, "/api/entries/nope", `"x"`, `{"collection":"blog","title":"x"}`); resp.StatusCode != 404 {
			t.Errorf("%s missing with If-Match = %d, want 404", m, resp.StatusCode)
		}
	}
}

func TestEntryIfMatchConcurrent(t *testing.T) {
	s, ids := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	raceWriters(t, func() string { return getETag(t, srv, "/api/entries/"+ids.pubSlug) },
		func(etag, body string) int {
			resp, _ := writeDo(t, srv, "PUT", "/api/entries/"+ids.pubSlug, etag,
				`{"collection":"blog","title":"Hello","body":"`+body+`","published":true}`)
			return resp.StatusCode
		})
}

// raceWriters fires two writes holding the same version and requires exactly
// one 200 and one 412, for several rounds: a single round can serialize by
// luck and pass without the guard on the statement; ten in a row do not.
func raceWriters(t *testing.T, etag func() string, write func(etag, body string) int) {
	t.Helper()
	for round := 0; round < 10; round++ {
		tag := etag()
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = write(tag, "round "+string(rune('a'+round))+" writer "+string(rune('0'+i)))
			}()
		}
		wg.Wait()
		slices.Sort(codes)
		if codes[0] != 200 || codes[1] != 412 {
			t.Fatalf("round %d: statuses = %v, want exactly one 200 and one 412", round, codes)
		}
	}
}

func TestSeriesIfMatch(t *testing.T) {
	cases := []struct {
		name    string
		ifMatch string // "current" = the GET's tag
		want    int
	}{
		{"absent", "", 200},
		{"current", "current", 200},
		{"weak current", "weak", 200},
		{"stale", `"bafystale"`, 412},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := readTestServer(t)
			if err := upsertSeries(s.db, &Series{Slug: "gallery", Title: "G", Body: "old"}); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(s.routes())
			defer srv.Close()
			etag := getETag(t, srv, "/api/series/gallery")
			im := map[string]string{"": "", "current": etag, "weak": "W/" + etag}[tc.ifMatch]
			if im == "" && tc.ifMatch != "" {
				im = tc.ifMatch
			}
			resp, rb := writeDo(t, srv, "PUT", "/api/series/gallery", im, `{"body":"new"}`)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, rb)
			}
			if tc.want == 412 {
				if !strings.Contains(string(rb), `"body":"old"`) || resp.Header.Get("ETag") != etag {
					t.Errorf("412 = %s ETag %q", rb, resp.Header.Get("ETag"))
				}
				return
			}
			if got := getETag(t, srv, "/api/series/gallery"); got != resp.Header.Get("ETag") || got == etag {
				t.Errorf("PUT ETag %q, GET ETag %q, old %q", resp.Header.Get("ETag"), got, etag)
			}
		})
	}
}

func TestSeriesIfMatchConcurrent(t *testing.T) {
	s, _ := readTestServer(t)
	if err := upsertSeries(s.db, &Series{Slug: "gallery", Body: "old"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	raceWriters(t, func() string { return getETag(t, srv, "/api/series/gallery") },
		func(etag, body string) int {
			resp, _ := writeDo(t, srv, "PUT", "/api/series/gallery", etag, `{"body":"`+body+`"}`)
			return resp.StatusCode
		})
}

func TestAPIDeleteSeries(t *testing.T) {
	s, ids := readTestServer(t)
	for _, se := range []*Series{
		{Slug: "trip", Body: "a"},         // embedded by a live entry and a trashed one
		{Slug: "trip-2", Body: "b"},       // only "series://trip-2" text exists — not a ref to trip
		{Slug: "unused", Body: "c"},       // nothing embeds it
		{Slug: "guarded", Body: "d"},      // for the If-Match cases
		{Slug: "draft-only", Body: "e"},   // embedded by a draft only
		{Slug: "trashed-only", Body: "f"}, // embedded by a trashed entry only
	} {
		if err := upsertSeries(s.db, se); err != nil {
			t.Fatal(err)
		}
	}
	live := &Entry{Collection: "blog", Slug: "uses-trip", Title: "T", Published: true,
		Body: "intro\n\n![](series://trip)\n\nseries://trip-2"}
	gone := &Entry{Collection: "blog", Slug: "trashed", Title: "X", Published: true,
		Body: "series://trip\n\nseries://trashed-only"}
	for _, e := range []*Entry{live, gone} {
		if err := insertEntry(s.db, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := deleteEntry(s.db, gone.Slug); err != nil {
		t.Fatal(err)
	}
	draft, _ := getEntry(s.db, ids.draftSlug)
	draft.Body = "series://draft-only"
	if err := updateEntry(s.db, draft.Slug, draft); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	if resp, _ := writeDo(t, srv, "DELETE", "/api/series/unused", "", ""); resp.StatusCode != 200 {
		t.Errorf("delete unused = %d, want 200", resp.StatusCode)
	}
	for slug, refs := range map[string]float64{"trip": 2, "draft-only": 1, "trashed-only": 1} {
		resp, body := writeDo(t, srv, "DELETE", "/api/series/"+slug, "", "")
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		if resp.StatusCode != http.StatusConflict || got["error"] != "series is still referenced" || got["references"] != refs {
			t.Errorf("delete %s = %d %s, want 409 with %v references", slug, resp.StatusCode, body, refs)
		}
		if se, _ := getSeries(s.db, slug); se == nil {
			t.Errorf("a refused delete removed %s", slug)
		}
	}
	// trip-2's only mention is in the same body as trip's; it is a reference
	// to trip-2, so trip-2 is referenced and trip's count is not inflated by it.
	if resp, _ := writeDo(t, srv, "DELETE", "/api/series/trip-2", "", ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("delete trip-2 = %d, want 409", resp.StatusCode)
	}

	etag := getETag(t, srv, "/api/series/guarded")
	if resp, _ := writeDo(t, srv, "DELETE", "/api/series/guarded", `"bafystale"`, ""); resp.StatusCode != 412 {
		t.Errorf("stale If-Match = %d, want 412", resp.StatusCode)
	}
	resp, body := writeDo(t, srv, "DELETE", "/api/series/guarded", etag, "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"deleted":"guarded"`) {
		t.Errorf("current If-Match = %d %s", resp.StatusCode, body)
	}
	if resp, _ := writeDo(t, srv, "DELETE", "/api/series/guarded", "", ""); resp.StatusCode != 404 {
		t.Errorf("delete missing = %d, want 404", resp.StatusCode)
	}
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/series/trip", nil)
	req.Header.Set("X-API-Key", "read-secret")
	if resp, err := srv.Client().Do(req); err != nil || resp.StatusCode != 401 {
		t.Errorf("read key delete = %v %v, want 401", resp.StatusCode, err)
	}
}

// Creates hand back the same ETag a GET of the new record sends.
func TestCreateETags(t *testing.T) {
	s, _ := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	resp, body := writeDo(t, srv, "POST", "/api/entries", "", `{"collection":"blog","title":"Fresh","body":"x"}`)
	var e Entry
	_ = json.Unmarshal(body, &e)
	if resp.StatusCode != 201 || resp.Header.Get("ETag") != getETag(t, srv, "/api/entries/"+e.Slug) {
		t.Errorf("entry create = %d ETag %q", resp.StatusCode, resp.Header.Get("ETag"))
	}
	resp, body = writeDo(t, srv, "POST", "/api/series", "", `{"title":"Fresh series","body":"x"}`)
	var se Series
	_ = json.Unmarshal(body, &se)
	if resp.StatusCode != 201 || resp.Header.Get("ETag") != getETag(t, srv, "/api/series/"+se.Slug) {
		t.Errorf("series create = %d ETag %q", resp.StatusCode, resp.Header.Get("ETag"))
	}
}
