package markdown

import (
	"context"
	"fmt"
	"html/template"
	"strings"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	ghtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

// RenderEditable converts a markdown body to the constrained HTML schema the
// shared document editor edits in place:
//
//	blocks:  <p> <h1>–<h6> <ul>/<ol>/<li> <blockquote> <hr>
//	         <pre data-code data-lang><code>  fenced/indented code
//	         <pre class="md-verbatim" data-verbatim>  anything else, as source
//	inline:  <strong> <em> <del> <code> <a href> <br> <img> <input type=checkbox>
//	embeds:  <figure class="doc-embed" data-blob|data-series contenteditable=false>
//	         <img data-blob> inline, <a class="blob-file" data-blob> file links
//
// Blocks goldmark parses but the editor cannot faithfully round-trip (tables,
// raw HTML, anything unrecognized) become verbatim blocks carrying their
// exact source, so a save can never corrupt them. The client serializes this
// schema — and only this schema — back to markdown; parsing stays goldmark's
// job.
func (r *Renderer) RenderEditable(ctx context.Context, body string) template.HTML {
	src, embeds := r.prepass(body)
	doc := mdSoft.Parser().Parse(text.NewReader([]byte(src)))

	var b strings.Builder
	for c := doc.FirstChild(); c != nil; c = c.NextSibling() {
		r.editBlock(&b, []byte(src), c, embeds)
	}
	html := b.String()

	failed := make(map[string]blobLookup)
	for i, e := range embeds {
		p := placeholderTok(i)
		var out string
		switch e.kind {
		case embedSeries:
			out = r.editableSeries(ctx, e.series)
		case embedLink:
			out = r.renderBlobLink(e, ` data-blob="`+e.cid+`"`)
		default:
			out = r.renderBlob(ctx, e.cid, e.alt, e.standalone, failed, ` data-blob="`+e.cid+`"`)
			if e.standalone {
				out = `<figure class="doc-embed" data-blob="` + e.cid + `" contenteditable="false">` + out + `</figure>`
			}
		}
		if e.standalone {
			html = strings.ReplaceAll(html, "<p>"+p+"</p>", out)
		}
		html = strings.ReplaceAll(html, p, out)
	}
	return template.HTML(html)
}

// editableSeries renders a series embed as an atomic, non-editable island;
// the data-series attribute is what serializes back, so even a missing
// series round-trips exactly.
func (r *Renderer) editableSeries(ctx context.Context, slug string) string {
	inner, ok := r.Series(ctx, slug)
	if !ok {
		inner = `<p class="series-missing">series://` + template.HTMLEscapeString(slug) + ` (missing)</p>`
	}
	return `<figure class="doc-embed series-embed" data-series="` + template.HTMLEscapeString(slug) +
		`" contenteditable="false">` + inner + `</figure>`
}

func (r *Renderer) editBlock(b *strings.Builder, src []byte, n ast.Node, embeds []embedRef) {
	switch t := n.(type) {
	case *ast.Heading:
		fmt.Fprintf(b, "<h%d>", t.Level)
		r.editInlines(b, src, n, embeds)
		fmt.Fprintf(b, "</h%d>", t.Level)
	case *ast.Paragraph:
		b.WriteString("<p>")
		r.editInlines(b, src, n, embeds)
		b.WriteString("</p>")
	case *ast.TextBlock: // tight list item content
		r.editInlines(b, src, n, embeds)
	case *ast.List:
		tag := "ul"
		attr := ""
		if t.IsOrdered() {
			tag = "ol"
			if t.Start != 1 {
				attr = fmt.Sprintf(` start="%d"`, t.Start)
			}
		}
		b.WriteString("<" + tag + attr + ">")
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			r.editBlock(b, src, c, embeds)
		}
		b.WriteString("</" + tag + ">")
	case *ast.ListItem:
		b.WriteString("<li>")
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			// Unwrap loose-list paragraphs into the item; nested lists and
			// code blocks render as themselves.
			if _, isPara := c.(*ast.Paragraph); isPara {
				if c.PreviousSibling() != nil {
					b.WriteString("<br>")
				}
				r.editInlines(b, src, c, embeds)
				continue
			}
			r.editBlock(b, src, c, embeds)
		}
		b.WriteString("</li>")
	case *ast.Blockquote:
		b.WriteString("<blockquote>")
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			r.editBlock(b, src, c, embeds)
		}
		b.WriteString("</blockquote>")
	case *ast.FencedCodeBlock:
		lang := ""
		if t.Info != nil {
			lang = string(t.Language(src))
		}
		b.WriteString(`<pre data-code="1" data-lang="` + template.HTMLEscapeString(lang) + `"><code>`)
		b.WriteString(template.HTMLEscapeString(restoreEmbeds(blockLines(src, n), embeds)))
		b.WriteString("</code></pre>")
	case *ast.CodeBlock:
		b.WriteString(`<pre data-code="1" data-lang=""><code>`)
		b.WriteString(template.HTMLEscapeString(restoreEmbeds(blockLines(src, n), embeds)))
		b.WriteString("</code></pre>")
	case *ast.ThematicBreak:
		b.WriteString("<hr>")
	default:
		r.editVerbatim(b, src, n, embeds)
	}
}

