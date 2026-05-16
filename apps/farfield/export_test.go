package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWriteMarkdownRoundTrips checks that an exported file parses back to the
// same frontmatter + body — export must be re-importable.
func TestWriteMarkdownRoundTrips(t *testing.T) {
	value := map[string]any{
		"title":     "Hello",
		"slug":      "1700000000000-hello",
		"published": true,
		"tags":      []any{"alpha", "beta"},
		"created":   "2024-03-07T12:58:00Z",
		"body":      "# Hello\n\n![](series://bafkreigallery)\n",
	}
	path := filepath.Join(t.TempDir(), "hello.md")
	if err := writeMarkdown(path, value); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	front, body, err := splitFrontmatter(string(raw))
	if err != nil {
		t.Fatalf("frontmatter did not parse: %v", err)
	}
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		t.Fatalf("yaml: %v", err)
	}

	if fm["title"] != "Hello" || fm["slug"] != "1700000000000-hello" {
		t.Fatalf("frontmatter mismatch: %v", fm)
	}
	if fm["published"] != true {
		t.Fatalf("published round-trip: %v", fm["published"])
	}
	if _, leaked := fm["body"]; leaked {
		t.Fatal("body leaked into frontmatter")
	}
	if !strings.Contains(body, "![](series://bafkreigallery)") {
		t.Fatalf("body not preserved: %q", body)
	}
}
