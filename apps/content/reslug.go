package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// tsPrefix matches the leading "<timestamp>-" of an entry slug.
var tsPrefix = regexp.MustCompile(`^[0-9]+-`)

// reslugEntries stamps every entry whose slug predates the timestamp-prefix
// convention — the handful published through the app before insertEntry began
// stamping — rewriting each to "<createdMillis>-<slug>" so app-authored
// entries match the migrated corpus. Any "(<oldSlug>.md)" cross-link in another
// entry's body is rewritten to match. updated_at is left untouched: like
// reslugSeries, this is a mechanical key fix, not a content edit. All in one
// transaction.
func reslugEntries(db *sql.DB) error {
	entries, err := listEntries(db, "", false)
	if err != nil {
		return err
	}

	rename := map[string]string{} // old slug -> new slug
	for _, e := range entries {
		if stampedSlug.MatchString(e.Slug) {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.CreatedAt)
		if err != nil {
			slog.Error("reslug-entries: unparseable created_at, skipping",
				"slug", e.Slug, "created_at", e.CreatedAt, "err", err)
			continue
		}
		rename[e.Slug] = fmt.Sprintf("%d-%s", t.UnixMilli(), e.Slug)
	}
	if len(rename) == 0 {
		slog.Info("reslug-entries: nothing to do")
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
			body = strings.ReplaceAll(body, "("+old+".md)", "("+nw+".md)")
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
		if _, err := tx.Exec(`UPDATE entries SET slug = ? WHERE slug = ?`, nw, old); err != nil {
			return err
		}
		slog.Info("reslug-entries: renamed", "from", old, "to", nw)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("reslug-entries complete", "entries", len(rename), "bodies", touched)
	return nil
}

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
