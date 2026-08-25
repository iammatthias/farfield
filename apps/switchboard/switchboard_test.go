package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/capability"
	"github.com/iammatthias/farfield/lib/web"
)

const testSecret = "shhh"

// newTestServer builds a switchboard wired to stub siblings. Nothing here
// reaches the network: the Photon client is left nil (so replies are skipped)
// and every sibling is an httptest server.
func newTestServer(t *testing.T) (*Server, *httptest.Server, *feedStub) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "switchboard.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	feed := &feedStub{}
	feedSrv := httptest.NewServer(feed)
	t.Cleanup(feedSrv.Close)

	tmpl, err := web.ParseTemplates(assets, nil)
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	s := &Server{
		db:            db,
		auth:          &web.Auth{DB: db, Password: "pw"},
		rd:            &web.Renderer{Templates: tmpl},
		webhookSecret: testSecret,
		allow:         map[string]bool{"+15551234567": true},
		rl:            web.NewRateLimiter(inboundPerMin, time.Minute),
	}
	// Every sibling points at the one stub; lib/capability does not care that
	// feed, bookmarks, scrap and qr happen to be the same server here.
	s.caps = capability.New(capability.Config{
		FeedURL: feedSrv.URL, FeedKey: "k",
		BookmarksURL: feedSrv.URL, BookmarksKey: "k",
		ScrapURL: feedSrv.URL, ScrapKey: "k",
		QRURL: feedSrv.URL, QRKey: "k",
	})
	s.reg = s.commands()

	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return s, srv, feed
}

// feedStub stands in for every sibling: it records what it was asked to do and
// answers in each service's real response shape.
type feedStub struct {
	mu     sync.Mutex
	posts  []map[string]any
	medias int
	n      int
}

func (f *feedStub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *feedStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/posts" && r.Method == http.MethodPost:
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		f.posts = append(f.posts, p)
		f.n++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"slug":"post%d"}`, f.n)
	case r.URL.Path == "/api/posts/media" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.medias++
		f.n++
		f.posts = append(f.posts, map[string]any{"multipart": string(body)})
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"slug":"post%d"}`, f.n)
	case r.URL.Path == "/api/bookmarks" && r.Method == http.MethodPost:
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.posts = append(f.posts, b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"bm1"}`)
	case r.URL.Path == "/api/pastes" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.posts = append(f.posts, map[string]any{"paste": string(body)})
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, "https://scrap.example/abc123")
	default:
		http.NotFound(w, r)
	}
}

// post sends a signed delivery and returns the status code.
func post(t *testing.T, srv *httptest.Server, id, sender, text string, opts ...func(*envelope)) int {
	t.Helper()
	env := envelope{Event: "messages"}
	env.Message.ID = id
	env.Message.Platform = "imessage"
	env.Message.Direction = "inbound"
	env.Message.Sender.ID = sender
	env.Message.Space.ID = "any;-;" + sender
	env.Message.Content = content{Type: "text", Text: text}
	for _, o := range opts {
		o(&env)
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return postRaw(t, srv, body, sign(body, time.Now()), strconv.FormatInt(time.Now().Unix(), 10))
}

func postRaw(t *testing.T, srv *httptest.Server, body []byte, sig, ts string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hooks/photon", strings.NewReader(string(body)))
	req.Header.Set(hdrSignature, sig)
	req.Header.Set(hdrTimestamp, ts)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func sign(body []byte, at time.Time) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	fmt.Fprintf(mac, "v0:%d:%s", at.Unix(), body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// ── signature and replay ───────────────────────────────────────────────────

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"event":"messages"}`)
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	good := sign(body, now)

	if !verifySignature(testSecret, ts, body, good) {
		t.Error("valid signature rejected")
	}
	if verifySignature(testSecret, ts, []byte(`{"event":"other"}`), good) {
		t.Error("signature accepted over tampered body")
	}
	if verifySignature("other", ts, body, good) {
		t.Error("signature accepted under the wrong secret")
	}
	if verifySignature(testSecret, "1", body, good) {
		t.Error("signature accepted with a substituted timestamp")
	}
	if verifySignature(testSecret, ts, body, strings.TrimPrefix(good, "v0=")) {
		t.Error("signature accepted without its version prefix")
	}
	if verifySignature("", ts, body, good) {
		t.Error("empty secret must never verify")
	}
}

