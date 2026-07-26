package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// Fleet search: one box over everything the fleet's read APIs expose —
// entries and series from this app's own tables, posts from feed, and
// public bookmarks from bookmarks (its list API is public-only by design;
// private bookmarks stay out of the corpus). Content hosts the page because
// it already ships the on-device embedding engine; ranking happens in the
// browser, this side only aggregates.
//
// Envs (defaults fit the local fleet): FEED_URL, FEED_READ_KEY,
// BOOKMARKS_URL, BOOKMARKS_READ_KEY.

var fleetClient = &http.Client{Timeout: 10 * time.Second}

// fleetDoc is one searchable thing, shaped for the browser: a stable cache
// key (the CID where the source has one — content-addressed, so cached
// embeddings can never go stale), display fields, and the admin URL to open.
type fleetDoc struct {
	Kind    string   `json:"kind"` // entry | series | post | bookmark
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	Snippet string   `json:"snippet"`
	Tags    []string `json:"tags,omitempty"`
	Meta    string   `json:"meta,omitempty"` // collection, date — display only
	URL     string   `json:"url"`
}

func (s *Server) handleFleetSearchPage(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "search.html", nil)
}

// fleetCorpusLimit bounds how many documents of each kind enter the corpus.
// The whole set is serialized into one response and embedded in the browser,
// so it has to have a ceiling — without one the payload grows with the
// archive forever. Newest-first ordering means the cap drops the oldest.
const fleetCorpusLimit = 2000

// handleFleetSearchData aggregates the corpus. Source failures degrade to
// partial results with a warning — search over most of the fleet beats a 500.
func (s *Server) handleFleetSearchData(w http.ResponseWriter, r *http.Request) {
	var docs []fleetDoc
	var warnings []string

	// The two sibling services are independent — fetch them while this app
	// reads its own tables, so the page waits on the slowest source rather
	// than on the sum of all three.
	type feedShape struct {
		Posts []struct {
			Slug, CID, Body string
			Tags            []string
		} `json:"posts"`
	}
	type bookmarkShape struct {
		Bookmarks []struct {
			ID          int64
			CID         string
			URL, Title  string
			Description string
			Tags        []string
		} `json:"bookmarks"`
	}
	var (
		feed     feedShape
		bm       bookmarkShape
		feedOK   bool
		bmOK     bool
		siblings sync.WaitGroup
	)
	siblings.Add(2)
	go func() {
		defer siblings.Done()
		feedOK = fleetFetch(s.feedURL+"/api/posts", s.feedReadKey, &feed)
	}()
	go func() {
		defer siblings.Done()
		bmOK = fleetFetch(s.bookmarksURL+"/api/bookmarks", s.bookmarksReadKey, &bm)
	}()

	entries, err := listEntriesFull(s.db, "", statusAll, fleetCorpusLimit, 0)
	if err == nil {
		for _, e := range entries {
			docs = append(docs, fleetDoc{
				Kind: "entry", Key: e.CID, Title: e.Title,
				Snippet: searchSnippet(e.Body, 400), Tags: e.Tags,
				Meta: e.Collection,
				URL:  web.FleetBase("content") + "/entries/" + e.Slug + "/edit",
			})
		}
	} else {
		warnings = append(warnings, "entries unavailable")
	}

	series, err := listSeries(s.db)
	if err == nil {
		for _, se := range series {
			title := se.Title
			if title == "" {
				title = se.Slug
			}
			docs = append(docs, fleetDoc{
				Kind: "series", Key: se.CID, Title: title,
				Snippet: searchSnippet(se.Body, 200),
				URL:     web.FleetBase("content") + "/series/" + se.Slug + "/edit",
			})
		}
	} else {
		warnings = append(warnings, "series unavailable")
	}

	siblings.Wait()

	if feedOK {
		for _, p := range capSlice(feed.Posts, fleetCorpusLimit) {
			docs = append(docs, fleetDoc{
				Kind: "post", Key: p.CID,
				Title:   searchSnippet(plainText(p.Body), 80),
				Snippet: searchSnippet(plainText(p.Body), 400), Tags: p.Tags,
				URL: web.FleetBase("feed") + "/posts/" + p.Slug + "/edit",
			})
		}
	} else {
		warnings = append(warnings, "feed unreachable")
	}

	if bmOK {
		for _, b := range capSlice(bm.Bookmarks, fleetCorpusLimit) {
			title := b.Title
			if title == "" {
				title = b.URL
			}
			docs = append(docs, fleetDoc{
				Kind: "bookmark", Key: b.CID, Title: title,
				Snippet: b.Description, Tags: b.Tags, Meta: b.URL,
				URL: web.FleetBase("bookmarks"),
			})
		}
	} else {
		warnings = append(warnings, "bookmarks unreachable")
	}

	web.WriteJSON(w, http.StatusOK, map[string]any{
		"docs": docs, "warnings": warnings,
	})
}

// capSlice returns at most n elements of s — the corpus cap, applied to a
// sibling service's response without assuming it honored any limit itself.
func capSlice[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// maxFleetResponse bounds a sibling's JSON response. These are trusted
// services, but a bounded read keeps one misbehaving upstream from
// exhausting this process's memory.
const maxFleetResponse = 64 << 20 // 64 MiB

// fleetFetch GETs a sibling read API into out, sending the key when set.
// False on any failure — the caller degrades rather than erroring.
func fleetFetch(url, key string, out any) bool {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := fleetClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxFleetResponse)).Decode(out) == nil
}

var (
	plainEmbedRe  = regexp.MustCompile(`!?\[[^\]]*\]\([^)]*\)`)
	plainMarkerRe = regexp.MustCompile("[*_`#>]+")
)

// plainText strips markdown furniture for display titles — posts have no
// title of their own, so their opening text stands in.
func plainText(md string) string {
	s := plainEmbedRe.ReplaceAllString(md, " ")
	s = plainMarkerRe.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}
