package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// vaultFront is the YAML frontmatter of an Obsidian feed file.
type vaultFront struct {
	Created string   `yaml:"created"`
	Tags    []string `yaml:"tags"`
}

// importVault imports a directory of feed .md files. Each file is one post,
// keyed by its filename stem, so re-running is idempotent. The vault itself
// is never modified.
func importVault(db *sql.DB, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	var imported, failed int
	for _, f := range files {
		p, err := postFromFile(f)
		if err != nil {
			slog.Error("import: skipping file", "file", filepath.Base(f), "err", err)
			failed++
			continue
		}
		if err := importPost(db, p); err != nil {
			slog.Error("import: upsert failed", "slug", p.Slug, "err", err)
			failed++
			continue
		}
		imported++
	}
	slog.Info("import-vault complete", "posts", imported, "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%d file(s) failed to import", failed)
	}
	return nil
}

// postFromFile parses one feed markdown file into a Post.
func postFromFile(file string) (*Post, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	front, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return nil, err
	}
	var fm vaultFront
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return nil, fmt.Errorf("parsing frontmatter: %w", err)
	}
	created := normalizeTime(fm.Created)
	return &Post{
		Slug:      strings.TrimSuffix(filepath.Base(file), ".md"),
		Body:      strings.TrimSpace(body),
		Tags:      fm.Tags,
		CreatedAt: created,
		UpdatedAt: created,
	}, nil
}

// splitFrontmatter separates a leading `---` YAML block from the markdown
// body. A leading `---` line inside the block is tolerated.
func splitFrontmatter(text string) (front, body string, err error) {
	text = strings.TrimPrefix(text, "\ufeff")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		rest, ok = strings.CutPrefix(text, "---\r\n")
	}
	if !ok {
		return "", "", fmt.Errorf("no `---` frontmatter block")
	}
	i := strings.Index(rest, "\n---")
	if i < 0 {
		return "", "", fmt.Errorf("unterminated frontmatter block")
	}
	return rest[:i], strings.TrimLeft(rest[i+4:], "\r\n"), nil
}

// normalizeTime parses a frontmatter timestamp and reformats it as RFC3339
// UTC. Empty or unparseable input yields "".
func normalizeTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{
		time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}
