package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
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

// handleFleetSearchData aggregates the corpus. Source failures degrade to
// partial results with a warning — search over most of the fleet beats a 500.
func (s *Server) handleFleetSearchData(w http.ResponseWriter, r *http.Request) {
	var docs []fleetDoc
	var warnings []string

	entries, err := listEntriesFull(s.db, "", statusAll, 0, 0)
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

	var feed struct {
		Posts []struct {
			Slug, CID, Body string
			Tags            []string
		} `json:"posts"`
	}
	if fleetFetch(s.feedURL+"/api/posts", s.feedReadKey, &feed) {
		for _, p := range feed.Posts {
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

	var bm struct {
		Bookmarks []struct {
			ID          int64
			CID         string
			URL, Title  string
			Description string
			Tags        []string
		} `json:"bookmarks"`
	}
	if fleetFetch(s.bookmarksURL+"/api/bookmarks", s.bookmarksReadKey, &bm) {
		for _, b := range bm.Bookmarks {
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
	return json.NewDecoder(resp.Body).Decode(out) == nil
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
