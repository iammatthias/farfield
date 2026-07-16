// Package markdown renders farfield markdown bodies to HTML: GitHub-flavored
// markdown plus the farfield embed vocabulary — blob://<cid> media resolved
// against the blobs service, and series://<slug> fragments resolved by the
// host app. It is the one rendering pipeline the admin UIs share, so a
// preview shows what a consumer of the read API would render.
package markdown

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

var defaultClient = &http.Client{Timeout: 10 * time.Second}

var (
	blobLineRe     = regexp.MustCompile(`^blob://([a-z0-9]+)$`)
	blobMDLineRe   = regexp.MustCompile(`^!\[([^\]]*)\]\(blob://([a-z0-9]+)\)$`)
	blobMDRe       = regexp.MustCompile(`!\[([^\]]*)\]\(blob://([a-z0-9]+)\)`)
	blobLinkRe     = regexp.MustCompile(`\[([^\]]*)\]\(blob://([a-z0-9]+)\)`)
	blobRefRe      = regexp.MustCompile(`blob://([a-z0-9]+)`)
	seriesLineRe   = regexp.MustCompile(`^series://([a-z0-9-]+)$`)
	seriesMDLineRe = regexp.MustCompile(`^!\[[^\]]*\]\(series://([a-z0-9-]+)\)$`)
)

// goldmark converters — GFM (autolinked URLs, strikethrough, tables). Raw HTML
// in the source is omitted (goldmark's safe default), so a body can never
// inject markup. Hard wraps keep a single newline a line break, the way a
// short note expects; long-form bodies use standard paragraph semantics.
var (
	mdSoft = goldmark.New(goldmark.WithExtensions(extension.GFM))
	mdHard = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(ghtml.WithHardWraps()),
	)
)

type blobLookup struct {
	meta *blobMeta
	err  error
}

type blobMeta struct {
	Mime string `json:"mime"`
}

// Renderer turns markdown bodies into HTML. It is safe for concurrent use and
// meant to live for the server's lifetime: successful blob metadata lookups
// are memoized forever (CIDs are content-addressed and immutable), failures
// only for a single Render call, so a blip in the blobs service is retried on
// the next request.
type Renderer struct {
	MetaBase   string // internal blobs base URL — metadata lookups
	PublicBase string // browser-facing blobs base URL — src attributes; MetaBase when empty
	HardWraps  bool   // single newline renders as <br> (short notes)

	// Series resolves a standalone series://<slug> embed to the HTML spliced
	// in its place. nil leaves series refs as plain text.
	Series func(ctx context.Context, slug string) (html string, ok bool)

	Client *http.Client // nil uses a shared 10s-timeout client

	cache sync.Map // cid → blobLookup, successes only, server-lifetime
}

// embedKind classifies a farfield embed token.
type embedKind int

const (
	embedImg    embedKind = iota // ![](blob://cid) or a bare blob:// ref
	embedLink                    // [text](blob://cid) — a file link
	embedSeries                  // ![](series://slug) or a bare series:// ref
)

type embedRef struct {
	kind       embedKind
	cid        string // blob CID (img, link)
	series     string // series slug
	text       string // link text (link)
	alt        string // image alt text (img)
	original   string // the source token, for restoring verbatim regions
	standalone bool
}

func placeholderTok(i int) string { return fmt.Sprintf("ffblobembed%dq", i) }

