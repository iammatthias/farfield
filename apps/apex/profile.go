package main

// The profile endpoint: one public GET that renders the site's freshest public
// material as ready-to-splice markdown, so the GitHub profile README can update
// itself with a curl and no credentials.
//
// The shape of the arrangement is the point. The repo side is a cron and a
// splice — no keys, no rendering, no knowledge of the fleet — because anything
// smarter in a public repo's Actions would need secrets there, and the fleet's
// rule is that keys live next to the services. So apex holds the read keys,
// does the rendering, and publishes only what iammatthias.com already shows:
// the latest feed post, the newest writing, today's art. Bookmarks, QR, and
// anything operational are deliberately absent.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// profileCollections are the content collections the writing section may draw
// from. An allowlist rather than "everything but feed": a future collection is
// invisible here until somebody decides it belongs on a public README.
var profileCollections = map[string]bool{
	"posts": true, "art": true, "recipes": true, "open-source": true,
}

// profileWritingCount is how many entries the writing section lists.
const profileWritingCount = 3

// profileCacheTTL bounds how stale the endpoint may serve. The README cron
// runs on the order of hours, so ten minutes of staleness is invisible there;
// what the cache actually buys is that a burst of anonymous hits costs the
// siblings one fan-out, not one each.
const profileCacheTTL = 10 * time.Minute

// profileClient keeps upstream fetches brisk. A slow sibling degrades to an
// omitted section rather than a hung endpoint.
var profileClient = &http.Client{Timeout: 8 * time.Second}

// profileServer holds the addresses, the keys, and the cache.
type profileServer struct {
	feedURL, feedKey       string
	contentURL, contentKey string
	dailyURL               string
	rl                     *web.RateLimiter

	mu      sync.Mutex
	cached  []byte
	etag    string
	renewed time.Time
}

func newProfileServer() *profileServer {
	return &profileServer{
		feedURL:    strings.TrimRight(store.Env("FEED_URL", "http://feed:8788"), "/"),
		feedKey:    store.Env("FEED_READ_KEY", ""),
		contentURL: strings.TrimRight(store.Env("CONTENT_URL", "http://content:8787"), "/"),
		contentKey: store.Env("CONTENT_READ_KEY", ""),
		dailyURL:   strings.TrimRight(store.Env("DAILY_URL", "http://daily:8792"), "/"),
		rl:         web.NewRateLimiter(60, time.Minute),
	}
}

