package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/markdown"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

func TestSplitTags(t *testing.T) {
	got := splitTags("life, web ,, life , go ")
	want := []string{"life", "web", "go"}
	if len(got) != len(want) {
		t.Fatalf("splitTags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tag %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEncodeDecodeTags(t *testing.T) {
	if got := decodeTags(encodeTags(nil)); len(got) != 0 {
		t.Errorf("nil round-trip = %v, want empty", got)
	}
	r := decodeTags(encodeTags([]string{"x", "y"}))
	if len(r) != 2 || r[0] != "x" || r[1] != "y" {
		t.Errorf("round-trip = %v, want [x y]", r)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	front, body, err := splitFrontmatter("---\ncreated: \"2026-01-01T00:00:00Z\"\n---\nhello there")
	if err != nil {
		t.Fatalf("splitFrontmatter: %v", err)
	}
	if body != "hello there" {
		t.Errorf("body = %q, want %q", body, "hello there")
	}
	if front == "" {
		t.Error("front is empty")
	}
}

func TestRenderPostBodyResolvesBlobEmbeds(t *testing.T) {
	// The renderer resolves distinct CIDs in parallel, so the stub's handler
	// runs on several goroutines at once and the tally needs a lock — without
	// one the map write races, which the runtime reports as a fatal error
	// rather than a test failure.
	var mu sync.Mutex
	counts := map[string]int{}
	blobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/meta") {
			http.NotFound(w, r)
			return
		}
		cid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/blobs/"), "/meta")
		mu.Lock()
		counts[cid]++
		mu.Unlock()
		mime := map[string]string{
			"bimg":  "image/png",
			"bvid":  "video/mp4",
			"baud":  "audio/mpeg",
			"bfile": "application/pdf",
		}[cid]
		if mime == "" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"mime": mime})
	}))
	defer blobs.Close()

	// The renderer is built the way run() builds the server's md field.
	s := &Server{md: &markdown.Renderer{MetaBase: blobs.URL, PublicBase: "https://public.example", HardWraps: true}}
	out := string(s.md.Render(context.Background(), "![](blob://bimg)\n\nHello <b>world</b> blob://bimg\n\nblob://bvid\n\nListen to blob://baud or grab blob://bfile — also [the file](blob://bfile)."))
	_ = s.md.Render(context.Background(), "blob://bimg")

	for _, want := range []string{
		`<img class="blob-media standalone" src="https://public.example/blobs/bimg" alt="" loading="lazy" decoding="async">`,
		"Hello",
		`<img class="blob-media inline" src="https://public.example/blobs/bimg" alt="" loading="lazy" decoding="async">`,
		`<video class="blob-media standalone" controls preload="metadata" src="https://public.example/blobs/bvid"></video>`,
		`<audio class="blob-media inline" controls preload="metadata" src="https://public.example/blobs/baud"></audio>`,
		// A bare ref to a non-media blob falls back to a file link…
		`<a class="blob-file" href="https://public.example/blobs/bfile">blob://bfile</a>`,
		// …and [text](blob://cid) is a file link that keeps the link text.
		`<a class="blob-file" href="https://public.example/blobs/bfile">the file</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered body missing %q:\n%s", want, out)
		}
	}
	// Raw HTML in the source must never pass through as markup.
	if strings.Contains(out, "<b>world</b>") {
		t.Fatalf("raw HTML leaked into rendered body:\n%s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["bimg"] != 1 || counts["bvid"] != 1 || counts["baud"] != 1 || counts["bfile"] != 1 {
		t.Fatalf("metadata fetch counts = %#v, want each CID once", counts)
	}
}

// newTestServer builds a routed feed server over a temp database, plus a
// valid session cookie for the session-gated admin surface.
func newTestServer(t *testing.T) (*httptest.Server, *http.Cookie) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "feed.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{
		db:   db,
		auth: &web.Auth{DB: db, Password: "pw"},
		md:   &markdown.Renderer{MetaBase: "http://127.0.0.1:0", PublicBase: "https://public.example", HardWraps: true},
	}
	tmpl, err := web.ParseTemplates(assets, nil)
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	s.rd = &web.Renderer{Templates: tmpl}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)

	tok := auth.NewSessionToken()
	if err := store.InsertSession(db, tok, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return srv, &http.Cookie{Name: "session", Value: tok}
}

// TestPreviewRendersHardWraps confirms the editor's live-preview endpoint is
// session-gated and renders markdown with feed's hard-wrap semantics — a
// single newline stays a visible line break.
func TestPreviewRendersHardWraps(t *testing.T) {
	srv, cookie := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	post := func(withSession bool) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/preview",
			strings.NewReader(`{"body":"line one\nline two"}`))
		req.Header.Set("Content-Type", "application/json")
		if withSession {
			req.AddCookie(cookie)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /preview: %v", err)
		}
		return resp
	}

	// No session → redirect to login, never rendered HTML.
	resp := post(false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/preview without session = %d, want 303", resp.StatusCode)
	}

	resp = post(true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/preview with session = %d, want 200", resp.StatusCode)
	}
	var out struct {
		HTML string `json:"html"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	if !strings.Contains(out.HTML, "<br>") {
		t.Errorf("preview HTML missing hard-wrap <br>:\n%s", out.HTML)
	}
	if !strings.Contains(out.HTML, "line one") || !strings.Contains(out.HTML, "line two") {
		t.Errorf("preview HTML missing body text:\n%s", out.HTML)
	}
}