// prepass swaps farfield embed tokens for markdown-inert placeholders so
// goldmark never mangles them. It returns the rewritten source and the embeds
// in placeholder order; embed originals allow restoring source text inside
// regions that must stay verbatim (code, unsupported blocks).
func (r *Renderer) prepass(body string) (string, []embedRef) {
	var embeds []embedRef
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if cid, alt := standaloneBlobCID(trimmed); cid != "" {
			embeds = append(embeds, embedRef{kind: embedImg, cid: cid, alt: alt, original: line, standalone: true})
			lines[i] = placeholderTok(len(embeds) - 1)
			continue
		}
		if slug := standaloneSeriesSlug(trimmed); slug != "" && r.Series != nil {
			embeds = append(embeds, embedRef{kind: embedSeries, series: slug, original: line, standalone: true})
			lines[i] = placeholderTok(len(embeds) - 1)
			continue
		}
		var b strings.Builder
		for line != "" {
			start, end, ref, ok := nextBlobToken(line)
			if !ok {
				b.WriteString(line)
				break
			}
			ref.original = line[start:end]
			b.WriteString(line[:start])
			b.WriteString(placeholderTok(len(embeds)))
			embeds = append(embeds, ref)
			line = line[end:]
		}
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n"), embeds
}

// restoreEmbeds swaps placeholders back to their original source tokens —
// used for verbatim regions where the embed must survive as text.
func restoreEmbeds(s string, embeds []embedRef) string {
	if !strings.Contains(s, "ffblobembed") {
		return s
	}
	pairs := make([]string, 0, len(embeds)*2)
	for i, e := range embeds {
		pairs = append(pairs, placeholderTok(i), e.original)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// Render converts a markdown body to HTML: farfield embeds are swapped for
// markdown-inert placeholders, the rest goes through goldmark, and the embeds
// — which need mime-aware <img>/<video>/<audio> tags or spliced fragments
// markdown cannot express — are substituted back into the output.
func (r *Renderer) Render(ctx context.Context, body string) template.HTML {
	src, embeds := r.prepass(body)
	md := mdSoft
	if r.HardWraps {
		md = mdHard
	}
	var buf bytes.Buffer
	var html string
	if err := md.Convert([]byte(src), &buf); err != nil {
		html = "<p>" + template.HTMLEscapeString(src) + "</p>"
	} else {
		html = buf.String()
	}

	// failed memoizes this call's unsuccessful lookups so a body with many
	// references to one dead blob does a single lookup per render.
	failed := make(map[string]blobLookup)
	for i, e := range embeds {
		p := placeholderTok(i)
		var out string
		switch e.kind {
		case embedSeries:
			out = r.renderSeries(ctx, e.series)
		case embedLink:
			out = r.renderBlobLink(e, "")
		default:
			out = r.renderBlob(ctx, e.cid, e.alt, e.standalone, failed, "")
		}
		if e.standalone {
			// A block embed gets unwrapped from the paragraph markdown put it in.
			html = strings.ReplaceAll(html, "<p>"+p+"</p>", out)
		}
		html = strings.ReplaceAll(html, p, out)
	}
	return template.HTML(html)
}

// renderBlobLink renders a [text](blob://cid) file link; attrs (with a
// leading space) are spliced into the tag for editor-flavored output.
func (r *Renderer) renderBlobLink(e embedRef, attrs string) string {
	text := e.text
	if strings.TrimSpace(text) == "" {
		text = "blob://" + e.cid
	}
	return `<a class="blob-file"` + attrs + ` href="` + template.HTMLEscapeString(r.blobURL(e.cid)) + `">` +
		template.HTMLEscapeString(text) + `</a>`
}

func standaloneBlobCID(s string) (cid, alt string) {
	if m := blobLineRe.FindStringSubmatch(s); len(m) == 2 {
		return m[1], ""
	}
	if m := blobMDLineRe.FindStringSubmatch(s); len(m) == 3 {
		return m[2], m[1]
	}
	return "", ""
}

func standaloneSeriesSlug(s string) string {
	if m := seriesLineRe.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	if m := seriesMDLineRe.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	return ""
}

// nextBlobToken finds the earliest blob reference in s: image syntax, link
// syntax ([text](blob://cid) — a file link), or a bare ref. Earliest start
// wins, so image syntax beats the link match beginning one rune later, and
// both consume the bare ref inside their destination.
func nextBlobToken(s string) (start, end int, ref embedRef, ok bool) {
	best := -1
	if idx := blobMDRe.FindStringSubmatchIndex(s); idx != nil {
		best, end = idx[0], idx[1]
		ref = embedRef{kind: embedImg, alt: s[idx[2]:idx[3]], cid: s[idx[4]:idx[5]]}
	}
	if idx := blobLinkRe.FindStringSubmatchIndex(s); idx != nil {
		if best == -1 || idx[0] < best {
			best, end = idx[0], idx[1]
			ref = embedRef{kind: embedLink, text: s[idx[2]:idx[3]], cid: s[idx[4]:idx[5]]}
		}
	}
	if idx := blobRefRe.FindStringSubmatchIndex(s); idx != nil {
		if best == -1 || idx[0] < best {
			best, end = idx[0], idx[1]
			ref = embedRef{kind: embedImg, cid: s[idx[2]:idx[3]]}
		}
	}
	if best == -1 {
		return 0, 0, embedRef{}, false
	}
	return best, end, ref, true
}

func (r *Renderer) renderSeries(ctx context.Context, slug string) string {
	html, ok := r.Series(ctx, slug)
	if !ok {
		return `<p class="series-missing">` + template.HTMLEscapeString("series://"+slug) + `</p>`
	}
	return `<figure class="series-embed">` + html + `</figure>`
}

// renderBlob renders a blob embed as its mime-appropriate media tag; attrs
// (with a leading space) are spliced into the tag for editor-flavored output.
func (r *Renderer) renderBlob(ctx context.Context, cid, alt string, standalone bool, failed map[string]blobLookup, attrs string) string {
	meta, err := r.meta(ctx, cid, failed)
	href := r.blobURL(cid)
	if err != nil || meta == nil || meta.Mime == "" {
		return `<a class="blob-file"` + attrs + ` href="` + template.HTMLEscapeString(href) + `">` +
			template.HTMLEscapeString("blob://"+cid) + `</a>`
	}
	cls := "blob-media inline"
	if standalone {
		cls = "blob-media standalone"
	}
	src := template.HTMLEscapeString(href)
	switch {
	case strings.HasPrefix(meta.Mime, "image/"):
		return `<img class="` + cls + `"` + attrs + ` src="` + src + `" alt="` + template.HTMLEscapeString(alt) + `" loading="lazy" decoding="async">`
	case strings.HasPrefix(meta.Mime, "video/"):
		return `<video class="` + cls + `"` + attrs + ` controls preload="metadata" src="` + src + `"></video>`
	case strings.HasPrefix(meta.Mime, "audio/"):
		return `<audio class="` + cls + `"` + attrs + ` controls preload="metadata" src="` + src + `"></audio>`
	default:
		return `<a class="blob-file"` + attrs + ` href="` + src + `">` + template.HTMLEscapeString("blob://"+cid) + `</a>`
	}
}

func (r *Renderer) meta(ctx context.Context, cid string, failed map[string]blobLookup) (*blobMeta, error) {
	if cached, ok := r.cache.Load(cid); ok {
		hit := cached.(blobLookup)
		return hit.meta, hit.err
	}
	if cached, ok := failed[cid]; ok {
		return cached.meta, cached.err
	}
	meta, err := r.fetchMeta(ctx, cid)
	if err != nil {
		failed[cid] = blobLookup{meta: meta, err: err}
	} else {
		r.cache.Store(cid, blobLookup{meta: meta})
	}
	return meta, err
}

func (r *Renderer) blobURL(cid string) string {
	base := r.PublicBase
	if base == "" {
		base = r.MetaBase
	}
	return joinURL(base, "blobs", cid)
}

func (r *Renderer) fetchMeta(ctx context.Context, cid string) (*blobMeta, error) {
	client := r.Client
	if client == nil {
		client = defaultClient
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(r.MetaBase, "blobs", cid, "meta"), nil)
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
