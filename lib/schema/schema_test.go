package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

func entrySchema(t *testing.T) Schema {
	t.Helper()
	var s Schema
	src := `{
		"id": "entry",
		"required": ["title", "tags", "body"],
		"properties": {
			"title":   { "type": "string" },
			"tags":    { "type": "array", "items": { "type": "string" } },
			"excerpt": { "type": "string" },
			"body":    { "type": "string" }
		}
	}`
	if err := json.Unmarshal([]byte(src), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func obj(t *testing.T, jsonStr string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestValidRecordPasses(t *testing.T) {
	err := ValidateAgainst(entrySchema(t), obj(t, `{"title":"x","tags":["a","b"],"body":"hi"}`))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestOptionalFieldMayBeAbsent(t *testing.T) {
	err := ValidateAgainst(entrySchema(t), obj(t, `{"title":"x","tags":[],"body":"hi"}`))
	if err != nil {
		t.Fatalf("excerpt is optional, got %v", err)
	}
}

func TestMissingRequiredFieldIsNamed(t *testing.T) {
	err := ValidateAgainst(entrySchema(t), obj(t, `{"tags":[],"body":"hi"}`))
	if err == nil || !strings.Contains(err.Error(), "`title`") {
		t.Fatalf("expected a `title` issue, got %v", err)
	}
}

func TestWrongTypeIsCaught(t *testing.T) {
	err := ValidateAgainst(entrySchema(t), obj(t, `{"title":7,"tags":[],"body":"hi"}`))
	if err == nil || !strings.Contains(err.Error(), "`title`") {
		t.Fatalf("expected a `title` type issue, got %v", err)
	}
}

func TestWrongArrayElementTypeIsCaught(t *testing.T) {
	err := ValidateAgainst(entrySchema(t), obj(t, `{"title":"x","tags":["ok",9],"body":"h"}`))
	if err == nil || !strings.Contains(err.Error(), "tags[1]") {
		t.Fatalf("expected a tags[1] issue, got %v", err)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	err := ValidateAgainst(entrySchema(t), obj(t, `{"title":"x","tags":[],"body":"h","surprise":1}`))
	if err == nil || !strings.Contains(err.Error(), "`surprise`") {
		t.Fatalf("expected a `surprise` issue, got %v", err)
	}
}

func TestRealContentSchemasLoadAndValidate(t *testing.T) {
	// Integration check against the repo's actual schema files.
	set, err := Load("../../schemas/content")
	if err != nil {
		t.Fatalf("real content schemas should load: %v", err)
	}
	if len(set.Collections()) != 7 {
		t.Fatalf("expected 7 collections, got %d", len(set.Collections()))
	}
	for _, name := range []string{"posts", "melange", "media"} {
		if _, ok := set.SchemaFor(name); !ok {
			t.Fatalf("missing schema for %q", name)
		}
	}
	record := obj(t, `{
		"title": "Sourdough",
		"slug": "1587970800000-sourdough",
		"published": true,
		"created": "2023-11-02T12:14:00Z",
		"updated": "2025-05-24T16:58:00Z",
		"tags": ["sourdough","bread"],
		"excerpt": "Growing up near San Francisco...",
		"body": "# Sourdough"
	}`)
	if err := set.Validate("recipes", record); err != nil {
		t.Fatalf("a real-shaped record should validate: %v", err)
	}
}
