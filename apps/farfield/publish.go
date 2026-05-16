package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/schema"
	"gopkg.in/yaml.v3"
)

// cmdImport bulk-imports a directory whose subfolders are collections.
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	service := fs.String("service", defaultService, "content service URL")
	schemaDir := fs.String("schemas", defaultSchemas, "schema directory")
	dryRun := fs.Bool("dry-run", false, "parse and validate, but write nothing")
	_ = fs.Parse(args)
	positionals, trailing := splitPositionals(fs.Args())
	_ = fs.Parse(trailing)
	if len(positionals) == 0 {
		return fmt.Errorf("usage: farfield import <dir> [flags]")
	}
	dir := positionals[0]

	set, err := schema.Load(*schemaDir)
	if err != nil {
		return fmt.Errorf("loading schemas: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	tok := token()
	var t tally

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		collection := e.Name()
		sc, ok := set.SchemaFor(collection)
		if !ok {
			fmt.Printf("skip  %s/ — not a known collection\n", collection)
			continue
		}
		files, _ := filepath.Glob(filepath.Join(dir, collection, "*.md"))
		sort.Strings(files)
		fmt.Printf("\n%s/  (%d files)\n", collection, len(files))
		for _, f := range files {
			publishFile(f, collection, sc, *service, tok, *dryRun, &t)
		}
	}

	fmt.Printf("\n%s\n", t.summary(*dryRun))
	if t.failed > 0 {
		return fmt.Errorf("%d file(s) failed", t.failed)
	}
	return nil
}

// cmdPush publishes individual files; the collection is the parent folder.
func cmdPush(args []string) error {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	service := fs.String("service", defaultService, "content service URL")
	schemaDir := fs.String("schemas", defaultSchemas, "schema directory")
	dryRun := fs.Bool("dry-run", false, "parse and validate, but write nothing")
	_ = fs.Parse(args)
	files, trailing := splitPositionals(fs.Args())
	_ = fs.Parse(trailing)
	if len(files) == 0 {
		return fmt.Errorf("usage: farfield push <file>... [flags]")
	}

	set, err := schema.Load(*schemaDir)
	if err != nil {
		return fmt.Errorf("loading schemas: %w", err)
	}
	tok := token()
	var t tally

	for _, f := range files {
		collection := filepath.Base(filepath.Dir(f))
		sc, ok := set.SchemaFor(collection)
		if !ok {
			fmt.Printf("fail  %s — unknown collection %q\n", f, collection)
			t.failed++
			continue
		}
		publishFile(f, collection, sc, *service, tok, *dryRun, &t)
	}

	fmt.Printf("\n%s\n", t.summary(*dryRun))
	if t.failed > 0 {
		return fmt.Errorf("%d file(s) failed", t.failed)
	}
	return nil
}

func publishFile(file, collection string, sc schema.Schema, service, tok string, dryRun bool, t *tally) {
	rkey, record, err := prepare(file, sc)
	if err != nil {
		fmt.Printf("fail  %s — %v\n", file, err)
		t.failed++
		return
	}
	if dryRun {
		fmt.Printf("ok    %s/%s (dry-run)\n", collection, rkey)
		t.dry++
		return
	}
	outcome, err := send(service, collection, rkey, record, tok)
	if err != nil {
		fmt.Printf("fail  %s/%s — %v\n", collection, rkey, err)
		t.failed++
		return
	}
	fmt.Printf("%-5s %s/%s\n", outcome, collection, rkey)
	t.add(outcome)
}

// prepare reads a markdown file and builds its record: (rkey, record).
func prepare(file string, sc schema.Schema) (string, map[string]any, error) {
	text, err := os.ReadFile(file)
	if err != nil {
		return "", nil, err
	}
	front, body, err := splitFrontmatter(string(text))
	if err != nil {
		return "", nil, err
	}
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return "", nil, fmt.Errorf("parsing YAML frontmatter: %w", err)
	}
	record, err := buildRecord(sc, fm, body)
	if err != nil {
		return "", nil, err
	}
	rkey, _ := fm["slug"].(string)
	if rkey == "" {
		rkey = strings.TrimSuffix(filepath.Base(file), ".md")
	}
	if !validRkey(rkey) {
		return "", nil, fmt.Errorf("slug %q is not a valid rkey ([a-z0-9-], 1-128)", rkey)
	}
	if err := schema.ValidateAgainst(sc, record); err != nil {
		return "", nil, err
	}
	return rkey, record, nil
}

