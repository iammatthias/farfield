package markdown

import (
	"context"
	"strings"
	"testing"
)

func TestEditableBasics(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL, PublicBase: "https://blobs.example"}

	got := string(r.RenderEditable(context.Background(),
		"# Title\n\nSome **bold** and *soft* and `code` and [a link](https://x.dev).\n\n- one\n- two\n\n> quoted\n\n---"))

	for _, want := range []string{
		"<h1>Title</h1>",
		"<strong>bold</strong>",
		"<em>soft</em>",
		"<code>code</code>",
		`<a href="https://x.dev">a link</a>`,
		"<ul><li>one</li><li>two</li></ul>",
		"<blockquote><p>quoted</p></blockquote>",
		"<hr>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestEditableEmbeds(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{
		MetaBase: srv.URL, PublicBase: "https://blobs.example",
		Series: func(_ context.Context, slug string) (string, bool) {
			return "<img src=\"x\">", slug == "trip"
		},
	}

	got := string(r.RenderEditable(context.Background(),
		"![](blob://img1)\n\ninline blob://img2 here\n\n[the file](blob://doc9)\n\n![](series://trip)"))

	if !strings.Contains(got, `<figure class="doc-embed" data-blob="img1" contenteditable="false">`) {
		t.Errorf("standalone blob not an island:\n%s", got)
	}
	if !strings.Contains(got, `data-blob="img2"`) || !strings.Contains(got, `class="blob-media inline"`) {
		t.Errorf("inline blob missing data attr:\n%s", got)
	}
	if !strings.Contains(got, `<a class="blob-file" data-blob="doc9"`) || !strings.Contains(got, ">the file</a>") {
		t.Errorf("blob file link not rendered:\n%s", got)
	}
	if !strings.Contains(got, `data-series="trip"`) || !strings.Contains(got, `contenteditable="false"`) {
		t.Errorf("series not an island:\n%s", got)
	}
}

func TestEditableVerbatimTable(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL}

	table := "| a | b |\n|---|---|\n| 1 | blob://img1 |"
	got := string(r.RenderEditable(context.Background(), "before\n\n"+table+"\n\nafter"))

	if !strings.Contains(got, `<pre class="md-verbatim" data-verbatim="1">`) {
		t.Fatalf("table should be a verbatim block:\n%s", got)
	}
	// The verbatim block must carry the exact source — pipes, separator row,
	// and the blob ref restored from its placeholder.
	for _, want := range []string{"| a | b |", "|---|---|", "blob://img1"} {
		if !strings.Contains(got, want) {
			t.Errorf("verbatim lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ffblobembed") {
		t.Errorf("placeholder leaked:\n%s", got)
	}
}

func TestEditableCodeBlockKeepsEmbedsAsSource(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL}

	got := string(r.RenderEditable(context.Background(),
		"```bash\ncurl blob://img1\n```"))
	if !strings.Contains(got, `<pre data-code="1" data-lang="bash"><code>curl blob://img1</code></pre>`) {
		t.Errorf("code block mangled:\n%s", got)
	}
}

func TestEditableTaskList(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL}
	got := string(r.RenderEditable(context.Background(), "- [x] done\n- [ ] todo"))
	if !strings.Contains(got, `<input type="checkbox" checked>`) || !strings.Contains(got, `<input type="checkbox">`) {
		t.Errorf("task checkboxes missing:\n%s", got)
	}
}

func TestEditableRawHTMLStaysInert(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL}
	got := string(r.RenderEditable(context.Background(), "before <span onclick=\"x()\">hi</span> after"))
	if strings.Contains(got, "<span onclick") {
		t.Fatalf("raw HTML must not go live:\n%s", got)
	}
	if !strings.Contains(got, "data-verbatim") {
		t.Errorf("raw HTML should be carried verbatim:\n%s", got)
	}
}

func TestRenderBlobFileLink(t *testing.T) {
	srv := metaServer(t)
	r := &Renderer{MetaBase: srv.URL, PublicBase: "https://blobs.example"}
	got := string(r.Render(context.Background(), "grab [the dataset](blob://doc42) here"))
	if !strings.Contains(got, `<a class="blob-file" href="https://blobs.example/blobs/doc42">the dataset</a>`) {
		t.Errorf("blob file link not rendered:\n%s", got)
	}
	if strings.Contains(got, "ffblobembed") {
		t.Errorf("placeholder leaked:\n%s", got)
	}
}

func TestEditableHardWrapsKeepSoftBreaks(t *testing.T) {
	srv := metaServer(t)
	soft := &Renderer{MetaBase: srv.URL}
	hard := &Renderer{MetaBase: srv.URL, HardWraps: true}

	if got := string(soft.RenderEditable(context.Background(), "one\ntwo")); strings.Contains(got, "<br>") {
		t.Errorf("soft renderer must not emit <br> for a bare newline:\n%s", got)
	}
	if got := string(hard.RenderEditable(context.Background(), "one\ntwo")); !strings.Contains(got, "one<br>two") {
		t.Errorf("hard-wrap renderer must keep the newline as <br>:\n%s", got)
	}
}
