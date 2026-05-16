package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iammatthias/farfield/lib/schema"
	"gopkg.in/yaml.v3"
)

// cmdExport is the reverse of import: it pulls every content record and writes
// it back out as a markdown file — YAML frontmatter plus the body — under
// <dir>/<collection>/<rkey>.md. It reconstructs an authoring vault from the
// served state, so a vault exported after migrate-images / extract-series
// holds the migrated bodies (blob://, series://), not the original ipfs://.
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	content := fs.String("content", defaultService, "content service URL")
	schemaDir := fs.String("schemas", defaultSchemas, "schema directory")
	_ = fs.Parse(args)
	positionals, trailing := splitPositionals(fs.Args())
	_ = fs.Parse(trailing)
	if len(positionals) == 0 {
		return fmt.Errorf("usage: farfield export <dir> [flags]")
	}
	dir := positionals[0]

	set, err := schema.Load(*schemaDir)
	if err != nil {
		return fmt.Errorf("loading schemas: %w", err)
	}
	var written, failed int

	for _, col := range set.Collections() {
		// media and series are derived records, not authored notes.
		if col.Name == "media" || col.Name == "series" {
			continue
		}
		records, err := listRecords(*content, col.Name)
		if err != nil {
			return err
		}
		colDir := filepath.Join(dir, col.Name)
		if err := os.MkdirAll(colDir, 0o755); err != nil {
			return err
		}
		fmt.Printf("\n%s/  (%d records)\n", col.Name, len(records))
		for _, rec := range records {
			rkey, _ := rec["rkey"].(string)
			value, _ := rec["value"].(map[string]any)
			if rkey == "" || value == nil {
				fmt.Printf("fail  %s — malformed record\n", col.Name)
				failed++
				continue
			}
			if err := writeMarkdown(filepath.Join(colDir, rkey+".md"), value); err != nil {
				fmt.Printf("fail  %s/%s — %v\n", col.Name, rkey, err)
				failed++
				continue
			}
			fmt.Printf("ok    %s/%s\n", col.Name, rkey)
			written++
		}
	}

	fmt.Printf("\ndone: %d file(s) written, %d failed\n", written, failed)
	if failed > 0 {
		return fmt.Errorf("%d failure(s)", failed)
	}
	return nil
}

// writeMarkdown reconstructs a markdown file from a record value: YAML
// frontmatter for every field except `body`, then the body.
func writeMarkdown(path string, value map[string]any) error {
	body, _ := value["body"].(string)
	front := make(map[string]any, len(value))
	for k, v := range value {
		if k != "body" {
			front[k] = v
		}
	}
	yamlBytes, err := yaml.Marshal(front)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(yamlBytes)
	buf.WriteString("---\n\n")
	buf.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		buf.WriteString("\n")
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
