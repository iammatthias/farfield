package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// tsPrefix matches the leading "<timestamp>-" of an entry slug.
var tsPrefix = regexp.MustCompile(`^[0-9]+-`)

// reslugSeries replaces the stale CID-shaped slugs that series carried over
// from the old records engine with readable slugs — derived from each series'
// title, or for an untitled series from the lone entry that embeds it — and
// rewrites every series:// reference in entry bodies to match. All in one
// transaction. updated_at is left untouched: the rendered output is
// unchanged, this is a mechanical key fix.
func reslugSeries(db *sql.DB) error {
	series, err := listSeries(db)
	if err != nil {
		return err
	}
	entries, err := listEntries(db, "", false)
	if err != nil {
		return err
	}

	taken := map[string]bool{}
	for _, s := range series {
		taken[s.Slug] = true
	}
	rename := map[string]string{} // old slug -> new slug
	for _, s := range series {
		cand := slugify(s.Title)
		if cand == "" {
			// Untitled — borrow the name of the single entry embedding it.
			var refs []string
			for _, e := range entries {
				if strings.Contains(e.Body, "series://"+s.Slug) {
					refs = append(refs, e.Slug)
				}
			}
			if len(refs) == 1 {
				cand = slugify(tsPrefix.ReplaceAllString(refs[0], ""))
			}
		}
		if cand == "" || cand == s.Slug {
			continue
		}
		newSlug := cand
		for i := 2; taken[newSlug]; i++ {
			newSlug = fmt.Sprintf("%s-%d", cand, i)
		}
		delete(taken, s.Slug)
		taken[newSlug] = true
		rename[s.Slug] = newSlug
	}
	if len(rename) == 0 {
		slog.Info("reslug-series: nothing to do")
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	touched := 0
	for i := range entries {
		e := entries[i]
		body := e.Body
		for old, nw := range rename {
			body = strings.ReplaceAll(body, "series://"+old, "series://"+nw)
		}
		if body == e.Body {
			continue
		}
		e.Body = body
		if _, err := tx.Exec(`UPDATE entries SET body = ?, cid = ? WHERE slug = ?`,
			body, entryCID(&e), e.Slug); err != nil {
			return err
		}
		touched++
	}
	for old, nw := range rename {
		if _, err := tx.Exec(`UPDATE series SET slug = ? WHERE slug = ?`, nw, old); err != nil {
			return err
		}
		slog.Info("reslug-series: renamed", "from", old, "to", nw)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("reslug-series complete", "series", len(rename), "entries", touched)
	return nil
}
