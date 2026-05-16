package main

import (
	"strings"
	"testing"
)

const ts = "2024-03-07T12:58:00Z"

// TestExtractSeriesRun: a run of 5 adjacent embeds becomes one series, and the
// run is replaced in the body by a single series:// marker.
func TestExtractSeriesRun(t *testing.T) {
	body := strings.Join([]string{
		"Intro paragraph.",
		"",
		"![](blob://baf1)",
		"![](blob://baf2)",
		"![](blob://baf3)",
		"![](blob://baf4)",
		"![](blob://baf5)",
		"",
		"Outro paragraph.",
	}, "\n")

	newBody, recs := extractSeries(body, 3, ts)
	if len(recs) != 1 {
		t.Fatalf("want 1 series, got %d", len(recs))
	}
	want := []string{"baf1", "baf2", "baf3", "baf4", "baf5"}
	if strings.Join(recs[0].refs, ",") != strings.Join(want, ",") {
		t.Fatalf("refs = %v, want %v", recs[0].refs, want)
	}
	if recs[0].record["created"] != ts {
		t.Fatalf("created = %v, want %s", recs[0].record["created"], ts)
	}
	marker := "![](series://" + recs[0].rkey + ")"
	if !strings.Contains(newBody, marker) {
		t.Fatalf("body missing series marker %q:\n%s", marker, newBody)
	}
	if strings.Contains(newBody, "blob://baf1") {
		t.Fatalf("extracted embed still inline:\n%s", newBody)
	}
	if !strings.Contains(newBody, "Intro paragraph.") || !strings.Contains(newBody, "Outro paragraph.") {
		t.Fatalf("surrounding text not preserved:\n%s", newBody)
	}
}

// TestExtractSeriesBelowThreshold: a 2-embed run stays inline when min is 3.
func TestExtractSeriesBelowThreshold(t *testing.T) {
	body := "![](blob://a)\n![](blob://b)\n\ntext"
	newBody, recs := extractSeries(body, 3, ts)
	if len(recs) != 0 {
		t.Fatalf("want 0 series, got %d", len(recs))
	}
	if newBody != body {
		t.Fatalf("body changed:\n%s", newBody)
	}
}

// TestExtractSeriesScatteredSingles: embeds separated by prose are not a run.
func TestExtractSeriesScatteredSingles(t *testing.T) {
	body := "![](blob://a)\n\nsome words\n\n![](blob://b)\n\nmore words\n\n![](blob://c)"
	_, recs := extractSeries(body, 3, ts)
	if len(recs) != 0 {
		t.Fatalf("want 0 series (singles split by prose), got %d", len(recs))
	}
}

// TestExtractSeriesHeadings: headings split runs, and the heading immediately
// above a run becomes that series' title — the ocean-dreams shape.
func TestExtractSeriesHeadings(t *testing.T) {
	body := strings.Join([]string{
		"## Generation 1",
		"",
		"![](blob://g1a)",
		"![](blob://g1b)",
		"![](blob://g1c)",
		"",
		"## Generation 2",
		"",
		"![](blob://g2a)",
		"![](blob://g2b)",
		"![](blob://g2c)",
	}, "\n")

	newBody, recs := extractSeries(body, 3, ts)
	if len(recs) != 2 {
		t.Fatalf("want 2 series, got %d", len(recs))
	}
	if recs[0].record["title"] != "Generation 1" {
		t.Fatalf("series 1 title = %v, want Generation 1", recs[0].record["title"])
	}
	if recs[1].record["title"] != "Generation 2" {
		t.Fatalf("series 2 title = %v, want Generation 2", recs[1].record["title"])
	}
	if !strings.Contains(newBody, "## Generation 1") || !strings.Contains(newBody, "## Generation 2") {
		t.Fatalf("headings not preserved:\n%s", newBody)
	}
	if recs[0].rkey == recs[1].rkey {
		t.Fatal("distinct runs produced the same rkey")
	}
}

// TestExtractSeriesNoTitle: a run with no heading above it gets no title.
func TestExtractSeriesNoTitle(t *testing.T) {
	body := "Just prose.\n\n![](blob://a)\n![](blob://b)\n![](blob://c)"
	_, recs := extractSeries(body, 3, ts)
	if len(recs) != 1 {
		t.Fatalf("want 1 series, got %d", len(recs))
	}
	if _, ok := recs[0].record["title"]; ok {
		t.Fatalf("unexpected title: %v", recs[0].record["title"])
	}
}

// TestExtractSeriesDeterministic: the rkey is the content CID of the refs, so
// identical runs yield identical rkeys — re-running upserts, never duplicates.
func TestExtractSeriesDeterministic(t *testing.T) {
	body := "![](blob://x)\n![](blob://y)\n![](blob://z)"
	_, a := extractSeries(body, 3, ts)
	_, b := extractSeries(body, 3, ts)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("want 1 series each, got %d and %d", len(a), len(b))
	}
	if a[0].rkey != b[0].rkey {
		t.Fatalf("rkey not deterministic: %s vs %s", a[0].rkey, b[0].rkey)
	}
}

// TestExtractSeriesIdempotent: a body already holding a series:// marker has no
// qualifying run left, so a second pass is a no-op.
func TestExtractSeriesIdempotent(t *testing.T) {
	body := "![](blob://a)\n![](blob://b)\n![](blob://c)"
	once, _ := extractSeries(body, 3, ts)
	twice, recs := extractSeries(once, 3, ts)
	if len(recs) != 0 {
		t.Fatalf("second pass extracted %d series, want 0", len(recs))
	}
	if twice != once {
		t.Fatalf("second pass changed the body:\n%s", twice)
	}
}
