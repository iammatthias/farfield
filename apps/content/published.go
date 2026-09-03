package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
)

// The publish date. published_at is when an entry first went public:
// stamped the first time published flips true, sticky after that
// (unpublishing and republishing never moves it), and outside the CID —
// it is metadata about the content, not the content, and a consumer that
// caches rendered HTML by CID must not re-render every entry because a
// date was filled in. The list and single-entry ETags cover it instead.

// stampPublishedAt decides the published_at a save stores. requested is the
// caller's value (the API's hand-correction path; an omitted field arrives
// as ""), and wins when it is a valid RFC 3339 instant no later than now.
// Otherwise an existing stamp is kept, an entry that is published without
// one is stamped now, and a never-published draft stays blank.
func stampPublishedAt(requested, existing string, published bool, now time.Time) string {
	if t, err := time.Parse(time.RFC3339, requested); err == nil && !t.After(now) {
		return t.UTC().Format(time.RFC3339)
	}
	if existing != "" {
		return existing
	}
	if published {
		return now.UTC().Format(time.RFC3339)
	}
	return ""
}

// entryETag is the validator for a single-entry read. The CID alone would
// miss published_at, which lives outside it by design, so the tag hashes
// both: a backfilled or hand-corrected date invalidates a cached copy,
// while a save that changes neither content nor date still revalidates.
func entryETag(e *Entry) string {
	return cid.Of([]byte(e.CID + "|" + e.PublishedAt))
}

// publishedAtCutoff is the creation instant of the first entry authored in
// the app rather than imported. Everything created before it came through
// migrate.go from the old site, and its created_at IS its original
// publication date. Everything from it on was born here — as a draft or
// not — and only revision history knows when it went public.
const publishedAtCutoff = "2026-05-18T01:06:15Z"