// buildRecord projects the frontmatter onto the schema: every declared field,
// coerced to its type; body taken from the markdown body. Unknown fields drop.
func buildRecord(sc schema.Schema, fm map[string]any, body string) (map[string]any, error) {
	out := map[string]any{}
	for name, field := range sc.Properties {
		if name == "body" {
			out["body"] = body
			continue
		}
		v, ok := fm[name]
		if !ok {
			continue
		}
		coerced, err := coerce(field, v)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		out[name] = coerced
	}
	return out, nil
}

func coerce(field schema.Field, v any) (any, error) {
	switch field.Type {
	case schema.TypeString:
		return asString(v), nil
	case schema.TypeDatetime:
		if t, ok := v.(time.Time); ok {
			return t.UTC().Format(time.RFC3339), nil
		}
		return toRFC3339(asString(v))
	case schema.TypeBoolean:
		switch t := v.(type) {
		case bool:
			return t, nil
		case string:
			return parseBool(t), nil
		default:
			return v, nil
		}
	case schema.TypeArray:
		arr, ok := v.([]any)
		if !ok || field.Items == nil {
			return v, nil
		}
		out := make([]any, len(arr))
		for i, e := range arr {
			c, err := coerce(*field.Items, e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	default: // integer, float, object — pass through
		return v, nil
	}
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// toRFC3339 normalizes a datetime string to RFC3339 UTC. Accepts RFC3339, a
// unix epoch (10-digit seconds / 13-digit millis), or the
// `YYYY-MM-DD HH:MM[:SS]` shape obsidian_cms uses (with an unpadded hour).
func toRFC3339(s string) (string, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	if isAllDigits(s) && len(s) >= 10 && len(s) <= 14 {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			if len(s) >= 13 {
				return time.UnixMilli(n).UTC().Format(time.RFC3339), nil
			}
			return time.Unix(n, 0).UTC().Format(time.RFC3339), nil
		}
	}
	norm := padHour(s)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, norm); err == nil {
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	return "", fmt.Errorf("unparseable datetime %q", s)
}

// padHour zero-pads a single-digit hour ("2024-10-10 9:00" -> "... 09:00") so
// Go's fixed-width time layouts match.
func padHour(s string) string {
	date, clock, found := strings.Cut(s, " ")
	if !found {
		return s
	}
	h, rest, ok := strings.Cut(clock, ":")
	if ok && len(h) == 1 {
		return date + " 0" + h + ":" + rest
	}
	return s
}

func splitFrontmatter(text string) (front, body string, err error) {
	text = strings.TrimPrefix(text, "\ufeff")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		rest, ok = strings.CutPrefix(text, "---\r\n")
	}
	if !ok {
		return "", "", fmt.Errorf("file has no `---` frontmatter block")
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", "", fmt.Errorf("frontmatter block is not terminated by `---`")
	}
	return rest[:idx], strings.TrimLeft(rest[idx+4:], "\r\n"), nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true
	default:
		return false
	}
}

func validRkey(rkey string) bool {
	if len(rkey) < 1 || len(rkey) > 128 {
		return false
	}
	for _, c := range rkey {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// send PUTs a record and reports the outcome: "new", "upd", or "same".
func send(service, collection, rkey string, record map[string]any, tok string) (string, error) {
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/records/%s/%s", service, collection, rkey)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("service unreachable — is it running?")
	}
	defer resp.Body.Close()
	var rb map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&rb)
	if resp.StatusCode >= 300 {
		msg, _ := rb["message"].(string)
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("%d %s", resp.StatusCode, msg)
	}
	if rb["seq"] == nil { // null seq -> no-op write
		return "same", nil
	}
	if resp.StatusCode == http.StatusCreated {
		return "new", nil
	}
	return "upd", nil
}

type tally struct {
	created, updated, unchanged, dry, failed int
}

func (t *tally) add(outcome string) {
	switch outcome {
	case "new":
		t.created++
	case "upd":
		t.updated++
	case "same":
		t.unchanged++
	}
}

func (t *tally) summary(dryRun bool) string {
	if dryRun {
		return fmt.Sprintf("dry-run: %d ok, %d failed", t.dry, t.failed)
	}
	return fmt.Sprintf("%d new, %d updated, %d unchanged, %d failed",
		t.created, t.updated, t.unchanged, t.failed)
}
