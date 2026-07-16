package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Blobs is content-addressed: it stores bytes and hands out CIDs, but it
// cannot know what references them. Entries and series (the content app) and
// posts (the feed app) embed blobs as blob://<cid> in their markdown bodies —
// so the hygiene page fetches every body from those services, extracts the
// references, and reports which stored blobs are in use, which are orphaned,
// and which references dangle.

// hygieneSources says where the sibling services live and which keys unlock
// their read APIs. Populated in run() from the environment:
//
//	CONTENT_URL      content service base URL (default http://127.0.0.1:8787)
//	CONTENT_API_KEY  content write key — hygiene asks for ?status=all so draft
//	                 bodies are scanned too; without it the scan falls back to
//	                 published entries only and the page says so
//	FEED_URL         feed service base URL (default http://127.0.0.1:8788)
//	FEED_READ_KEY    feed read token — optional, because feed's read gate is
//	                 open when feed has no read key configured
type hygieneSources struct {
	ContentURL string
	ContentKey string
	FeedURL    string
	FeedKey    string
}

// hygieneClient fetches sibling-service bodies. The timeout is short: the
// hygiene page renders synchronously, and partial data beats a hung page.
var hygieneClient = &http.Client{Timeout: 10 * time.Second}

// blobRefRe matches blob://<cid> embeds in markdown bodies. CIDs are
// lowercase base32 (see validCID), so [a-z0-9]+ is exact enough here.
var blobRefRe = regexp.MustCompile(`blob://([a-z0-9]+)`)

// blobRef is one place a blob is referenced from: a content entry, a content
// series, or a feed post.
type blobRef struct {
	Kind  string // "entry", "series", or "post"
	Slug  string
	Title string // empty for posts, which have no title
}

// referencedBlob pairs a stored blob with the references keeping it alive.
type referencedBlob struct {
	Meta Meta
	Refs []blobRef
}

// missingRef is a CID that bodies reference but the store does not hold.
type missingRef struct {
	CID  string
	Refs []blobRef
}

// hygieneReport is everything the hygiene page renders.
type hygieneReport struct {
	Referenced []referencedBlob // stored blobs with live references
	Orphans    []Meta           // stored blobs referenced by nothing
	Missing    []missingRef     // referenced CIDs absent from the store

	// DerivedThumbs counts distinct generated-thumbnail CIDs. Thumbnails are
	// derived from a parent blob (its thumb_cid), so they are never orphans —
	// they live and die with the parent — and are excluded from the report.
	DerivedThumbs int

	// DraftsSkipped reports that content rejected ?status=all (no write
	// key), so draft entry bodies were not scanned.
	DraftsSkipped bool

	// SourceErrors holds one message per source that could not be fetched.
	// When any is set the orphan list is untrustworthy — an unreferenced
	// blob may only be unscanned — so the page must not offer deletion.
	SourceErrors []string
}

// SourcesOK reports that every source was scanned, making the orphan list
// trustworthy enough to offer deletion.
func (r *hygieneReport) SourcesOK() bool { return len(r.SourceErrors) == 0 }