func TestFreshTimestamp(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		ts   string
		want bool
	}{
		{"now", strconv.FormatInt(now.Unix(), 10), true},
		{"recent", strconv.FormatInt(now.Add(-2*time.Minute).Unix(), 10), true},
		{"stale", strconv.FormatInt(now.Add(-30*time.Minute).Unix(), 10), false},
		{"far future", strconv.FormatInt(now.Add(30*time.Minute).Unix(), 10), false},
		{"garbage", "not-a-number", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := freshTimestamp(c.ts, now); got != c.want {
			t.Errorf("%s: freshTimestamp = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	_, srv, feed := newTestServer(t)
	body := []byte(`{"event":"messages","message":{"id":"m1"}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	if got := postRaw(t, srv, body, "v0=deadbeef", ts); got != http.StatusUnauthorized {
		t.Errorf("bad signature = %d, want 401", got)
	}
	if got := postRaw(t, srv, body, "", ts); got != http.StatusUnauthorized {
		t.Errorf("missing signature = %d, want 401", got)
	}
	if feed.count() != 0 {
		t.Error("an unsigned delivery reached a sibling service")
	}
}

func TestWebhookRejectsReplay(t *testing.T) {
	_, srv, _ := newTestServer(t)
	old := time.Now().Add(-time.Hour)
	body, _ := json.Marshal(envelope{Event: "messages"})
	// Correctly signed for its own timestamp — only staleness disqualifies it.
	got := postRaw(t, srv, body, sign(body, old), strconv.FormatInt(old.Unix(), 10))
	if got != http.StatusUnauthorized {
		t.Errorf("replayed delivery = %d, want 401", got)
	}
}

// A webhook with no secret configured must not exist at all: fail closed, since
// the alternative publishes whatever anyone posts to a public feed.
func TestWebhookDisabledWithoutSecret(t *testing.T) {
	s, srv, _ := newTestServer(t)
	s.webhookSecret = ""
	body, _ := json.Marshal(envelope{Event: "messages"})
	if got := postRaw(t, srv, body, sign(body, time.Now()),
		strconv.FormatInt(time.Now().Unix(), 10)); got != http.StatusNotFound {
		t.Errorf("hook without a secret = %d, want 404", got)
	}
}

// ── filters ────────────────────────────────────────────────────────────────

func TestWebhookIgnoresStranger(t *testing.T) {
	_, srv, feed := newTestServer(t)
	if got := post(t, srv, "m1", "+19995550000", "hello"); got != http.StatusOK {
		t.Errorf("stranger = %d, want 200 (acknowledged, not retried)", got)
	}
	if feed.count() != 0 {
		t.Error("a stranger's message reached feed")
	}
}

func TestWebhookIgnoresGroupThread(t *testing.T) {
	_, srv, feed := newTestServer(t)
	post(t, srv, "m1", "+15551234567", "hello", func(e *envelope) {
		e.Message.Space.Type = "group"
	})
	if feed.count() != 0 {
		t.Error("a group message reached feed")
	}
}

func TestWebhookDedupes(t *testing.T) {
	_, srv, feed := newTestServer(t)
	post(t, srv, "same-id", "+15551234567", "hello")
	post(t, srv, "same-id", "+15551234567", "hello")
	if feed.count() != 1 {
		t.Errorf("posts = %d, want 1 — a redelivered webhook posted twice", feed.count())
	}
}

// ── routing ────────────────────────────────────────────────────────────────

func TestBareURLBecomesBookmark(t *testing.T) {
	_, srv, feed := newTestServer(t)
	post(t, srv, "m1", "+15551234567", "https://example.com/article")
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if len(feed.posts) != 1 {
		t.Fatalf("calls = %d, want 1", len(feed.posts))
	}
	if feed.posts[0]["url"] != "https://example.com/article" {
		t.Errorf("did not route to bookmarks: %v", feed.posts[0])
	}
}

func TestURLWithTextBecomesPost(t *testing.T) {
	_, srv, feed := newTestServer(t)
	post(t, srv, "m1", "+15551234567", "worth reading https://example.com/article")
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if len(feed.posts) != 1 {
		t.Fatalf("calls = %d, want 1", len(feed.posts))
	}
	if _, isBookmark := feed.posts[0]["url"]; isBookmark {
		t.Error("a URL with surrounding text should be a post, not a bookmark")
	}
}

func TestNormalizeHandle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"+1 (555) 123-4567", "+15551234567"},
		{"+15551234567", "+15551234567"},
		{" Me@Example.COM ", "me@example.com"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeHandle(c.in); got != c.want {
			t.Errorf("normalizeHandle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A caption and its photos arrive as one "group" content, which is what makes
// "photos sent together become one post" work without guessing from timing.
func TestFlattenGroup(t *testing.T) {
	c := content{Type: "group", Items: []content{
		{Type: "text", Text: "beach day"},
		{Type: "attachment", ID: "a1", Name: "IMG_1.HEIC", MimeType: "image/heic"},
		{Type: "attachment", ID: "a2", Name: "IMG_2.HEIC", MimeType: "image/heic"},
		{Type: "reaction"},
	}}
	text, atts := flatten(c)
	if text != "beach day" {
		t.Errorf("text = %q, want %q", text, "beach day")
	}
	if len(atts) != 2 || atts[0].ID != "a1" || atts[1].ID != "a2" {
		t.Errorf("attachments = %+v, want the two images in order", atts)
	}
}

func TestFlattenIgnoresNonActionable(t *testing.T) {
	text, atts := flatten(content{Type: "reaction"})
	if text != "" || len(atts) != 0 {
		t.Errorf("reaction flattened to (%q, %v), want empty", text, atts)
	}
}

// An empty message is recorded and ignored rather than posted.
func TestEmptyMessageIgnored(t *testing.T) {
	s, srv, feed := newTestServer(t)
	post(t, srv, "m1", "+15551234567", "")
	if feed.count() != 0 {
		t.Error("an empty message reached feed")
	}
	rec, err := getMessage(s.db, "m1")
	if err != nil || rec == nil {
		t.Fatalf("message not recorded: %v", err)
	}
	if rec.Status != statusIgnored {
		t.Errorf("status = %q, want %q", rec.Status, statusIgnored)
	}
}

func TestAllowFailsClosed(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.allow = nil
	if s.allowed("+15551234567") {
		t.Error("an empty allowlist must permit nobody")
	}
	if s.allowed("") {
		t.Error("an empty handle must never be allowed")
	}
}

func TestParseTargets(t *testing.T) {
	got := parseTargets("feed=http://feed:8788, qr=http://qr:8794 ,bad,")
	if len(got) != 2 {
		t.Fatalf("targets = %+v, want 2", got)
	}
	if got[0].Name != "feed" || got[1].URL != "http://qr:8794" {
		t.Errorf("targets = %+v", got)
	}
}

// The shape that actually arrives: content is flat — text plus an attachments
// array keyed by guid/fileName/totalBytes — not the union the SDK docs
// describe. Parsing only the union lost a real message its photo AND its
// caption, so both shapes are pinned here.
func TestFlattenFlatContent(t *testing.T) {
	c := content{
		Text: "￼Test",
		Attachments: []content{{
			GUID:       "spc-att-aa2fa55c",
			FileName:   "IMG_6899.HEIC",
			MimeType:   "image/heic",
			TotalBytes: "1338165",
		}},
	}
	text, atts := flatten(c)
	if text != "Test" {
		t.Errorf("text = %q, want %q (U+FFFC placeholder must be stripped)", text, "Test")
	}
	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(atts))
	}
	if atts[0].ID != "spc-att-aa2fa55c" {
		t.Errorf("id = %q, want the guid", atts[0].ID)
	}
	if atts[0].Name != "IMG_6899.HEIC" || atts[0].MimeType != "image/heic" {
		t.Errorf("attachment = %+v", atts[0])
	}
	if atts[0].Size != 1338165 {
		t.Errorf("size = %d, want 1338165 (totalBytes arrives as a string)", atts[0].Size)
	}
}

// A bare photo with no caption: the text is only the placeholder, so it must
// flatten to empty rather than to a stray box glyph.
func TestFlattenBarePhoto(t *testing.T) {
	text, atts := flatten(content{
		Text:        "￼",
		Attachments: []content{{GUID: "g1", FileName: "IMG.HEIC"}},
	})
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
	if len(atts) != 1 {
		t.Errorf("attachments = %d, want 1", len(atts))
	}
}

// The documented union shape must keep working — both are accepted.
func TestFlattenUnionStillWorks(t *testing.T) {
	text, atts := flatten(content{Type: "group", Items: []content{
		{Type: "text", Text: "beach day"},
		{Type: "attachment", ID: "a1", Name: "IMG_1.HEIC", MimeType: "image/heic", Size: 10},
	}})
	if text != "beach day" || len(atts) != 1 || atts[0].ID != "a1" {
		t.Errorf("union shape broke: text=%q atts=%+v", text, atts)
	}
}

// One attachment must not be counted twice when it appears under both a type
// tag and an attachments array.
func TestFlattenDedupesAttachments(t *testing.T) {
	_, atts := flatten(content{
		Type:        "attachment",
		GUID:        "same",
		FileName:    "x.heic",
		Attachments: []content{{GUID: "same", FileName: "x.heic"}},
	})
	if len(atts) != 1 {
		t.Errorf("attachments = %d, want 1", len(atts))
	}
}

// A photo message must route to feed rather than be ignored. This is the
// regression the live bug produced: a real photo-plus-caption flattened to
// nothing and was recorded as "none/ignored", so neither the image nor the
// caption ever reached the feed.
func TestPhotoRoutesToFeedNotIgnored(t *testing.T) {
	s, srv, _ := newTestServer(t)
	post(t, srv, "m-photo", "+15551234567", "", func(e *envelope) {
		e.Message.Content = content{
			Text:        "￼Test",
			Attachments: []content{{GUID: "g1", FileName: "IMG.HEIC", MimeType: "image/heic"}},
		}
	})
	rec, err := getMessage(s.db, "m-photo")
	if err != nil {
		t.Fatalf("getMessage: %v", err)
	}
	if rec == nil {
		t.Fatal("photo message was not recorded at all")
	}
	if rec.Route != "feed" {
		t.Errorf("route = %q, want %q — the photo was ignored", rec.Route, "feed")
	}
	if rec.Body != "Test" {
		t.Errorf("body = %q, want %q — the caption was lost", rec.Body, "Test")
	}
	// The test server has no Photon line, so fetching the bytes fails and the
	// dispatch is recorded as an error. That is the correct outcome here: what
	// matters is that it tried, rather than silently dropping the message.
	if rec.Status != statusError {
		t.Errorf("status = %q, want %q (no line configured in tests)", rec.Status, statusError)
	}
}

// ── slash-command dispatch ─────────────────────────────────────────────────

// recorded returns the logged outcome for one delivered message.
func recorded(t *testing.T, s *Server, id string) *Message {
	t.Helper()
	rec, err := getMessage(s.db, id)
	if err != nil || rec == nil {
		t.Fatalf("message %s not recorded: %v", id, err)
	}
	return rec
}

// TestSlashCommandDispatchesFromTheRegistry pins the path that replaces the old
// hand-written switch: the leading slash names a command, the registry resolves
// it, and the row records the command's own name so the audit vocabulary and the
// command table stay the same list.
func TestSlashCommandDispatchesFromTheRegistry(t *testing.T) {
	s, srv, feed := newTestServer(t)
	post(t, srv, "m1", "+15551234567", "/feed a deliberate post #life")

	if feed.count() != 1 {
		t.Fatalf("feed calls = %d, want 1", feed.count())
	}
	feed.mu.Lock()
	body, _ := feed.posts[0]["body"].(string)
	tags, _ := feed.posts[0]["tags"].([]any)
	feed.mu.Unlock()
	if body != "a deliberate post" {
		t.Errorf("body = %q, want the hashtag stripped", body)
	}
	if len(tags) != 1 || tags[0] != "life" {
		t.Errorf("tags = %v, want [life]", tags)
	}

	rec := recorded(t, s, "m1")
	if rec.Route != "feed" || rec.Status != statusOK {
		t.Errorf("route/status = %q/%q, want feed/ok", rec.Route, rec.Status)
	}
}

// An alias resolves to the same command.
func TestSlashCommandAlias(t *testing.T) {
	s, srv, feed := newTestServer(t)
	post(t, srv, "m1", "+15551234567", "/post via the alias")
	if feed.count() != 1 {
		t.Fatalf("feed calls = %d, want 1", feed.count())
	}
	if got := recorded(t, s, "m1").Route; got != "feed" {
		t.Errorf("route = %q, want feed — an alias must record its canonical name", got)
	}
}

// TestUnknownSlashCommandExplains: a mistyped command is someone misremembering
// a name, so it answers with something actionable and touches no service.
func TestUnknownSlashCommandExplains(t *testing.T) {
	s, srv, feed := newTestServer(t)
	post(t, srv, "m1", "+15551234567", "/nope do a thing")

	if feed.count() != 0 {
		t.Error("an unknown command reached a service")
	}
	rec := recorded(t, s, "m1")
	if rec.Status != statusError {
		t.Errorf("status = %q, want %q", rec.Status, statusError)
	}
	if !strings.Contains(rec.Reply, "/nope") || !strings.Contains(rec.Reply, "/help") {
		t.Errorf("reply = %q, want it to name the command and point at /help", rec.Reply)
	}
}

// A command missing a required argument answers with its usage rather than a
// bare failure — over a text message that reply is the only documentation.
func TestMissingArgumentAnswersWithUsage(t *testing.T) {
	s, srv, _ := newTestServer(t)
	post(t, srv, "m1", "+15551234567", "/qr")
	if reply := recorded(t, s, "m1").Reply; !strings.Contains(reply, "/qr <target>") {
		t.Errorf("reply = %q, want the usage", reply)
	}
}

// TestHelpListsEveryRegisteredCommand is the drift guard at the switchboard
// level: help is generated over this surface's registry, so a command added
// without documentation is impossible.
func TestHelpListsEveryRegisteredCommand(t *testing.T) {
	s, srv, _ := newTestServer(t)
	post(t, srv, "m1", "+15551234567", "/help")

	reply := recorded(t, s, "m1").Reply
	for _, spec := range s.reg.Specs() {
		if !strings.Contains(reply, spec.Usage()) {
			t.Errorf("/help omits %q", spec.Usage())
		}
	}
	// The commands that only exist here, beside the shared fleet ones.
	for _, want := range []string{"/undo", "/append", "/tags"} {
		if !strings.Contains(reply, want) {
			t.Errorf("/help omits %s", want)
		}
	}
}

// TestSiblingConfigHonoursTheBindAddress is the regression guard for the defect
// that survived the move to a host unit: as a container switchboard received
// compose DNS names from docker-compose.yml, and as a unit it receives nothing,
// so every sibling URL fell back to loopback. Nothing listens there — the fleet
// publishes on the docker0 gateway — so /feed, /bm, /scrap, /qr and /pulse all
// failed with "connection refused" against perfectly healthy services.
func TestSiblingConfigHonoursTheBindAddress(t *testing.T) {
	t.Setenv("FARFIELD_BIND_IP", "172.17.0.1")
	cfg := siblingConfig()

	for name, url := range map[string]string{
		"feed": cfg.FeedURL, "bookmarks": cfg.BookmarksURL,
		"scrap": cfg.ScrapURL, "qr": cfg.QRURL, "pulse": cfg.PulseURL,
	} {
		if url == "" {
			t.Errorf("%s has no URL", name)
			continue
		}
		if strings.Contains(url, "127.0.0.1") || strings.Contains(url, "localhost") {
			t.Errorf("%s = %q — loopback is where nothing listens once switchboard "+
				"is a host unit", name, url)
		}
		if !strings.Contains(url, "172.17.0.1") {
			t.Errorf("%s = %q, want the bind address", name, url)
		}
	}

	// The status roll-up has to travel the same road.
	for _, target := range cfg.Targets {
		if strings.Contains(target.URL, "127.0.0.1") {
			t.Errorf("status target %s = %q — loopback", target.Name, target.URL)
		}
	}
}

// An explicit env var still wins, for a deployment that is neither.
func TestSiblingConfigEnvOverrides(t *testing.T) {
	t.Setenv("FARFIELD_BIND_IP", "172.17.0.1")
	t.Setenv("FEED_URL", "http://feed.internal:9999")
	if got := siblingConfig().FeedURL; got != "http://feed.internal:9999" {
		t.Errorf("FeedURL = %q, want the override", got)
	}
}
