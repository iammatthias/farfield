package main

import (
	"path/filepath"
	"testing"
)

func TestValidDateAndRange(t *testing.T) {
	if !validDate("2024-01-02") {
		t.Error("2024-01-02 should be valid")
	}
	if validDate("2024-13-40") || validDate("not-a-date") {
		t.Error("malformed dates should be rejected")
	}
	if nasaDateInRange("2025-12-31") {
		t.Error("a pre-Farfield-calendar date should be out of range")
	}
	if !nasaDateInRange(calendarStart) {
		t.Error("the Farfield calendar start should be in range")
	}
}

func TestDaysBetween(t *testing.T) {
	if got := daysBetween("2024-01-01", "2024-01-01"); got != 1 {
		t.Errorf("single day = %d, want 1", got)
	}
	if got := daysBetween("2024-01-01", "2024-01-14"); got != 14 {
		t.Errorf("two weeks = %d, want 14", got)
	}
	if got := daysBetween("2024-01-14", "2024-01-01"); got != 0 {
		t.Errorf("reversed range = %d, want 0", got)
	}
}

func TestPageCount(t *testing.T) {
	if pageCount(0) != 1 {
		t.Error("an empty archive should still report one page")
	}
	if pageCount(pageSize) != 1 {
		t.Errorf("exactly one page = %d, want 1", pageCount(pageSize))
	}
	if pageCount(pageSize+1) != 2 {
		t.Errorf("one item over = %d, want 2", pageCount(pageSize+1))
	}
}

