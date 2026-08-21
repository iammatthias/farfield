package main

import (
	"path/filepath"
	"testing"
)

// TestPaginationKeepsPostsSharingATimestamp pins the bug that made the cursor
// compound. Timestamps are second-granular, so posts published in the same
// second collide; with a bare `created_at < ?` cursor, a page boundary landing
// between two of them dropped the older one from every page forever.
func TestPaginationKeepsPostsSharingATimestamp(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "feed.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Six posts, all sharing one timestamp — the worst case, and the one a
	// vault sync or a scripted import actually produces.
	const ts = "2026-08-21T12:00:00Z"
	want := map[string]bool{}
	for _, slug := range []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff"} {
		if _, err := db.Exec(
			`INSERT INTO posts (slug, cid, body, tags, created_at, updated_at)
			 VALUES (?, '', ?, '[]', ?, ?)`, slug, "body-"+slug, ts, ts); err != nil {
			t.Fatal(err)
		}
		want[slug] = true
	}

	// Page through two at a time, exactly as the index handler does.
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		got, err := listPosts(db, 3, cursor) // pageSize 2 + 1 lookahead
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			break
		}
		if len(got) > 2 {
			got = got[:2]
			cursor = makeCursor(&got[len(got)-1])
		} else {
			cursor = ""
		}
		for _, p := range got {
			if seen[p.Slug] {
				t.Errorf("post %q served twice — the cursor overlaps", p.Slug)
			}
			seen[p.Slug] = true
		}
		if cursor == "" {
			break
		}
	}

	for slug := range want {
		if !seen[slug] {
			t.Errorf("post %q was never returned by any page — it is unreachable by paging", slug)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("paged %d posts, want %d", len(seen), len(want))
	}
}

// TestBareTimestampCursorStillWorks — old links and clients send just a
// timestamp; they must keep working, with the previous semantics.
func TestBareTimestampCursorStillWorks(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "feed.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for slug, ts := range map[string]string{
		"old": "2026-08-01T00:00:00Z",
		"new": "2026-08-20T00:00:00Z",
	} {
		if _, err := db.Exec(
			`INSERT INTO posts (slug, cid, body, tags, created_at, updated_at)
			 VALUES (?, '', 'x', '[]', ?, ?)`, slug, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listPosts(db, 0, "2026-08-10T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Slug != "old" {
		t.Errorf("bare-timestamp cursor returned %d posts (%v), want just the older one", len(got), got)
	}
}
