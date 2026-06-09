package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
	counts := map[string]int{}
	blobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/meta") {
			http.NotFound(w, r)
			return
		}
		cid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/blobs/"), "/meta")
		counts[cid]++
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

	renderer := newBodyRenderer(context.Background(), blobs.URL, "https://public.example", &sync.Map{})
	out := string(renderer.render("![](blob://bimg)\n\nHello <b>world</b> blob://bimg\n\nblob://bvid\n\nListen to blob://baud or grab blob://bfile."))
	_ = renderer.render("blob://bimg")

	for _, want := range []string{
		`<img class="blob-media standalone" src="https://public.example/blobs/bimg" alt="" loading="lazy" decoding="async">`,
		"Hello &lt;b&gt;world&lt;/b&gt; ",
		`<img class="blob-media inline" src="https://public.example/blobs/bimg" alt="" loading="lazy" decoding="async">`,
		`<video class="blob-media standalone" controls preload="metadata" src="https://public.example/blobs/bvid"></video>`,
		`<audio class="blob-media inline" controls preload="metadata" src="https://public.example/blobs/baud"></audio>`,
		`<a class="blob-file" href="https://public.example/blobs/bfile">blob://bfile</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered body missing %q:\n%s", want, out)
		}
	}
	if counts["bimg"] != 1 || counts["bvid"] != 1 || counts["baud"] != 1 || counts["bfile"] != 1 {
		t.Fatalf("metadata fetch counts = %#v, want each CID once", counts)
	}
}
