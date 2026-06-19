package main

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
)

// md renders markdown to HTML. Raw HTML in the source is omitted (goldmark's
// safe default), so descriptions and changelogs can carry formatting without
// opening an injection surface on the public share page.
var md = goldmark.New()

// renderMarkdown converts markdown source to safe HTML for templates. Empty
// input yields empty output; a parse failure falls back to escaped plain text.
func renderMarkdown(src string) template.HTML {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String())
}
