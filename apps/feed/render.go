package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	ghtml "github.com/yuin/goldmark/renderer/html"
)

var blobMetaClient = &http.Client{Timeout: 10 * time.Second}

var (
	blobLineRe   = regexp.MustCompile(`^blob://([a-z0-9]+)$`)
	blobMDLineRe = regexp.MustCompile(`^!\[[^\]]*\]\(blob://([a-z0-9]+)\)$`)
	blobMDRe     = regexp.MustCompile(`!\[[^\]]*\]\(blob://([a-z0-9]+)\)`)
	blobRefRe    = regexp.MustCompile(`blob://([a-z0-9]+)`)
)

// postView is the feed template shape: the original post fields plus rendered
// HTML for the body.
type postView struct {
	Post
	BodyHTML template.HTML
}

type blobLookup struct {
	meta *blobMeta
	err  error
}

type blobMeta struct {
	Mime string `json:"mime"`
}

// bodyRenderer resolves blob:// refs against the blobs service. Successful
// metadata lookups land in the shared server-lifetime cache — blob CIDs are
// content-addressed and immutable, so a hit never goes stale. Failures are
// memoized only for the lifetime of this renderer (one page render), so a
// blip in the blobs service is retried on the next request.
type bodyRenderer struct {
	ctx        context.Context
	client     *http.Client
	metaBase   string
	publicBase string
	shared     *sync.Map // cid → blobLookup, successes only, server-lifetime
	cache      map[string]blobLookup
}

func newBodyRenderer(ctx context.Context, metaBase, publicBase string, shared *sync.Map) *bodyRenderer {
	metaBase = strings.TrimRight(metaBase, "/")
	publicBase = strings.TrimRight(publicBase, "/")
	if publicBase == "" {
		publicBase = metaBase
	}
	return &bodyRenderer{
		ctx:        ctx,
		client:     blobMetaClient,
		metaBase:   metaBase,
		publicBase: publicBase,
		shared:     shared,
		cache:      make(map[string]blobLookup),
	}
}

// md renders post bodies as markdown — GFM (autolinked URLs, strikethrough,
// tables) with hard wraps, so a single newline stays a line break the way a
// short note expects. Raw HTML in the source is omitted (goldmark's safe
// default), so a body can never inject markup.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(ghtml.WithHardWraps()),
)

// render turns a post body into HTML: blob:// embeds are swapped for
// markdown-inert placeholders, the rest of the body goes through the markdown
// renderer, and the embeds — which need mime-aware <img>/<video>/<audio> tags
// markdown cannot express — are substituted back into the output.
func (r *bodyRenderer) render(body string) template.HTML {
	type embed struct {
		cid        string
		standalone bool
	}
	var embeds []embed
	placeholder := func(i int) string { return fmt.Sprintf("ffblobembed%dq", i) }

	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if cid := standaloneBlobCID(strings.TrimSpace(line)); cid != "" {
			lines[i] = placeholder(len(embeds))
			embeds = append(embeds, embed{cid: cid, standalone: true})
			continue
		}
		var b strings.Builder
		for line != "" {
			start, end, cid, ok := nextBlobToken(line)
			if !ok {
				b.WriteString(line)
				break
			}
			b.WriteString(line[:start])
			b.WriteString(placeholder(len(embeds)))
			embeds = append(embeds, embed{cid: cid})
			line = line[end:]
		}
		lines[i] = b.String()
	}

	src := strings.Join(lines, "\n")
	var buf bytes.Buffer
	var html string
	if err := md.Convert([]byte(src), &buf); err != nil {
		html = "<p>" + template.HTMLEscapeString(src) + "</p>"
	} else {
		html = buf.String()
	}

	for i, e := range embeds {
		p := placeholder(i)
		if e.standalone {
			// A block embed gets unwrapped from the paragraph markdown put it in.
			html = strings.ReplaceAll(html, "<p>"+p+"</p>", r.renderEmbed(e.cid, true))
		}
		html = strings.ReplaceAll(html, p, r.renderEmbed(e.cid, e.standalone))
	}
	return template.HTML(html)
}

func standaloneBlobCID(s string) string {
	if m := blobLineRe.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	if m := blobMDLineRe.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	return ""
}

func nextBlobToken(s string) (start, end int, cid string, ok bool) {
	bestStart, bestEnd := -1, -1
	bestCID := ""
	if idx := blobMDRe.FindStringSubmatchIndex(s); idx != nil {
		bestStart, bestEnd = idx[0], idx[1]
		bestCID = s[idx[2]:idx[3]]
	}
	if idx := blobRefRe.FindStringSubmatchIndex(s); idx != nil {
		if bestStart == -1 || idx[0] < bestStart {
			bestStart, bestEnd = idx[0], idx[1]
			bestCID = s[idx[2]:idx[3]]
		}
	}
	if bestStart == -1 {
		return 0, 0, "", false
	}
	return bestStart, bestEnd, bestCID, true
}

func (r *bodyRenderer) renderEmbed(cid string, standalone bool) string {
	meta, err := r.meta(cid)
	href := r.blobURL(cid)
	if err != nil || meta == nil || meta.Mime == "" {
		return `<a class="blob-file" href="` + template.HTMLEscapeString(href) + `">` + template.HTMLEscapeString("blob://"+cid) + `</a>`
	}
	cls := "blob-media inline"
	if standalone {
		cls = "blob-media standalone"
	}
	src := template.HTMLEscapeString(href)
	switch {
	case strings.HasPrefix(meta.Mime, "image/"):
		return `<img class="` + cls + `" src="` + src + `" alt="" loading="lazy" decoding="async">`
	case strings.HasPrefix(meta.Mime, "video/"):
		return `<video class="` + cls + `" controls preload="metadata" src="` + src + `"></video>`
	case strings.HasPrefix(meta.Mime, "audio/"):
		return `<audio class="` + cls + `" controls preload="metadata" src="` + src + `"></audio>`
	default:
		return `<a class="blob-file" href="` + src + `">` + template.HTMLEscapeString("blob://"+cid) + `</a>`
	}
}

func (r *bodyRenderer) meta(cid string) (*blobMeta, error) {
	if r.shared != nil {
		if cached, ok := r.shared.Load(cid); ok {
			hit := cached.(blobLookup)
			return hit.meta, hit.err
		}
	}
	if cached, ok := r.cache[cid]; ok {
		return cached.meta, cached.err
	}
	meta, err := fetchBlobMeta(r.ctx, r.client, r.metaBase, cid)
	r.cache[cid] = blobLookup{meta: meta, err: err}
	if err == nil && r.shared != nil {
		r.shared.Store(cid, blobLookup{meta: meta})
	}
	return meta, err
}

func (r *bodyRenderer) blobURL(cid string) string {
	return joinURL(r.publicBase, "blobs", cid)
}

func fetchBlobMeta(ctx context.Context, client *http.Client, base, cid string) (*blobMeta, error) {
	if client == nil {
		client = blobMetaClient
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(base, "blobs", cid, "meta"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("blob meta %s: %s", cid, resp.Status)
	}
	var meta blobMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func joinURL(base string, elems ...string) string {
	joined, err := url.JoinPath(base, elems...)
	if err == nil {
		return joined
	}
	parts := append([]string{strings.TrimRight(base, "/")}, elems...)
	return strings.Join(parts, "/")
}