// TestComposerRendersDocumentFirst confirms the admin composer page carries
// the document-first editor markup: the doc-card trigger, the hidden
// markdown fallback, the metadata rail, and the editor config with feed's
// hard-wrap flag.
func TestComposerRendersDocumentFirst(t *testing.T) {
	srv, cookie := newTestServer(t)

	req, _ := http.NewRequest("GET", srv.URL+"/new", nil)
	req.AddCookie(cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /new: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /new = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /new: %v", err)
	}
	page := string(raw)
	for _, want := range []string{
		`data-async`,
		`data-doc-open`,
		`doc-card empty`, // new post: empty state
		`doc-fallback`,
		`<textarea id="body"`,
		`edit-rail`,
		`doc-words`,
		`save-note`,
		`preview:"/preview"`,
		`editdoc:"/editdoc"`,
		`hardWraps:true`,
		`/static/editor.js`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("composer page missing %q", want)
		}
	}
	if strings.Contains(page, "embed-toolbar") {
		t.Error("composer page still carries the old embed toolbar")
	}
}

// TestSaveAnswersJSON confirms the admin create/update handlers answer the
// editor's async saves (Accept: application/json) with {slug, action,
// editURL} on success and a JSON error on failure, while plain form posts
// still redirect.
func TestSaveAnswersJSON(t *testing.T) {
	srv, cookie := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	post := func(path string, form url.Values, acceptJSON bool) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if acceptJSON {
			req.Header.Set("Accept", "application/json")
		}
		req.AddCookie(cookie)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Async create → JSON with the canonical URLs.
	resp := post("/posts", url.Values{"body": {"hello note"}, "tags": {"a, b"}}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("async create = %d, want 200", resp.StatusCode)
	}
	var saved struct{ Slug, Action, EditURL string }
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	resp.Body.Close()
	if saved.Slug == "" {
		t.Fatal("async create returned no slug")
	}
	if saved.Action != "/posts/"+saved.Slug {
		t.Errorf("action = %q, want %q", saved.Action, "/posts/"+saved.Slug)
	}
	if saved.EditURL != "/posts/"+saved.Slug+"/edit" {
		t.Errorf("editURL = %q, want %q", saved.EditURL, "/posts/"+saved.Slug+"/edit")
	}

	// Async update of the created post → same JSON shape.
	resp = post("/posts/"+saved.Slug, url.Values{"body": {"hello again"}}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("async update = %d, want 200", resp.StatusCode)
	}
	var updated struct{ Slug string }
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	resp.Body.Close()
	if updated.Slug != saved.Slug {
		t.Errorf("update slug = %q, want %q", updated.Slug, saved.Slug)
	}

	// Async save with an empty body → JSON 400, not a re-rendered form.
	resp = post("/posts", url.Values{"body": {""}}, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("async empty-body create = %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("async error Content-Type = %q, want JSON", ct)
	}
	resp.Body.Close()

	// A plain form post (no Accept: application/json) still redirects home.
	resp = post("/posts", url.Values{"body": {"plain form post"}}, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("plain create = %d, want 303", resp.StatusCode)
	}
}

func TestRenderPostBodyMarkdown(t *testing.T) {
	s := &Server{md: &markdown.Renderer{MetaBase: "http://127.0.0.1:0", PublicBase: "https://public.example", HardWraps: true}}
	out := string(s.md.Render(context.Background(),
		"A [link](https://example.com) and **bold** text.\nsecond line\n\n- one\n- two\n\n`code` and https://auto.example"))

	for _, want := range []string{
		`<a href="https://example.com">link</a>`,
		`<strong>bold</strong>`,
		`<br>`, // hard wraps: a single newline stays a line break
		`<li>one</li>`,
		`<code>code</code>`,
		`<a href="https://auto.example">https://auto.example</a>`, // GFM autolink
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("markdown body missing %q:\n%s", want, out)
		}
	}
}
