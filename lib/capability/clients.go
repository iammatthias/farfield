// Package capability is the fleet's action layer: one description of everything
// farfield can be asked to do, and one implementation of each.
//
// Before this package the same knowledge lived in two shapes and one of them was
// a lie. switchboard held typed clients for its sibling services and a hardcoded
// `switch` naming the commands, beside a hand-written help string that nothing
// checked against either. Anything else wanting to post to feed — a CLI, an
// agent, a cron job — had to rebuild the clients or fall back to curl.
//
// So the commands are declared once, as data, and every surface derives from that
// declaration: the `farfield` CLI, the markdown slash commands agents load out of
// ~/.claude/commands, switchboard's deterministic dispatch, and the help text all
// read the same table. A command that is not in the table does not exist on any of
// them, and help cannot drift from behaviour because it is generated from it.
//
// This file holds the service clients. Keys live with the caller and never with
// the phone — that is the whole reason switchboard exists rather than letting a
// device hold credentials.
package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// UserAgent identifies farfield's own callers to the Cloudflare edge, which is
// choosy about some default client strings and answers 403 to several of them.
const UserAgent = "farfield-capability/1.0"

// NamedFile is one inbound photo on its way to the app that will store it.
type NamedFile struct {
	Name string
	Mime string
	Data []byte
}

// svc is the shared shape of a farfield service: a base URL and the API key for
// it.
type svc struct {
	URL string
	Key string
	hc  *http.Client
}

func newSvc(url, key string) svc {
	return svc{
		URL: strings.TrimRight(url, "/"),
		Key: key,
		// Long enough to carry photo bytes to feed, short enough that a wedged
		// sibling cannot hold an inbound webhook open indefinitely.
		hc: &http.Client{Timeout: 90 * time.Second},
	}
}

// do issues a request with the service key attached and returns the body.
func (s svc) do(ctx context.Context, method, path, contentType string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.URL+path, body)
	if err != nil {
		return nil, err
	}
	if s.Key != "" {
		req.Header.Set("X-API-Key", s.Key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", resp.Status, FirstLine(out))
	}
	return out, nil
}

func (s svc) postJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return s.do(ctx, http.MethodPost, path, "application/json", bytes.NewReader(buf))
}

// FirstLine keeps an error reply to one readable line. Errors from here end up in
// a text message as often as in a terminal, so a wall of JSON is never the right
// answer.
func FirstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// ── feed ───────────────────────────────────────────────────────────────────

// FeedClient publishes short posts.
type FeedClient struct{ svc }

type feedPost struct {
	Slug string   `json:"slug"`
	Body string   `json:"body"`
	Tags []string `json:"tags"`
}

