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

func adminDo(t *testing.T, ts *httptest.Server, method, path, key string, headers map[string]string, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func seedBookmarks(t *testing.T, s *Server) (pub, priv *Bookmark) {
	t.Helper()
	pub = &Bookmark{URL: "https://example.com/a", Title: "A", Category: "zeta", Public: true, AdminNotes: "pub note"}
	priv = &Bookmark{URL: "https://example.com/b", Title: "B", Category: "Alpha", Public: false, AdminNotes: "secret note"}
	for _, b := range []*Bookmark{pub, priv} {
		if err := insertBookmark(s.db, b); err != nil {
			t.Fatal(err)
		}
	}
	return pub, priv
}

func TestAdminBookmarksGate(t *testing.T) {
	s := newTestServer(t)
	s.auth.ReadKey = "r1"
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	for _, tc := range []struct {
		name    string
		key     string
		headers map[string]string
		want    int
	}{
		{"tunnel", "k1", map[string]string{"Cf-Ray": "abc-SJC"}, 404},
		{"tunnel client ip", "k1", map[string]string{"Cf-Connecting-Ip": "203.0.113.4"}, 404},
		{"no key", "", nil, 401},
		{"read key", "r1", nil, 401},
		{"write key", "k1", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := adminDo(t, ts, "GET", "/api/admin/bookmarks", tc.key, tc.headers, "")
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
			if resp.Header.Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
			}
		})
	}
}