// handle serves GET /api/profile.
func (p *profileServer) handle(w http.ResponseWriter, r *http.Request) {
	body, etag := p.current(r.Context())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	if web.ETagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

// current returns the rendered document, rebuilding it when the cache has aged
// out. Failures keep the previous document: a sibling being down for a minute
// must not blank a section on someone's profile.
func (p *profileServer) current(ctx context.Context) ([]byte, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != nil && time.Since(p.renewed) < profileCacheTTL {
		return p.cached, p.etag
	}

	doc := p.build(ctx)
	body, err := json.Marshal(doc)
	if err != nil || len(doc.Sections) == 0 {
		// Nothing renderable. Serve the previous document if there is one;
		// otherwise say so honestly rather than 500 into a README.
		if p.cached != nil {
			return p.cached, p.etag
		}
		body = []byte(`{"sections":{}}`)
	}
	p.cached = body
	p.etag = cid.Of(body)[:16]
	p.renewed = time.Now()
	return p.cached, p.etag
}

// profileDoc is the response contract.
//
// Sections hold GitHub-flavored markdown, ready to splice verbatim. A section
// whose upstream failed is ABSENT rather than empty, so a splicer can keep the
// old content for that section instead of blanking it.
type profileDoc struct {
	Sections  map[string]string `json:"sections"`
	UpdatedAt string            `json:"updatedAt"`
}

// build fetches the three sources concurrently and renders each section.
func (p *profileServer) build(ctx context.Context) profileDoc {
	ctx, cancel := context.WithTimeout(ctx, 9*time.Second)
	defer cancel()

	doc := profileDoc{Sections: map[string]string{}, UpdatedAt: store.NowRFC3339()}
	var mu sync.Mutex
	var wg sync.WaitGroup
	add := func(name string, render func(context.Context) (string, error)) {
		defer wg.Done()
		md, err := render(ctx)
		if err != nil {
			// An omitted section is the contract for "upstream unavailable";
			// the log line is for the operator, not the reader.
			slog.Warn("profile section unavailable", "section", name, "err", err)
			return
		}
		mu.Lock()
		doc.Sections[name] = md
		mu.Unlock()
	}
	wg.Add(3)
	go add("feed", p.renderFeed)
	go add("writing", p.renderWriting)
	go add("daily", p.renderDaily)
	wg.Wait()
	return doc
}

// ── feed ───────────────────────────────────────────────────────────────────

// blobEmbedRe matches feed's image embeds: ![alt](blob://<cid>).
var blobEmbedRe = regexp.MustCompile(`!\[[^\]]*\]\(blob://([a-z0-9]+)\)`)

func (p *profileServer) renderFeed(ctx context.Context) (string, error) {
	raw, err := p.get(ctx, p.feedURL+"/api/posts?limit=1", p.feedKey)
	if err != nil {
		return "", err
	}
	var out struct {
		Posts []struct {
			Slug      string `json:"slug"`
			Body      string `json:"body"`
			CreatedAt string `json:"createdAt"`
		} `json:"posts"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if len(out.Posts) == 0 {
		return "", fmt.Errorf("feed returned no posts")
	}
	post := out.Posts[0]

	// The first embedded image rides along; the embed syntax itself comes out
	// of the quoted text, because a README renders blob:// as a broken link.
	firstCID := ""
	if m := blobEmbedRe.FindStringSubmatch(post.Body); m != nil {
		firstCID = m[1]
	}
	text := strings.TrimSpace(blobEmbedRe.ReplaceAllString(post.Body, ""))

	var b strings.Builder
	if text == "" {
		b.WriteString("> *(a wordless transmission)*\n")
	} else {
		for _, line := range strings.Split(text, "\n") {
			b.WriteString("> " + line + "\n")
		}
	}
	if firstCID != "" {
		fmt.Fprintf(&b, "\n<img src=\"https://blobs.farfield.systems/blobs/%s\" width=\"480\" alt=\"\">\n", firstCID)
	}
	fmt.Fprintf(&b, "\n*%s · [permalink](https://iammatthias.com/feed/%s)*",
		shortDate(post.CreatedAt), post.Slug)
	return b.String(), nil
}

// ── writing ────────────────────────────────────────────────────────────────

func (p *profileServer) renderWriting(ctx context.Context) (string, error) {
	// One unfiltered list, filtered here against the allowlist — four filtered
	// requests would be four times the traffic for the same rows.
	raw, err := p.get(ctx, p.contentURL+"/api/entries?bodies=0", p.contentKey)
	if err != nil {
		return "", err
	}
	var out struct {
		Entries []struct {
			Collection string `json:"collection"`
			Slug       string `json:"slug"`
			Title      string `json:"title"`
			Excerpt    string `json:"excerpt"`
			CreatedAt  string `json:"createdAt"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	entries := out.Entries[:0]
	for _, e := range out.Entries {
		if profileCollections[e.Collection] {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no entries in the profile collections")
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].CreatedAt > entries[j].CreatedAt })
	if len(entries) > profileWritingCount {
		entries = entries[:profileWritingCount]
	}

	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- **[%s](https://iammatthias.com/%s/%s)** `%s` · %s",
			escapeMD(e.Title), e.Collection, e.Slug, e.Collection, shortDate(e.CreatedAt))
		if ex := strings.TrimSpace(e.Excerpt); ex != "" {
			b.WriteString(" — " + escapeMD(ex))
		}
	}
	return b.String(), nil
}

// ── daily ──────────────────────────────────────────────────────────────────

func (p *profileServer) renderDaily(ctx context.Context) (string, error) {
	raw, err := p.get(ctx, p.dailyURL+"/api/art", "")
	if err != nil {
		return "", err
	}
	var art struct {
		Date  string `json:"date"`
		Biome string `json:"biome"`
		Zone  struct {
			Name string `json:"name"`
		} `json:"zone"`
	}
	if err := json.Unmarshal(raw, &art); err != nil {
		return "", err
	}
	if art.Date == "" {
		return "", fmt.Errorf("daily returned no art")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<a href=\"https://daily.farfield.systems/art/%s\"><img src=\"https://daily.farfield.systems/art/%s.svg\" width=\"480\" alt=\"daily art for %s\"></a>\n",
		art.Date, art.Date, art.Date)
	fmt.Fprintf(&b, "\n*%s · %s · %s*", art.Biome, art.Zone.Name, art.Date)
	return b.String(), nil
}

// ── plumbing ───────────────────────────────────────────────────────────────

// get fetches one sibling URL, attaching the read key when there is one.
func (p *profileServer) get(ctx context.Context, url, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	req.Header.Set("User-Agent", "farfield-apex/1.0")
	resp, err := profileClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 120)])))
	}
	return body, nil
}

// shortDate reduces an RFC3339 timestamp to its date, which is all a README
// needs.
func shortDate(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// escapeMD neutralises the characters that would break out of an inline
// markdown context. Titles and excerpts are the site's own words, so this is
// belt rather than armour — but a stray ] in a title must not eat the link.
func escapeMD(s string) string {
	r := strings.NewReplacer("[", "\\[", "]", "\\]", "*", "\\*", "_", "\\_", "`", "\\`")
	return r.Replace(s)
}