// fetchJSON GETs a URL, sending key as X-API-Key when non-empty, and decodes
// a 200 response into out. It returns the response status code (0 when the
// request never got one) alongside any error.
func fetchJSON(ctx context.Context, url, key string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := hygieneClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

// collectRefs pulls every markdown body from the sibling services and maps
// cid → the references embedding it. draftsSkipped reports that content
// rejected the draft scan; errs holds one message per source that could not
// be fetched at all.
func (s *Server) collectRefs(ctx context.Context) (refs map[string][]blobRef, draftsSkipped bool, errs []string) {
	refs = map[string][]blobRef{}
	add := func(kind, slug, title, body string) {
		for _, m := range blobRefRe.FindAllStringSubmatch(body, -1) {
			refs[m[1]] = append(refs[m[1]], blobRef{Kind: kind, Slug: slug, Title: title})
		}
	}

	// Content entries — {"entries":[{slug,title,body,...}]}. status=all needs
	// the write key; when content refuses, fall back to published only.
	content := strings.TrimRight(s.sources.ContentURL, "/")
	var entries struct {
		Entries []struct {
			Slug  string `json:"slug"`
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"entries"`
	}
	code, err := fetchJSON(ctx, content+"/api/entries?status=all", s.sources.ContentKey, &entries)
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		draftsSkipped = true
		_, err = fetchJSON(ctx, content+"/api/entries", s.sources.ContentKey, &entries)
	}
	if err != nil {
		errs = append(errs, "content entries: "+err.Error())
	}
	for _, e := range entries.Entries {
		add("entry", e.Slug, e.Title, e.Body)
	}

	// Content series — {"series":[{slug,title,body,...}]}.
	var series struct {
		Series []struct {
			Slug  string `json:"slug"`
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"series"`
	}
	if _, err := fetchJSON(ctx, content+"/api/series", s.sources.ContentKey, &series); err != nil {
		errs = append(errs, "content series: "+err.Error())
	}
	for _, se := range series.Series {
		add("series", se.Slug, se.Title, se.Body)
	}

	// Feed posts — {"posts":[{slug,body,createdAt,...}]}. The list is
	// cursor-paginated (?limit= capped at 200, ?before=<createdAt>), so walk
	// the cursor until a short page.
	const feedPage = 200
	feed := strings.TrimRight(s.sources.FeedURL, "/")
	for before := ""; ; {
		var posts struct {
			Posts []struct {
				Slug      string `json:"slug"`
				Body      string `json:"body"`
				CreatedAt string `json:"createdAt"`
			} `json:"posts"`
		}
		u := fmt.Sprintf("%s/api/posts?limit=%d", feed, feedPage)
		if before != "" {
			u += "&before=" + url.QueryEscape(before)
		}
		if _, err := fetchJSON(ctx, u, s.sources.FeedKey, &posts); err != nil {
			errs = append(errs, "feed posts: "+err.Error())
			break
		}
		for _, p := range posts.Posts {
			add("post", p.Slug, "", p.Body)
		}
		if len(posts.Posts) < feedPage {
			break
		}
		before = posts.Posts[len(posts.Posts)-1].CreatedAt
	}
	return refs, draftsSkipped, errs
}

// buildHygiene compares the store against the collected references. Database
// errors are hard failures; source-fetch failures degrade to a partial report
// with SourceErrors set.
func (s *Server) buildHygiene(ctx context.Context) (*hygieneReport, error) {
	blobs, err := listAllMeta(s.db)
	if err != nil {
		return nil, err
	}
	refs, draftsSkipped, errs := s.collectRefs(ctx)

	// thumbs holds every generated-thumbnail CID; inStore everything the
	// store can serve (blob rows plus their thumbnails), so a body that
	// happens to reference a thumbnail directly is not reported as missing.
	thumbs := map[string]bool{}
	inStore := map[string]bool{}
	for _, m := range blobs {
		inStore[m.CID] = true
		if m.ThumbCID != "" {
			thumbs[m.ThumbCID] = true
			inStore[m.ThumbCID] = true
		}
	}

	rep := &hygieneReport{
		DerivedThumbs: len(thumbs),
		DraftsSkipped: draftsSkipped,
		SourceErrors:  errs,
	}
	for _, m := range blobs {
		switch {
		case len(refs[m.CID]) > 0:
			rep.Referenced = append(rep.Referenced, referencedBlob{Meta: m, Refs: refs[m.CID]})
		case thumbs[m.CID]:
			// A row whose bytes double as another blob's thumbnail is
			// derived — its parent's references keep it alive.
		default:
			rep.Orphans = append(rep.Orphans, m)
		}
	}
	for cid, rs := range refs {
		if !inStore[cid] {
			rep.Missing = append(rep.Missing, missingRef{CID: cid, Refs: rs})
		}
	}
	// Map iteration order is random — sort so the section is stable.
	slices.SortFunc(rep.Missing, func(a, b missingRef) int {
		return strings.Compare(a.CID, b.CID)
	})
	return rep, nil
}

// handleHygiene renders the blob-hygiene report. Unreachable sources degrade
// to a warning on the page — never a 500 — but they also disable the orphan
// delete buttons, because a fetch failure must not make blobs look orphaned.
func (s *Server) handleHygiene(w http.ResponseWriter, r *http.Request) {
	rep, err := s.buildHygiene(r.Context())
	if err != nil {
		slog.Error("hygiene report", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.rd.Render(w, "hygiene.html", map[string]any{"Report": rep})
}