// backfillPublishedAt gives every published entry that predates the
// published_at column a publish date. It only ever looks at rows with
// published = 1 and no stamp, so it is idempotent and free on every later
// start; trashed rows are included, so a restore comes back dated.
//
// An imported entry (created before publishedAtCutoff) is dated by its
// created_at. An app-born entry is reconstructed from its revisions — see
// reconstructPublishedAt. Anything but an exact reconstruction is logged
// as assumed, so the slugs can be hand-corrected through the API.
func backfillPublishedAt(db *sql.DB) error {
	rows, err := db.Query(`SELECT e.id, e.slug, c.slug, e.excerpt, e.tags, e.created_at
		FROM entries e JOIN collections c ON c.id = e.collection_id
		WHERE e.published = 1 AND e.published_at = '' ORDER BY e.created_at`)
	if err != nil {
		return err
	}
	type pending struct {
		id        int64
		createdAt string
		e         Entry // slug, collection, excerpt, tags — the CID inputs a revision does not store
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var tags string
		if err := rows.Scan(&p.id, &p.e.Slug, &p.e.Collection, &p.e.Excerpt, &tags, &p.createdAt); err != nil {
			rows.Close()
			return err
		}
		p.e.Tags = decodeTags(tags)
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(todo) == 0 {
		return nil
	}

	assumed := 0
	for _, p := range todo {
		at, how, exact := p.createdAt, "imported", true
		if p.createdAt >= publishedAtCutoff {
			revs, err := revisionsOldestFirst(db, p.id)
			if err != nil {
				return err
			}
			at, how, exact = reconstructPublishedAt(&p.e, p.createdAt, revs)
		}
		if _, err := db.Exec(`UPDATE entries SET published_at = ? WHERE id = ?`, at, p.id); err != nil {
			return err
		}
		switch {
		case !exact:
			assumed++
			slog.Warn("published_at backfill: assumed", "slug", p.e.Slug, "published_at", at, "how", how)
		case how != "imported":
			slog.Info("published_at backfill", "slug", p.e.Slug, "published_at", at, "how", how)
		}
	}
	slog.Info("published_at backfill complete", "entries", len(todo), "assumed", assumed)
	return nil
}

// revisionsOldestFirst is an entry's whole revision log in save order.
func revisionsOldestFirst(db *sql.DB, entryID int64) ([]Revision, error) {
	rows, err := db.Query(
		`SELECT id, entry_id, cid, title, body, saved_at
		   FROM entry_revisions WHERE entry_id = ? ORDER BY id ASC`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var r Revision
		if err := rows.Scan(&r.ID, &r.EntryID, &r.CID, &r.Title, &r.Body, &r.SavedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// revisionsSince is the first full day revision history was live. An entry
// created before it has no birth revision — its log begins at its first
// edit after that day — so nothing in the log can say whether it was
// already public when the log begins: a draft save there may be an
// unpublish, not the state it was born in. Only created_at is left.
const revisionsSince = "2026-07-16T00:00:00Z"

// revisionStartsAtCreation is the tolerance for calling an entry's oldest
// revision its birth: insertEntry snapshots right after the row lands, so
// a log that begins later than this was either pruned or started after
// the entry existed (the vault sync also creates entries with an earlier,
// authored created_at, which reads the same way — and is as uncertain).
const revisionStartsAtCreation = time.Minute

// reconstructPublishedAt dates an app-born entry from its revision log,
// oldest first. The CID input includes the published flag, so re-hashing a
// revision's title and body with published = true — against the entry's
// current collection, excerpt, and tags, which revisions do not store —
// tells whether that save was a published state. Walking forward:
//
//   - a draft save followed by a published save dates the entry at the
//     published save: the publish moment, to the second;
//   - a log that is published from its earliest surviving save dates the
//     entry at created_at — it went public no later than that, and
//     nothing older is known;
//   - no published save at all (history lost) falls back to created_at.
//
// It returns the date, a short account of how it was reached, and whether
// that account is exact rather than assumed. Exact needs the log to speak
// for the entry's whole life: a log that predates revisionsSince, a first
// save that is not the birth, keepRevisions pruning ahead of a published
// first save, and revisions whose excerpt or tags have since changed
// (which match neither hash) all blur the answer.
func reconstructPublishedAt(e *Entry, createdAt string, revs []Revision) (at, how string, exact bool) {
	if len(revs) == 0 {
		return createdAt, "no revision history; created_at", false
	}
	if createdAt < revisionsSince {
		return createdAt, "predates revision history; created_at", false
	}
	firstPub, lastDraft := -1, -1
	for i, rv := range revs {
		probe := Entry{Collection: e.Collection, Excerpt: e.Excerpt, Tags: e.Tags,
			Title: rv.Title, Body: rv.Body, Published: true}
		if entryCID(&probe) == rv.CID {
			firstPub = i
			break
		}
		probe.Published = false
		if entryCID(&probe) == rv.CID {
			lastDraft = i
		}
	}
	switch {
	case firstPub < 0:
		return createdAt, "no published revision matches; created_at", false
	case lastDraft >= 0:
		at = revs[firstPub].SavedAt
		if gap := firstPub - lastDraft - 1; gap > 0 {
			return at, fmt.Sprintf("first published save, %d unclassifiable saves after the last draft", gap), false
		}
		return at, "first published save after a draft", true
	case firstPub > 0:
		return createdAt, "published at earliest classifiable save; created_at", false
	case len(revs) >= keepRevisions:
		return createdAt, "published at earliest surviving save, history pruned; created_at", false
	}
	// The oldest save is the birth only if it happened at creation.
	born, err1 := time.Parse(time.RFC3339, createdAt)
	first, err2 := time.Parse(time.RFC3339, revs[0].SavedAt)
	if err1 != nil || err2 != nil || first.Sub(born) > revisionStartsAtCreation {
		return createdAt, "published at earliest save, which postdates creation; created_at", false
	}
	return createdAt, "born published", true
}
