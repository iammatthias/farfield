// Package schema is the lexicon-lite schema system.
//
// A schema is one JSON file: { id, version, required, properties }. A
// collections.json manifest in the same directory maps each collection name
// to a schema id plus display metadata. Loading and validation are shared by
// the services and the CLI's codegen, so frontend types and backend
// validation cannot drift.
//
// Validation is a small hand-rolled check over the lexicon-lite subset —
// deliberately not a full JSON Schema engine.
package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FieldType is a lexicon-lite field type.
type FieldType string

const (
	TypeString   FieldType = "string"
	TypeInteger  FieldType = "integer"
	TypeFloat    FieldType = "float"
	TypeBoolean  FieldType = "boolean"
	TypeDatetime FieldType = "datetime" // an RFC3339 timestamp, carried as a string
	TypeArray    FieldType = "array"
	TypeObject   FieldType = "object"
)

// Field is one entry in a schema's properties.
type Field struct {
	Type        FieldType `json:"type"`
	Description string    `json:"description,omitempty"`
	Items       *Field    `json:"items,omitempty"` // element type, when Type is array
}

// Schema is a content-type schema.
type Schema struct {
	ID          string           `json:"id"`
	Version     int              `json:"version,omitempty"`
	Description string           `json:"description,omitempty"`
	Required    []string         `json:"required,omitempty"`
	Properties  map[string]Field `json:"properties"`
}

// Collection is a collection's definition: its name, the schema it uses, and
// display metadata for the site's menus.
type Collection struct {
	Name        string `json:"name"`
	Schema      string `json:"schema"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

type manifest struct {
	Collections []Collection `json:"collections"`
}

// Set is a loaded set of schemas plus the collection manifest.
type Set struct {
	schemas     map[string]Schema
	collections []Collection
}

// Load reads every *.json in dir: collections.json is the manifest, every
// other file is a schema.
func Load(dir string) (*Set, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	set := &Set{schemas: map[string]Schema{}}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		if name == "collections.json" {
			var m manifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", name, err)
			}
			set.collections = m.Collections
			continue
		}
		var s Schema
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		if _, dup := set.schemas[s.ID]; dup {
			return nil, fmt.Errorf("duplicate schema id %q", s.ID)
		}
		set.schemas[s.ID] = s
	}
	for _, c := range set.collections {
		if _, ok := set.schemas[c.Schema]; !ok {
			return nil, fmt.Errorf("collection %q references unknown schema %q", c.Name, c.Schema)
		}
	}
	return set, nil
}

// Collections returns every collection in the manifest.
func (s *Set) Collections() []Collection { return s.collections }

// Collection returns one collection's definition, if it exists.
func (s *Set) Collection(name string) (Collection, bool) {
	for _, c := range s.collections {
		if c.Name == name {
			return c, true
		}
	}
	return Collection{}, false
}

// SchemaFor returns the schema backing a collection, if the collection exists.
func (s *Set) SchemaFor(collection string) (Schema, bool) {
	c, ok := s.Collection(collection)
	if !ok {
		return Schema{}, false
	}
	sc, ok := s.schemas[c.Schema]
	return sc, ok
}

// Validate checks a record against its collection's schema.
func (s *Set) Validate(collection string, record map[string]any) error {
	sc, ok := s.SchemaFor(collection)
	if !ok {
		return &ValidationError{Issues: []string{fmt.Sprintf("unknown collection %q", collection)}}
	}
	return ValidateAgainst(sc, record)
}

// ValidationError is a record that failed validation, with one issue per
// problem found.
type ValidationError struct {
	Issues []string
}

func (e *ValidationError) Error() string {
	return "invalid record: " + strings.Join(e.Issues, "; ")
}

// ValidateAgainst checks record against schema: required fields present, no
// unknown fields, every field the declared type.
func ValidateAgainst(s Schema, record map[string]any) error {
	var issues []string
	for _, req := range s.Required {
		if _, ok := record[req]; !ok {
			issues = append(issues, fmt.Sprintf("missing required field `%s`", req))
		}
	}
	// Deterministic issue order.
	keys := make([]string, 0, len(record))
	for k := range record {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		field, known := s.Properties[k]
		if !known {
			issues = append(issues, fmt.Sprintf("unknown field `%s` (not in schema)", k))
			continue
		}
		checkField(k, field, record[k], &issues)
	}
	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

func checkField(path string, f Field, v any, issues *[]string) {
	ok := false
	switch f.Type {
	case TypeString, TypeDatetime:
		_, ok = v.(string)
	case TypeBoolean:
		_, ok = v.(bool)
	case TypeInteger, TypeFloat:
		_, ok = v.(float64) // JSON numbers decode to float64
	case TypeObject:
		_, ok = v.(map[string]any)
	case TypeArray:
		_, ok = v.([]any)
	}
	if !ok {
		*issues = append(*issues, fmt.Sprintf("field `%s` should be %s, got %s", path, f.Type, kindOf(v)))
		return
	}
	if f.Type == TypeArray && f.Items != nil {
		for i, e := range v.([]any) {
			checkField(fmt.Sprintf("%s[%d]", path, i), *f.Items, e, issues)
		}
	}
}

func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}