// editVerbatim emits a block the editor's schema cannot represent (tables,
// raw HTML, unknown extensions) as its exact source inside a mono block. The
// user can still edit it — as markdown — and it serializes back verbatim.
func (r *Renderer) editVerbatim(b *strings.Builder, src []byte, n ast.Node, embeds []embedRef) {
	lo, hi, ok := nodeSpan(src, n)
	if !ok {
		return
	}
	// Expand to whole lines: cell/inline segments cover inner text only, so
	// without this a table would lose its leading pipes.
	for lo > 0 && src[lo-1] != '\n' {
		lo--
	}
	for hi < len(src) && src[hi] != '\n' {
		hi++
	}
	b.WriteString(`<pre class="md-verbatim" data-verbatim="1">`)
	b.WriteString(template.HTMLEscapeString(restoreEmbeds(string(src[lo:hi]), embeds)))
	b.WriteString("</pre>")
}

// nodeSpan finds the source range a node covers by walking every block line
// segment and inline text segment beneath it.
func nodeSpan(src []byte, n ast.Node) (lo, hi int, ok bool) {
	lo, hi = -1, -1
	grow := func(start, stop int) {
		if lo == -1 || start < lo {
			lo = start
		}
		if stop > hi {
			hi = stop
		}
	}
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if c.Type() == ast.TypeBlock {
			lines := c.Lines()
			for i := 0; i < lines.Len(); i++ {
				seg := lines.At(i)
				grow(seg.Start, seg.Stop)
			}
		}
		if txt, isText := c.(*ast.Text); isText {
			grow(txt.Segment.Start, txt.Segment.Stop)
		}
		return ast.WalkContinue, nil
	})
	return lo, hi, lo != -1
}

// blockLines concatenates a leaf block's source lines (code block content).
func blockLines(src []byte, n ast.Node) string {
	var b strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(src))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (r *Renderer) editInlines(b *strings.Builder, src []byte, parent ast.Node, embeds []embedRef) {
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		r.editInline(b, src, c, embeds)
	}
}

func (r *Renderer) editInline(b *strings.Builder, src []byte, n ast.Node, embeds []embedRef) {
	switch t := n.(type) {
	case *ast.Text:
		b.WriteString(template.HTMLEscapeString(string(t.Segment.Value(src))))
		if t.HardLineBreak() {
			b.WriteString("<br>")
		} else if t.SoftLineBreak() {
			// Under hard-wrap semantics a bare newline IS a line break — it
			// must survive as <br> or a round trip would flatten it.
			if r.HardWraps {
				b.WriteString("<br>")
			} else {
				b.WriteString(" ")
			}
		}
	case *ast.String:
		b.WriteString(template.HTMLEscapeString(string(t.Value)))
	case *ast.Emphasis:
		tag := "em"
		if t.Level == 2 {
			tag = "strong"
		}
		b.WriteString("<" + tag + ">")
		r.editInlines(b, src, n, embeds)
		b.WriteString("</" + tag + ">")
	case *ast.CodeSpan:
		b.WriteString("<code>")
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if txt, isText := c.(*ast.Text); isText {
				b.WriteString(template.HTMLEscapeString(restoreEmbeds(string(txt.Segment.Value(src)), embeds)))
			}
		}
		b.WriteString("</code>")
	case *ast.Link:
		b.WriteString(`<a href="` + safeURL(t.Destination) + `">`)
		r.editInlines(b, src, n, embeds)
		b.WriteString("</a>")
	case *ast.AutoLink:
		url := string(t.URL(src))
		b.WriteString(`<a href="` + safeURL([]byte(url)) + `">` +
			template.HTMLEscapeString(string(t.Label(src))) + "</a>")
	case *ast.Image:
		// Blob images became placeholders in the pre-pass; this is an
		// external image.
		b.WriteString(`<img src="` + safeURL(t.Destination) +
			`" alt="` + template.HTMLEscapeString(nodeText(src, n)) + `">`)
	case *east.Strikethrough:
		b.WriteString("<del>")
		r.editInlines(b, src, n, embeds)
		b.WriteString("</del>")
	case *east.TaskCheckBox:
		if t.IsChecked {
			b.WriteString(`<input type="checkbox" checked>`)
		} else {
			b.WriteString(`<input type="checkbox">`)
		}
	case *ast.RawHTML:
		// Kept as literal source in a verbatim span, so it survives the
		// round trip without ever becoming live markup.
		var s strings.Builder
		for i := 0; i < t.Segments.Len(); i++ {
			seg := t.Segments.At(i)
			s.Write(seg.Value(src))
		}
		b.WriteString(`<code data-verbatim="1">` + template.HTMLEscapeString(s.String()) + "</code>")
	default:
		if n.ChildCount() > 0 {
			r.editInlines(b, src, n, embeds)
		}
	}
}

// nodeText collects the plain text beneath an inline node (image alt).
func nodeText(src []byte, n ast.Node) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if txt, isText := c.(*ast.Text); isText {
				b.Write(txt.Segment.Value(src))
			}
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// safeURL escapes a destination and blanks it if the scheme is dangerous.
//
// The read path renders through goldmark, which suppresses javascript: and
// friends on every link and image. RenderEditable hand-builds its anchors so
// it can round-trip a document, and in doing so escaped the value but kept
// the scheme — meaning the admin editor rendered a live javascript: href that
// the published page would have neutered. Same input, two answers; this makes
// it one, using goldmark's own predicate rather than a second opinion.
func safeURL(dest []byte) string {
	if ghtml.IsDangerousURL(dest) {
		return ""
	}
	return template.HTMLEscapeString(string(dest))
}
