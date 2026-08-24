package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"sync"
	"time"
)

// namedFile is one inbound photo on its way to the app that will store it.
type namedFile struct {
	Name string
	Mime string
	Data []byte
}

// svc is the shared shape of a sibling farfield service: a base URL and the
// server-side API key for it. Keys live here and only here — the phone never
// holds one, which is the point of routing through switchboard at all.
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
	req.Header.Set("User-Agent", userAgent)
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
		return nil, fmt.Errorf("%s: %s", resp.Status, firstLine(out))
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

// firstLine keeps an error reply to one readable line for a text message.
func firstLine(b []byte) string {
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

type feedClient struct{ svc }

type feedPost struct {
	Slug string   `json:"slug"`
	Body string   `json:"body"`
	Tags []string `json:"tags"`
}

// CreatePost publishes a post. With photos it posts multipart to feed's media
// endpoint, which uploads the bytes to blobs and embeds the resulting CIDs —
// switchboard deliberately never touches blobs itself.
func (c *feedClient) CreatePost(ctx context.Context, body string, tags []string, files []namedFile) (string, error) {
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

func (c *feedClient) getPost(ctx context.Context, slug string) (*feedPost, error) {
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
func (c *feedClient) AppendToPost(ctx context.Context, slug, body string, tags []string) (string, error) {
	cur, err := c.getPost(ctx, slug)
	if err != nil {
		return "", err
	}
	merged := strings.TrimSpace(cur.Body)
	if body != "" {
		merged = strings.TrimSpace(merged + "\n\n" + body)
	}
	_, err = c.putPost(ctx, slug, merged, dedupe(append(append([]string{}, cur.Tags...), tags...)))
	return slug, err
}

// RetagPost replaces a post's tags, leaving the body untouched.
func (c *feedClient) RetagPost(ctx context.Context, slug string, tags []string) error {
	cur, err := c.getPost(ctx, slug)
	if err != nil {
		return err
	}
	_, err = c.putPost(ctx, slug, cur.Body, tags)
	return err
}

func (c *feedClient) putPost(ctx context.Context, slug, body string, tags []string) ([]byte, error) {
	buf, err := json.Marshal(map[string]any{"body": body, "tags": orEmpty(tags)})
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPut, "/api/posts/"+slug, "application/json", bytes.NewReader(buf))
}

func (c *feedClient) DeletePost(ctx context.Context, slug string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/posts/"+slug, "", nil)
	return err
}

// ── bookmarks ──────────────────────────────────────────────────────────────

type bookmarksClient struct {
	svc
	DefaultCategory string
}

func (c *bookmarksClient) Create(ctx context.Context, link, category string) (string, error) {
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

func (c *bookmarksClient) Delete(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/bookmarks/"+id, "", nil)
	return err
}

// ── scrap ──────────────────────────────────────────────────────────────────

type scrapClient struct{ svc }

// Create posts a paste. scrap takes the raw body and its options as query
// parameters, and answers with the URL as plain text rather than JSON.
func (c *scrapClient) Create(ctx context.Context, text string) (string, string, error) {
	out, err := c.do(ctx, http.MethodPost, "/api/pastes?visibility=unlisted",
		"text/plain; charset=utf-8", strings.NewReader(text))
	if err != nil {
		return "", "", err
	}
	pasteURL := firstLine(out)
	if pasteURL == "" {
		return "", "", fmt.Errorf("scrap returned no url")
	}
	// Unlisted by default: a texted paste is a link to hand to someone, not a
	// thing to list publicly.
	id := pasteURL[strings.LastIndexByte(pasteURL, '/')+1:]
	return pasteURL, id, nil
}

func (c *scrapClient) Delete(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/pastes/"+id, "", nil)
	return err
}

// ── qr ─────────────────────────────────────────────────────────────────────

type qrClient struct {
	svc
	PublicURL string
}

func (c *qrClient) Create(ctx context.Context, target, label string) (string, error) {
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
func (c *qrClient) PNG(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/qr/"+id+".png", "", nil)
}

func (c *qrClient) Delete(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/codes/"+id, "", nil)
	return err
}

// ── fleet status ───────────────────────────────────────────────────────────

// fleetStatus probes every configured service's public /status concurrently and
// renders a compact report. It needs no credentials: /status is public on every
// farfield app precisely so it can be polled.
func (s *Server) fleetStatus(ctx context.Context) string {
	if len(s.targets) == 0 {
		return "no targets configured"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	type result struct {
		name string
		ok   bool
		note string
	}
	results := make([]result, len(s.targets))
	var wg sync.WaitGroup
	for i, t := range s.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = result{name: t.Name}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				strings.TrimRight(t.URL, "/")+"/status", nil)
			if err != nil {
				results[i].note = "bad url"
				return
			}
			req.Header.Set("User-Agent", userAgent)
			resp, err := statusClient.Do(req)
			if err != nil {
				results[i].note = "unreachable"
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
			results[i].ok = resp.StatusCode == http.StatusOK
			if !results[i].ok {
				results[i].note = resp.Status
			}
		}()
	}
	wg.Wait()

	var down []string
	up := 0
	for _, r := range results {
		if r.ok {
			up++
			continue
		}
		down = append(down, r.name+" ("+r.note+")")
	}
	sort.Strings(down)
	if len(down) == 0 {
		return fmt.Sprintf("all %d services up", up)
	}
	return fmt.Sprintf("%d/%d up\ndown: %s", up, len(results), strings.Join(down, ", "))
}

// statusClient is separate from the sibling clients: status probes should give
// up fast so one hung service cannot stall the whole report.
var statusClient = &http.Client{Timeout: 5 * time.Second}

// pulseSummary renders today's traffic and any open incidents.
func (s *Server) pulseSummary(ctx context.Context) (string, error) {
	out, err := s.pulseSvc.do(ctx, http.MethodGet, "/api/overview", "", nil)
	if err != nil {
		return "", err
	}
	var ov struct {
		Targets []struct {
			Name string `json:"name"`
			Up24 string `json:"up24h"`
			Last *struct {
				OK        bool  `json:"ok"`
				LatencyMS int64 `json:"latencyMs"`
			} `json:"last"`
			Incident *struct {
				OpenedAt string `json:"openedAt"`
				LastErr  string `json:"lastErr"`
			} `json:"incident"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(out, &ov); err != nil {
		return "", err
	}

	up, down := 0, 0
	var lines []string
	for _, t := range ov.Targets {
		if t.Last != nil && t.Last.OK {
			up++
		} else {
			down++
		}
		// Only open incidents get a line — a healthy fleet should answer in one.
		if t.Incident != nil {
			lines = append(lines, fmt.Sprintf("! %s down since %s — %s",
				t.Name, t.Incident.OpenedAt, firstLine([]byte(t.Incident.LastErr))))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d targets · %d up · %d down", len(ov.Targets), up, down)
	sort.Strings(lines)
	for _, line := range lines {
		b.WriteString("\n" + line)
	}
	return b.String(), nil
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