func TestMediaKind(t *testing.T) {
	cases := map[string]string{
		"https://apod.nasa.gov/a.jpg":     "image",
		"https://apod.nasa.gov/a.PNG?x=1": "image",
		"https://x/clip.mp4":              "video",
		"https://x/clip.webm":             "video",
		"https://youtube.com/embed/abc":   "",
		"https://x/page.html":             "",
	}
	for u, want := range cases {
		if got := mediaKind(u); got != want {
			t.Errorf("mediaKind(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestNASAArchivePaginatesCachedPhotosOnly(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "cal.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Simulate a partially warmed cache: recent APOD records exist, but the
	// full Jan 1 -> today archive has not been backfilled. The archive must
	// not advertise empty pages for theoretical calendar days.
	for _, d := range []string{"2026-05-06", "2026-05-07", "2026-05-08"} {
		if err := upsertPhoto(db, &Photo{
			Source: sourceNASA, Date: d, Title: "day " + d,
			ImageURL: "https://x/" + d + ".jpg", MediaType: "image",
		}); err != nil {
			t.Fatalf("upsert %s: %v", d, err)
		}
	}

	s := &Server{db: db, fetcher: newFetcher("DEMO_KEY")}
	s.fetcher.noteNASAError() // keep the test offline and cache-only

	res, err := s.archive(1)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if res.Total != 3 || res.Pages != 1 || res.HasNext {
		t.Fatalf("archive metadata = total %d pages %d hasNext %v, want total 3 pages 1 no next",
			res.Total, res.Pages, res.HasNext)
	}
	if len(res.Photos) != 3 || res.Photos[0].Date != "2026-05-08" || res.Photos[2].Date != "2026-05-06" {
		t.Fatalf("archive photos = %+v, want cached photos newest-first", res.Photos)
	}
}

func TestParseAPOD(t *testing.T) {
	one, err := parseAPOD([]byte(`{"date":"2024-01-01","title":"T","media_type":"image","url":"u"}`))
	if err != nil || len(one) != 1 || one[0].Title != "T" {
		t.Fatalf("single object: got %v, err %v", one, err)
	}
	arr, err := parseAPOD([]byte(`[{"date":"2024-01-01"},{"date":"2024-01-02"}]`))
	if err != nil || len(arr) != 2 {
		t.Fatalf("array: got %v, err %v", arr, err)
	}
	if _, err := parseAPOD([]byte(`{"code":429,"msg":"rate limited"}`)); err == nil {
		t.Error("an APOD error envelope should surface as an error")
	}
}

func TestApodToPhoto(t *testing.T) {
	p := apodToPhoto(apodResponse{
		Date: "2024-03-04", Title: " Pillars ", MediaType: "image",
		URL: "https://apod/small.jpg", HDURL: "https://apod/big.jpg",
		Copyright: " Someone ",
	})
	if p.Source != sourceNASA {
		t.Errorf("source = %q, want %q", p.Source, sourceNASA)
	}
	if p.ImageURL != "https://apod/big.jpg" {
		t.Errorf("image should prefer hdurl, got %q", p.ImageURL)
	}
	if p.ThumbURL != "https://apod/small.jpg" {
		t.Errorf("thumb should fall back to url, got %q", p.ThumbURL)
	}
	if p.Title != "Pillars" || p.Credit != "Someone" {
		t.Errorf("fields not trimmed: title %q, credit %q", p.Title, p.Credit)
	}
}

func TestPhotoCID(t *testing.T) {
	a := Photo{Title: "A", ImageURL: "u"}
	b := Photo{Title: "A", ImageURL: "u"}
	if photoCID(&a) != photoCID(&b) {
		t.Error("identical content should yield an identical CID")
	}
	b.Title = "B"
	if photoCID(&a) == photoCID(&b) {
		t.Error("changed content should change the CID")
	}
	// the key, fetch time, and placeholder flag are excluded from the CID
	c := a
	c.Date, c.Source = "2024-01-01", sourceNASA
	c.FetchedAt, c.Placeholder = nowRFC3339(), true
	if photoCID(&a) != photoCID(&c) {
		t.Error("non-content fields must not affect the CID")
	}
}

// TestDBRoundTrip exercises the self-migrating schema and the query helpers
// against a fresh on-disk database.
func TestDBRoundTrip(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "cal.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	for _, d := range []string{"2024-01-01", "2024-01-02", "2024-01-03"} {
		if err := upsertPhoto(db, &Photo{
			Source: sourceNASA, Date: d, Title: "day " + d,
			ImageURL: "https://x/" + d + ".jpg", MediaType: "image",
		}); err != nil {
			t.Fatalf("upsert %s: %v", d, err)
		}
	}

	got, err := getPhoto(db, sourceNASA, "2024-01-02")
	if err != nil || got == nil {
		t.Fatalf("getPhoto: got %v, err %v", got, err)
	}
	if got.CID == "" {
		t.Error("upsert should stamp a content CID")
	}

	if miss, err := getPhoto(db, sourceNASA, "2099-01-01"); err != nil || miss != nil {
		t.Errorf("absent photo should be (nil, nil), got %v, err %v", miss, err)
	}

	if n, err := countPhotos(db, sourceNASA); err != nil || n != 3 {
		t.Fatalf("countPhotos = %d, err %v, want 3", n, err)
	}

	page, err := listPhotos(db, sourceNASA, 2, 0)
	if err != nil || len(page) != 2 {
		t.Fatalf("listPhotos = %d rows, err %v, want 2", len(page), err)
	}
	if page[0].Date != "2024-01-03" {
		t.Errorf("expected newest-first ordering, got %q first", page[0].Date)
	}

	latest, err := latestPhoto(db, sourceNASA)
	if err != nil || latest == nil || latest.Date != "2024-01-03" {
		t.Fatalf("latestPhoto = %v, err %v", latest, err)
	}

	if prev, err := neighborDate(db, sourceNASA, "2024-01-02", true); err != nil || prev != "2024-01-01" {
		t.Errorf("prev neighbor = %q, err %v, want 2024-01-01", prev, err)
	}
	if next, err := neighborDate(db, sourceNASA, "2024-01-02", false); err != nil || next != "2024-01-03" {
		t.Errorf("next neighbor = %q, err %v, want 2024-01-03", next, err)
	}

	between, err := listPhotosBetween(db, sourceNASA, "2024-01-01", "2024-01-02")
	if err != nil || len(between) != 2 {
		t.Fatalf("listPhotosBetween = %d rows, err %v, want 2", len(between), err)
	}
}