// CreatePost publishes a post. With photos it posts multipart to feed's media
// endpoint, which uploads the bytes to blobs and embeds the resulting CIDs —
// nothing here ever touches blobs directly, because feed owns the post that
// references them.
func (c *FeedClient) CreatePost(ctx context.Context, body string, tags []string, files []NamedFile) (string, error) {
	if len(files) == 0 {
		out, err := c.postJSON(ctx, "/api/posts", map[string]any{
			"body": body, "tags": orEmpty(tags),
		})
		if err != nil {
			return "", err
		}
		return decodeSlug(out)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("body", body); err != nil {
		return "", err
	}
	if err := mw.WriteField("tags", strings.Join(tags, ",")); err != nil {
		return "", err
	}
	for _, f := range files {
		// CreateFormFile hardcodes application/octet-stream; the part header is
		// built by hand so feed (and blobs behind it) see the real type and an
		// iPhone HEIC is stored as HEIC rather than as opaque bytes.
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="file"; filename=%q`, f.Name))
		if f.Mime != "" {
			h.Set("Content-Type", f.Mime)
		}
		part, err := mw.CreatePart(h)
		if err != nil {
			return "", err
		}
		if _, err := part.Write(f.Data); err != nil {
			return "", err
		}
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	out, err := c.do(ctx, http.MethodPost, "/api/posts/media", mw.FormDataContentType(), &buf)
	if err != nil {
		return "", err
	}
	return decodeSlug(out)
}

func decodeSlug(b []byte) (string, error) {
	var p feedPost
	if err := json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	if p.Slug == "" {
		return "", fmt.Errorf("feed returned no slug")
	}
	return p.Slug, nil
}

func (c *FeedClient) getPost(ctx context.Context, slug string) (*feedPost, error) {
	out, err := c.do(ctx, http.MethodGet, "/api/posts/"+slug, "", nil)
	if err != nil {
		return nil, err
	}
	var p feedPost
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// AppendToPost adds a paragraph to an existing post. feed's update replaces the
// whole record, so the current body is read first and the new text appended —
// the alternative would silently discard everything already there.
func (c *FeedClient) AppendToPost(ctx context.Context, slug, body string, tags []string) (string, error) {
	cur, err := c.getPost(ctx, slug)
	if err != nil {
		return "", err
	}
	merged := strings.TrimSpace(cur.Body)
	if body != "" {
		merged = strings.TrimSpace(merged + "\n\n" + body)
	}
	_, err = c.putPost(ctx, slug, merged, Dedupe(append(append([]string{}, cur.Tags...), tags...)))
	return slug, err
}

// RetagPost replaces a post's tags, leaving the body untouched.
func (c *FeedClient) RetagPost(ctx context.Context, slug string, tags []string) error {
	cur, err := c.getPost(ctx, slug)
	if err != nil {
		return err
	}
	_, err = c.putPost(ctx, slug, cur.Body, tags)
	return err
}

func (c *FeedClient) putPost(ctx context.Context, slug, body string, tags []string) ([]byte, error) {
	buf, err := json.Marshal(map[string]any{"body": body, "tags": orEmpty(tags)})
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPut, "/api/posts/"+slug, "application/json", bytes.NewReader(buf))
}

func (c *FeedClient) DeletePost(ctx context.Context, slug string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/posts/"+slug, "", nil)
	return err
}

// ── bookmarks ──────────────────────────────────────────────────────────────

// BookmarksClient files links.
type BookmarksClient struct {
	svc
	DefaultCategory string
}

func (c *BookmarksClient) Create(ctx context.Context, link, category string) (string, error) {
	if category == "" {
		category = c.DefaultCategory
	}
	out, err := c.postJSON(ctx, "/api/bookmarks", map[string]any{
		"url": link, "category": category, "public": true,
	})
	if err != nil {
		return "", err
	}
	var b struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &b); err != nil {
		return "", err
	}
	if b.ID == "" {
		return "", fmt.Errorf("bookmarks returned no id")
	}
	return b.ID, nil
}

func (c *BookmarksClient) Delete(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/bookmarks/"+id, "", nil)
	return err
}

// ── scrap ──────────────────────────────────────────────────────────────────

// ScrapClient stores pastes.
type ScrapClient struct{ svc }

// Create posts a paste. scrap takes the raw body and its options as query
// parameters, and answers with the URL as plain text rather than JSON.
//
// Unlisted by default: a paste made this way is a link to hand to someone, not a
// thing to list publicly.
func (c *ScrapClient) Create(ctx context.Context, text string) (string, string, error) {
	out, err := c.do(ctx, http.MethodPost, "/api/pastes?visibility=unlisted",
		"text/plain; charset=utf-8", strings.NewReader(text))
	if err != nil {
		return "", "", err
	}
	pasteURL := FirstLine(out)
	if pasteURL == "" {
		return "", "", fmt.Errorf("scrap returned no url")
	}
	id := pasteURL[strings.LastIndexByte(pasteURL, '/')+1:]
	return pasteURL, id, nil
}

func (c *ScrapClient) Delete(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/pastes/"+id, "", nil)
	return err
}

// ── qr ─────────────────────────────────────────────────────────────────────

// QRClient mints QR codes.
type QRClient struct {
	svc
	PublicURL string
}

func (c *QRClient) Create(ctx context.Context, target, label string) (string, error) {
	if label == "" {
		label = "texted"
	}
	out, err := c.postJSON(ctx, "/api/codes", map[string]any{
		"target": target, "label": label, "mode": "direct", "ec": "M",
		"public": true, "enabled": true,
	})
	if err != nil {
		return "", err
	}
	var code struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &code); err != nil {
		return "", err
	}
	if code.ID == "" {
		return "", fmt.Errorf("qr returned no id")
	}
	return code.ID, nil
}

// PNG fetches the raster rendering, which is what can be sent inline in a
// message — an SVG would arrive as a file attachment instead of a picture.
func (c *QRClient) PNG(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/qr/"+id+".png", "", nil)
}

func (c *QRClient) Delete(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/codes/"+id, "", nil)
	return err
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
