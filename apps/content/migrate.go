package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// importSeries reads the series records from an old records-engine content
// database and writes them as markdown fragments — each ref becomes a
// blob://<cid> image line. Keyed by rkey, so re-running is idempotent.
func importSeries(db *sql.DB, oldDBPath string) error {
	old, err := sql.Open("sqlite", "file:"+oldDBPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer old.Close()

	rows, err := old.Query(`SELECT rkey, value FROM records WHERE collection = 'series'`)
	if err != nil {
		return fmt.Errorf("reading old series records: %w", err)
	}
	defer rows.Close()

	var imported, failed int
	for rows.Next() {
		var rkey, value string
		if err := rows.Scan(&rkey, &value); err != nil {
			return err
		}
		var old struct {
			Created     string   `json:"created"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Refs        []string `json:"refs"`
		}
		if err := json.Unmarshal([]byte(value), &old); err != nil {
			slog.Error("import-series: unparseable record", "rkey", rkey, "err", err)
			failed++
			continue
		}
		var b strings.Builder
		if old.Description != "" {
			b.WriteString(old.Description)
			b.WriteString("\n\n")
		}
		for _, ref := range old.Refs {
			b.WriteString("![](blob://")
			b.WriteString(ref)
			b.WriteString(")\n\n")
		}
		created := normalizeTime(old.Created)
		if created == "" {
			created = nowRFC3339()
		}
		s := &Series{
			Rkey:      rkey,
			Title:     old.Title,
			Body:      strings.TrimSpace(b.String()),
			CreatedAt: created,
			UpdatedAt: created,
		}
		if err := upsertSeries(db, s); err != nil {
			slog.Error("import-series: upsert failed", "rkey", rkey, "err", err)
			failed++
			continue
		}
		imported++
	}
	slog.Info("import-series complete", "imported", imported, "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%d series failed to import", failed)
	}
	return rows.Err()
}

// vaultFront is the YAML frontmatter of an Obsidian content file.
type vaultFront struct {
	Title     string   `yaml:"title"`
	Slug      string   `yaml:"slug"`
	Excerpt   string   `yaml:"excerpt"`
	Tags      []string `yaml:"tags"`
	Published bool     `yaml:"published"`
	Created   string   `yaml:"created"`
	Updated   string   `yaml:"updated"`
}

// importVault imports an Obsidian content directory: each subfolder is a
// collection, each .md file an entry with YAML frontmatter. Entries are keyed
// by slug, so re-running is idempotent. The vault itself is never modified.
func importVault(db *sql.DB, dir string) error {
	dirs, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", dir, err)
	}
	var collections, imported, failed int
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		collections++
		coll, err := getOrCreateCollection(db, slugify(d.Name()), prettifyName(d.Name()))
		if err != nil {
			return fmt.Errorf("collection %s: %w", d.Name(), err)
		}
		files, _ := filepath.Glob(filepath.Join(dir, d.Name(), "*.md"))
		sort.Strings(files)
		for _, f := range files {
			e, err := entryFromFile(f, coll.Slug)
			if err != nil {
				slog.Error("import: skipping file", "file", filepath.Base(f), "err", err)
				failed++
				continue
			}
			if err := importEntry(db, e); err != nil {
				slog.Error("import: upsert failed", "slug", e.Slug, "err", err)
				failed++
				continue
			}
			imported++
		}
		slog.Info("imported collection", "collection", coll.Slug, "entries", len(files))
	}
	slog.Info("import-vault complete",
		"collections", collections, "entries", imported, "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%d file(s) failed to import", failed)
	}
	return nil
}

// entryFromFile parses one vault markdown file into an Entry.
func entryFromFile(file, collection string) (*Entry, error) {
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

	slug := slugify(firstNonEmpty(fm.Slug, strings.TrimSuffix(filepath.Base(file), ".md")))
	if slug == "" {
		return nil, fmt.Errorf("no usable slug")
	}
	created := normalizeTime(fm.Created)
	updated := normalizeTime(fm.Updated)
	if updated == "" {
		updated = created
	}
	return &Entry{
		Collection: collection,
		Slug:       slug,
		Title:      firstNonEmpty(fm.Title, slug),
		Excerpt:    strings.TrimSpace(fm.Excerpt),
		Body:       strings.TrimSpace(body),
		Tags:       fm.Tags,
		Published:  fm.Published,
		CreatedAt:  created,
		UpdatedAt:  updated,
	}, nil
}

// splitFrontmatter separates a leading `---` YAML block from the markdown
// body. A leading `---` line inside the block (a YAML document marker, as
// Obsidian sometimes writes) is tolerated.
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

// prettifyName turns a folder name into a display name: "open-source" becomes
// "Open Source".
func prettifyName(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
