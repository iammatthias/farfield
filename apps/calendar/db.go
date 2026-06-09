package main

import (
	"database/sql"
	"errors"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// dateLayout is the calendar key format — every photo is keyed by a day.
const dateLayout = "2006-01-02"

// Photo is one day's image, from one source. (source, date) is the stable
// key; cid is the content hash — it changes whenever the content does. NASA
// dates are real calendar days; UFO dates are synthetic ordering keys assigned
// at scrape time (see ufo.go).
type Photo struct {
	Source      string `json:"source"`
	Date        string `json:"date"`
	CID         string `json:"cid"`
	Title       string `json:"title"`
	Explanation string `json:"explanation"`
	ImageURL    string `json:"imageUrl"`
	ThumbURL    string `json:"thumbUrl"`
	MediaType   string `json:"mediaType"` // "image" | "video" | "other"
	Credit      string `json:"credit"`
	SourceURL   string `json:"sourceUrl"`
	Placeholder bool   `json:"placeholder"` // true when upstream was unavailable
	FetchedAt   string `json:"fetchedAt"`
}

const schema = `
CREATE TABLE IF NOT EXISTS photos (
	source      TEXT NOT NULL,
	date        TEXT NOT NULL,
	cid         TEXT NOT NULL DEFAULT '',
	title       TEXT NOT NULL DEFAULT '',
	explanation TEXT NOT NULL DEFAULT '',
	image_url   TEXT NOT NULL DEFAULT '',
	thumb_url   TEXT NOT NULL DEFAULT '',
	media_type  TEXT NOT NULL DEFAULT 'image',
	credit      TEXT NOT NULL DEFAULT '',
	source_url  TEXT NOT NULL DEFAULT '',
	placeholder INTEGER NOT NULL DEFAULT 0,
	fetched_at  TEXT NOT NULL,
	PRIMARY KEY (source, date)
);
CREATE INDEX IF NOT EXISTS photos_by_date ON photos (source, date DESC);`

// photoCols is the column list, in Photo-field order, shared by every query.
const photoCols = `source, date, cid, title, explanation, image_url, thumb_url, ` +
	`media_type, credit, source_url, placeholder, fetched_at`

// openDB opens the SQLite database, applies pragmas, and migrates the schema.
// The migration sequence is idempotent — it brings any database, fresh or old,
// to the current schema on every startup. See the self-migrating-sqlite skill.
func openDB(path string) (*sql.DB, error) {
	db, err := store.OpenDB(path)
	if err != nil {
		return nil, err
	}
	// 1. Current schema — builds fresh databases, no-ops on existing ones.
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// 2. Columns added after the first release — CREATE TABLE IF NOT EXISTS
	//    will not add these to a table that already exists.
	for _, c := range []struct{ col, decl string }{
		{"cid", "TEXT NOT NULL DEFAULT ''"},
		{"thumb_url", "TEXT NOT NULL DEFAULT ''"},
		{"media_type", "TEXT NOT NULL DEFAULT 'image'"},
		{"credit", "TEXT NOT NULL DEFAULT ''"},
		{"source_url", "TEXT NOT NULL DEFAULT ''"},
		{"placeholder", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := store.EnsureColumn(db, "photos", c.col, c.decl); err != nil {
			return nil, err
		}
	}
	// 3. Backfill content CIDs for any rows that predate the column.
	if err := backfillCIDs(db); err != nil {
		return nil, err
	}
	// 4. The public calendar starts on Jan 1 2026. If a development DB was warmed
	// from NASA's full APOD history, drop those old rows so navigation, counts,
	// and archive pages only move forward from calendarStart.
	if err := pruneBeforeCalendarStart(db); err != nil {
		return nil, err
	}
	return db, nil
}

func pruneBeforeCalendarStart(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM photos WHERE source = ? AND date < ?`, sourceNASA, calendarStart)
	return err
}

// photoCID is the content identifier of a photo — a CIDv1 over its content.
// The key (source, date), fetch time, and placeholder flag are excluded so the
// CID tracks what the photo *is*, not when or how it was retrieved.
func photoCID(p *Photo) string {
	return cid.OfValue(map[string]any{
		"title":       p.Title,
		"explanation": p.Explanation,
		"imageUrl":    p.ImageURL,
		"thumbUrl":    p.ThumbURL,
		"mediaType":   p.MediaType,
		"credit":      p.Credit,
		"sourceUrl":   p.SourceURL,
	})
}

// backfillCIDs computes the content CID for any photo that lacks one — a
// one-time migration for databases created before the cid column existed.
func backfillCIDs(db *sql.DB) error {
	rows, err := db.Query(`SELECT ` + photoCols + ` FROM photos WHERE cid = ''`)
	if err != nil {
		return err
	}
	var photos []Photo
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			rows.Close()
			return err
		}
		photos = append(photos, *p)
	}
	rows.Close()
	for i := range photos {
		if _, err := db.Exec(`UPDATE photos SET cid = ? WHERE source = ? AND date = ?`,
			photoCID(&photos[i]), photos[i].Source, photos[i].Date); err != nil {
			return err
		}
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanPhoto(row scanner) (*Photo, error) {
	var p Photo
	var placeholder int
	if err := row.Scan(&p.Source, &p.Date, &p.CID, &p.Title, &p.Explanation,
		&p.ImageURL, &p.ThumbURL, &p.MediaType, &p.Credit, &p.SourceURL,
		&placeholder, &p.FetchedAt); err != nil {
		return nil, err
	}
	p.Placeholder = placeholder != 0
	return &p, nil
}

// getPhoto returns one photo by (source, date), or (nil, nil) if absent.
func getPhoto(db *sql.DB, source, date string) (*Photo, error) {
	p, err := scanPhoto(db.QueryRow(
		`SELECT `+photoCols+` FROM photos WHERE source = ? AND date = ?`,
		source, date))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// upsertPhoto inserts or replaces a photo, keyed by (source, date). It stamps
// the content CID and a fetch time, so every write keeps both current.
func upsertPhoto(db *sql.DB, p *Photo) error {
	if p.MediaType == "" {
		p.MediaType = "image"
	}
	if p.FetchedAt == "" {
		p.FetchedAt = store.NowRFC3339()
	}
	p.CID = photoCID(p)
	placeholder := 0
	if p.Placeholder {
		placeholder = 1
	}
	_, err := db.Exec(
		`INSERT INTO photos (`+photoCols+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(source, date) DO UPDATE SET
		   cid=excluded.cid, title=excluded.title, explanation=excluded.explanation,
		   image_url=excluded.image_url, thumb_url=excluded.thumb_url,
		   media_type=excluded.media_type, credit=excluded.credit,
		   source_url=excluded.source_url, placeholder=excluded.placeholder,
		   fetched_at=excluded.fetched_at`,
		p.Source, p.Date, p.CID, p.Title, p.Explanation, p.ImageURL, p.ThumbURL,
		p.MediaType, p.Credit, p.SourceURL, placeholder, p.FetchedAt)
	return err
}

// listPhotos returns a page of photos for a source, newest day first.
func listPhotos(db *sql.DB, source string, limit, offset int) ([]Photo, error) {
	rows, err := db.Query(
		`SELECT `+photoCols+` FROM photos WHERE source = ?
		 ORDER BY date DESC LIMIT ? OFFSET ?`, source, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectPhotos(rows)
}

// listPhotosBetween returns every cached photo for a source within the
// inclusive [start, end] date range, newest day first.
func listPhotosBetween(db *sql.DB, source, start, end string) ([]Photo, error) {
	rows, err := db.Query(
		`SELECT `+photoCols+` FROM photos
		 WHERE source = ? AND date BETWEEN ? AND ?
		 ORDER BY date DESC`, source, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectPhotos(rows)
}

func collectPhotos(rows *sql.Rows) ([]Photo, error) {
	out := []Photo{}
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// countPhotos returns the number of cached photos for a source.
func countPhotos(db *sql.DB, source string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM photos WHERE source = ?`, source).Scan(&n)
	return n, err
}

// countPhotosBetween returns how many photos a source has cached within the
// inclusive [start, end] date range.
func countPhotosBetween(db *sql.DB, source, start, end string) (int, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM photos WHERE source = ? AND date BETWEEN ? AND ?`,
		source, start, end).Scan(&n)
	return n, err
}

// latestPhoto returns the newest cached photo for a source, or (nil, nil).
func latestPhoto(db *sql.DB, source string) (*Photo, error) {
	p, err := scanPhoto(db.QueryRow(
		`SELECT `+photoCols+` FROM photos WHERE source = ?
		 ORDER BY date DESC LIMIT 1`, source))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// neighborDate returns the date of the cached photo immediately before (prev)
// or after a given date for a source, or "" when there is none.
func neighborDate(db *sql.DB, source, date string, prev bool) (string, error) {
	q := `SELECT date FROM photos WHERE source = ? AND date > ? ORDER BY date ASC LIMIT 1`
	if prev {
		q = `SELECT date FROM photos WHERE source = ? AND date < ? ORDER BY date DESC LIMIT 1`
	}
	var d string
	err := db.QueryRow(q, source, date).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return d, err
}
