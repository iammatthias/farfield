package markdown

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// metaServer answers blob metadata lookups: cid "img1"/"img2" are images,
// "vid1" is a video, everything else 404s.
func metaServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "img"):
			w.Write([]byte(`{"mime":"image/jpeg"}`))
		case strings.Contains(r.URL.Path, "vid"):
			w.Write([]byte(`{"mime":"video/mp4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRenderBlobEmbeds(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL, PublicBase: "https://blobs.example"}

	got := string(r.Render(context.Background(), "before\n\n![](blob://img1)\n\nafter blob://vid1 end"))

	if !strings.Contains(got, `<img class="blob-media standalone" src="https://blobs.example/blobs/img1"`) {
		t.Errorf("standalone image not rendered:\n%s", got)
	}
	if !strings.Contains(got, `<video class="blob-media inline"`) {
		t.Errorf("inline video not rendered:\n%s", got)
	}
	if strings.Contains(got, "ffblobembed") {
		t.Errorf("placeholder leaked into output:\n%s", got)
	}
}

func TestRenderUnknownBlobFallsBackToLink(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL}
	got := string(r.Render(context.Background(), "blob://missing123"))
	if !strings.Contains(got, `<a class="blob-file"`) || !strings.Contains(got, "blob://missing123") {
		t.Errorf("dead blob should render as a link:\n%s", got)
	}
}

func TestRenderSeries(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{
		MetaBase: srv.URL,
		Series: func(_ context.Context, slug string) (string, bool) {
			if slug == "trip" {
				return `<img src="x">`, true
			}
			return "", false
		},
	}

	got := string(r.Render(context.Background(), "![](series://trip)\n\nseries://gone"))
	if !strings.Contains(got, `<figure class="series-embed"><img src="x"></figure>`) {
		t.Errorf("series not spliced:\n%s", got)
	}
	if !strings.Contains(got, `series://gone`) || !strings.Contains(got, "series-missing") {
		t.Errorf("unresolved series should render as a marked ref:\n%s", got)
	}
}

func TestRenderSeriesIgnoredWithoutResolver(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL}
	got := string(r.Render(context.Background(), "series://trip"))
	if !strings.Contains(got, "series://trip") {
		t.Errorf("series ref should stay literal without a resolver:\n%s", got)
	}
}

func TestHardWraps(t *testing.T) {
	srv := metaServer(t)
	soft := &Renderer{MetaBase: srv.URL}
	hard := &Renderer{MetaBase: srv.URL, HardWraps: true}

	if got := string(soft.Render(context.Background(), "one\ntwo")); strings.Contains(got, "<br") {
		t.Errorf("soft renderer inserted a hard break:\n%s", got)
	}
	if got := string(hard.Render(context.Background(), "one\ntwo")); !strings.Contains(got, "<br") {
		t.Errorf("hard-wrap renderer kept the newline soft:\n%s", got)
	}
}

func TestRawHTMLIsOmitted(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL}
	got := string(r.Render(context.Background(), `<script>alert(1)</script>`))
	if strings.Contains(got, "<script>") {
		t.Errorf("raw HTML must not pass through:\n%s", got)
	}
}