func TestAdminBookmarksListAndGet(t *testing.T) {
	s := newTestServer(t)
	pub, priv := seedBookmarks(t, s)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, body := adminDo(t, ts, "GET", "/api/admin/bookmarks", "k1", nil, "")
	if resp.StatusCode != 200 {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
	var list struct{ Bookmarks []Bookmark }
	_ = json.Unmarshal(body, &list)
	if len(list.Bookmarks) != 2 {
		t.Fatalf("list has %d bookmarks, want both public and private", len(list.Bookmarks))
	}
	// Public order: category case-folded, so "Alpha" sorts before "zeta".
	if list.Bookmarks[0].ID != priv.ID || list.Bookmarks[0].AdminNotes != "secret note" {
		t.Errorf("first = %+v, want the private Alpha bookmark with its notes", list.Bookmarks[0])
	}

	resp, body = adminDo(t, ts, "GET", "/api/admin/bookmarks/"+priv.ID, "k1", nil, "")
	var got Bookmark
	_ = json.Unmarshal(body, &got)
	if resp.StatusCode != 200 || got.AdminNotes != "secret note" || resp.Header.Get("ETag") != `"`+priv.CID+`"` {
		t.Errorf("get private = %d %s ETag %q", resp.StatusCode, body, resp.Header.Get("ETag"))
	}
	// The public GET of the public one sends the same tag the admin GET does.
	pubResp, _ := adminDo(t, ts, "GET", "/api/bookmarks/"+pub.ID, "", nil, "")
	adminResp, _ := adminDo(t, ts, "GET", "/api/admin/bookmarks/"+pub.ID, "k1", nil, "")
	if pubResp.Header.Get("ETag") != adminResp.Header.Get("ETag") {
		t.Errorf("public ETag %q != admin ETag %q", pubResp.Header.Get("ETag"), adminResp.Header.Get("ETag"))
	}
	if resp, _ := adminDo(t, ts, "GET", "/api/admin/bookmarks/nope", "k1", nil, ""); resp.StatusCode != 404 {
		t.Errorf("missing = %d, want 404", resp.StatusCode)
	}
}

func TestAdminBookmarkRefresh(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head><meta property="og:title" content="Fresh OG"><meta property="og:site_name" content="Site"></head></html>`)
	}))
	defer upstream.Close()
	// "localhost", not the literal 127.0.0.1 httptest hands out: a literal
	// private address is refused before any request is made.
	base := strings.Replace(upstream.URL, "127.0.0.1", "localhost", 1)

	s := newTestServer(t)
	ok := &Bookmark{URL: base + "/page", Title: "Kept title", OGTitle: "Stale OG", AdminNotes: "n"}
	bad := &Bookmark{URL: base + "/broken", OGTitle: "Stale OG"}
	for _, b := range []*Bookmark{ok, bad} {
		if err := insertBookmark(s.db, b); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, body := adminDo(t, ts, "POST", "/api/admin/bookmarks/"+ok.ID+"/refresh", "k1", nil, "")
	if resp.StatusCode != 200 {
		t.Fatalf("refresh = %d %s", resp.StatusCode, body)
	}
	var got Bookmark
	_ = json.Unmarshal(body, &got)
	if got.OGTitle != "Fresh OG" || got.OGSiteName != "Site" || got.Title != "Kept title" || got.AdminNotes != "n" {
		t.Errorf("refreshed = %+v", got)
	}
	if resp.Header.Get("ETag") != `"`+got.CID+`"` || got.CID == ok.CID {
		t.Errorf("ETag %q, cid %q (was %q)", resp.Header.Get("ETag"), got.CID, ok.CID)
	}

	resp, body = adminDo(t, ts, "POST", "/api/admin/bookmarks/"+bad.ID+"/refresh", "k1", nil, "")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), `"error"`) {
		t.Errorf("failed fetch = %d %s, want 502 with an error", resp.StatusCode, body)
	}
	if stored, _ := getBookmark(s.db, bad.ID); stored.OGTitle != "Stale OG" {
		t.Errorf("a failed refresh changed the record: %+v", stored)
	}
	if resp, _ := adminDo(t, ts, "POST", "/api/admin/bookmarks/nope/refresh", "k1", nil, ""); resp.StatusCode != 404 {
		t.Errorf("missing = %d, want 404", resp.StatusCode)
	}
}

func TestBookmarksIfMatch(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		ifMatch func(etag string) string
		want    int
	}{
		{"put absent", "PUT", func(string) string { return "" }, 200},
		{"put current", "PUT", func(e string) string { return e }, 200},
		{"put stale", "PUT", func(string) string { return `"bafystale"` }, 412},
		{"delete current", "DELETE", func(e string) string { return e }, 200},
		{"delete stale", "DELETE", func(string) string { return `"bafystale"` }, 412},
		{"delete absent", "DELETE", func(string) string { return "" }, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			_, priv := seedBookmarks(t, s)
			ts := httptest.NewServer(s.routes())
			defer ts.Close()
			etag := `"` + priv.CID + `"`
			h := map[string]string{}
			if v := tc.ifMatch(etag); v != "" {
				h["If-Match"] = v
			}
			resp, body := adminDo(t, ts, tc.method, "/api/bookmarks/"+priv.ID, "k1", h, `{"title":"Changed"}`)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
			switch {
			case tc.want == 412:
				var pf struct {
					Error   string
					Current Bookmark
				}
				_ = json.Unmarshal(body, &pf)
				if pf.Current.Title != "B" || pf.Current.AdminNotes != "secret note" || resp.Header.Get("ETag") != etag {
					t.Errorf("412 = %s ETag %q", body, resp.Header.Get("ETag"))
				}
				if stored, _ := getBookmark(s.db, priv.ID); stored == nil || stored.Title != "B" {
					t.Error("a refused write changed the bookmark")
				}
			case tc.method == "PUT":
				var got Bookmark
				_ = json.Unmarshal(body, &got)
				if resp.Header.Get("ETag") != `"`+got.CID+`"` || got.CID == priv.CID {
					t.Errorf("PUT ETag %q for cid %q", resp.Header.Get("ETag"), got.CID)
				}
				admin, _ := adminDo(t, ts, "GET", "/api/admin/bookmarks/"+priv.ID, "k1", nil, "")
				if admin.Header.Get("ETag") != resp.Header.Get("ETag") {
					t.Errorf("PUT ETag %q != GET ETag %q", resp.Header.Get("ETag"), admin.Header.Get("ETag"))
				}
			}
		})
	}
}

func TestBookmarksIfMatchConcurrent(t *testing.T) {
	s := newTestServer(t)
	_, priv := seedBookmarks(t, s)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	// Several rounds: one round can serialize by luck and pass without the
	// guard on the UPDATE; ten in a row do not.
	for round := 0; round < 10; round++ {
		cur, _ := getBookmark(s.db, priv.ID)
		etag := `"` + cur.CID + `"`
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, _ := adminDo(t, ts, "PUT", "/api/bookmarks/"+priv.ID, "k1",
					map[string]string{"If-Match": etag},
					`{"title":"round `+string(rune('a'+round))+` writer `+string(rune('0'+i))+`"}`)
				codes[i] = resp.StatusCode
			}()
		}
		wg.Wait()
		slices.Sort(codes)
		if codes[0] != 200 || codes[1] != 412 {
			t.Fatalf("round %d: statuses = %v, want exactly one 200 and one 412", round, codes)
		}
	}
}
