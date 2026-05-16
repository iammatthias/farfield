package main

import (
	"flag"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/core"
	"github.com/iammatthias/farfield/lib/schema"
)

// blobEmbedRe matches a line that is exactly one markdown embed pointing at a
// blob — `![alt](blob://<cid>)`. Alt text is ignored; a series ref is the bare
// CID, and the referenced blob's mimetype lives on its own media record, so a
// run may mix images, video, and other types.
var blobEmbedRe = regexp.MustCompile(`^!\[[^\]]*\]\(blob://([a-z0-9]+)\)$`)

// headingRe matches an ATX markdown heading, capturing its text.
var headingRe = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*$`)

// cmdExtractSeries lifts runs of consecutive inline blob embeds out of content
// bodies into explicit `series` records. It runs after migrate-images, on the
// already-rewritten `blob://` bodies: a run of `minRun`+ adjacent embeds (blank
// lines allowed between) becomes one series record, and the run is replaced in
// the body by a single `![](series://<rkey>)` line.
//
// The series rkey is the content CID of its ordered refs, so re-running is
// idempotent — a body already holding `series://` has no qualifying run left.
func cmdExtractSeries(args []string) error {
	fs := flag.NewFlagSet("extract-series", flag.ExitOnError)
	content := fs.String("content", defaultService, "content service URL")
	schemaDir := fs.String("schemas", defaultSchemas, "schema directory")
	minRun := fs.Int("min", 3, "minimum consecutive embeds to form a series")
	dryRun := fs.Bool("dry-run", false, "report, but write nothing")
	_ = fs.Parse(args)

	if *minRun < 2 {
		return fmt.Errorf("-min must be at least 2")
	}
	set, err := schema.Load(*schemaDir)
	if err != nil {
		return fmt.Errorf("loading schemas: %w", err)
	}
	tok := token()
	var seriesCount, rewritten, failed int

	for _, col := range set.Collections() {
		// media records carry no body; series is what we're writing.
		if col.Name == "media" || col.Name == "series" {
			continue
		}
		records, err := listRecords(*content, col.Name)
		if err != nil {
			return err
		}
		for _, rec := range records {
			value, _ := rec["value"].(map[string]any)
			rkey, _ := rec["rkey"].(string)
			if value == nil {
				continue
			}
			body, _ := value["body"].(string)
			created, _ := value["created"].(string)
			if created == "" {
				created = time.Now().UTC().Format(time.RFC3339)
			}

			newBody, found := extractSeries(body, *minRun, created)
			if len(found) == 0 {
				continue
			}

			ok := true
			for _, sr := range found {
				if *dryRun {
					fmt.Printf("ok    series/%s — %d media (from %s/%s)\n",
						sr.rkey, len(sr.refs), col.Name, rkey)
					continue
				}
				if _, err := send(*content, "series", sr.rkey, sr.record, tok); err != nil {
					fmt.Printf("fail  series/%s — %v\n", sr.rkey, err)
					failed++
					ok = false
				}
			}
			if !ok {
				continue
			}
			seriesCount += len(found)

			if *dryRun {
				fmt.Printf("ok    %s/%s — would extract %d series\n", col.Name, rkey, len(found))
				rewritten++
				continue
			}
			value["body"] = newBody
			if _, err := send(*content, col.Name, rkey, value, tok); err != nil {
				fmt.Printf("fail  %s/%s — %v\n", col.Name, rkey, err)
				failed++
				continue
			}
			fmt.Printf("ok    %s/%s — extracted %d series\n", col.Name, rkey, len(found))
			rewritten++
		}
	}

	verb := "done"
	if *dryRun {
		verb = "dry-run"
	}
	fmt.Printf("\n%s: %d series, %d record(s) rewritten, %d failed\n",
		verb, seriesCount, rewritten, failed)
	if failed > 0 {
		return fmt.Errorf("%d failure(s)", failed)
	}
	return nil
}

// seriesRec is one series to create: its rkey, its ordered refs, and the
// record payload sent to the content service.
type seriesRec struct {
	rkey   string
	refs   []string
	record map[string]any
}

// extractSeries finds every run of minRun+ consecutive blob embeds in body and
// returns the rewritten body plus the series records to create. created is the
// parent entry's timestamp, carried onto each series. With no qualifying run
// it returns body unchanged and a nil slice.
func extractSeries(body string, minRun int, created string) (string, []seriesRec) {
	lines := strings.Split(body, "\n")
	runs := findRuns(lines, minRun)
	if len(runs) == 0 {
		return body, nil
	}

	recs := make([]seriesRec, 0, len(runs))
	out := make([]string, 0, len(lines))
	ri := 0
	for i := 0; i < len(lines); {
		if ri < len(runs) && runs[ri].start == i {
			run := runs[ri]
			rkey := core.BlobCID([]byte(strings.Join(run.cids, "\n")))
			record := map[string]any{
				"refs":    run.cids,
				"created": created,
			}
			if title := headingBefore(lines, run.start); title != "" {
				record["title"] = title
			}
			recs = append(recs, seriesRec{rkey: rkey, refs: run.cids, record: record})
			out = append(out, "![](series://"+rkey+")")
			i = run.end + 1
			ri++
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n"), recs
}

// run is a maximal stretch of consecutive blob embeds in a body's lines.
type run struct {
	start, end int // inclusive line indices
	cids       []string
}

// findRuns returns every maximal run of minRun+ consecutive blob embeds. Blank
// lines between embeds are part of the run; any other content ends it.
func findRuns(lines []string, minRun int) []run {
	var runs []run
	for i := 0; i < len(lines); {
		cid, ok := embedCID(lines[i])
		if !ok {
			i++
			continue
		}
		r := run{start: i, end: i, cids: []string{cid}}
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue // blank lines are interior to the run
			}
			c, ok := embedCID(lines[j])
			if !ok {
				break
			}
			r.cids = append(r.cids, c)
			r.end = j
		}
		if len(r.cids) >= minRun {
			runs = append(runs, r)
		}
		i = r.end + 1
	}
	return runs
}

// embedCID returns the blob CID of a line that is exactly one blob embed.
func embedCID(line string) (string, bool) {
	m := blobEmbedRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// headingBefore returns the text of the heading immediately preceding a run
// (skipping blank lines), or "" if the nearest non-blank line is not a heading.
func headingBefore(lines []string, start int) string {
	for k := start - 1; k >= 0; k-- {
		t := strings.TrimSpace(lines[k])
		if t == "" {
			continue
		}
		if m := headingRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
		return ""
	}
	return ""
}
