// Package recipe parses and renders farfield recipe blocks — the structured
// YAML that a recipe entry carries inside a ```recipe fence in its markdown
// body.
//
// One block is the single source of truth for two views of the same recipe:
// the Cooking-for-Engineers tabular grid (ingredients down the left, operations
// bracketing them rightward) and the traditional ingredients-table-plus-
// numbered-steps layout. Both fall out of the same ingredient list and step
// graph, so they can never disagree.
//
// The grid works because the steps form a tree: every step consumes some set of
// ingredients and earlier step results, and the leaves under a step are always
// a contiguous run of rows. Layout turns that tree into table cells with the
// right rowspans; see layout.go.
package recipe

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Recipe is one parsed ```recipe block.
type Recipe struct {
	Yield  string `yaml:"yield,omitempty"`
	Time   string `yaml:"time,omitempty"`
	Source string `yaml:"source,omitempty"`
	// SourceURL turns Source into a link. A bare URL in Source works too.
	SourceURL string `yaml:"sourceURL,omitempty"`
	// Notes is free prose rendered under the steps.
	Notes string `yaml:"notes,omitempty"`

	Ingredients []Ingredient `yaml:"ingredients"`
	Steps       []Step       `yaml:"steps"`
}

// Ingredient is one row of the grid's left column and one row of the
// traditional ingredients table.
type Ingredient struct {
	// ID is how steps refer to this ingredient. Optional — it defaults to a
	// slug of Item, so most blocks never write one.
	ID     string `yaml:"id,omitempty"`
	Item   string `yaml:"item"`
	Amount string `yaml:"amount,omitempty"`
	Note   string `yaml:"note,omitempty"`
	// Group labels a run of ingredients in the traditional table ("Dough",
	// "Filling"). It has no effect on the grid — grouping there comes from the
	// step tree.
	Group string `yaml:"group,omitempty"`
}

// Step is one operation. In the grid it is a cell spanning the rows of every
// ingredient beneath it; in the traditional view it is one numbered step.
type Step struct {
	// ID is how later steps refer to this result. Optional — it defaults to
	// s1, s2, … in declaration order.
	ID string `yaml:"id,omitempty"`
	// In lists the ingredient and step IDs this step consumes, in the order
	// their rows should appear. Empty means "the previous step's result",
	// which is what a plain linear chain wants.
	In []string `yaml:"in,omitempty"`
	// Do is the terse grid label — "melt", "stir until coated". Keep it short;
	// it has to fit a grid cell.
	Do string `yaml:"do"`
	// Detail is the full prose for the traditional numbered list. Empty falls
	// back to Do.
	Detail string `yaml:"detail,omitempty"`
	// For is how long the operation takes — "20 min", "2–3 h", "overnight".
	// It rides in the grid cell beneath the label, so the grid reads as a
	// timeline, and repeats in the numbered list.
	For string `yaml:"for,omitempty"`
	// Phase groups numbered steps under a subheading ("Prep", "Grill").
	Phase string `yaml:"phase,omitempty"`
	// Title captions this step's grid. Only meaningful on a finishing step —
	// a recipe that ends two ways gives each ending a title.
	Title string `yaml:"title,omitempty"`
	// Prep marks a step that has no place in the grid — preheating an oven,
	// chilling a glass. It still appears in the numbered list.
	Prep bool `yaml:"prep,omitempty"`
	// Vertical overrides the automatic choice to rotate this cell's label.
	Vertical *bool `yaml:"vertical,omitempty"`
}

// Parse decodes a recipe block's YAML source, fills in defaults, and validates
// that the steps form a usable tree. A returned Recipe is always safe to lay
// out; on error it is nil.
func Parse(src string) (*Recipe, error) {
	var r Recipe
	dec := yaml.NewDecoder(strings.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	if err := r.normalize(); err != nil {
		return nil, err
	}
	return &r, nil
}

// errNoIngredients and friends are the validation failures worth naming; the
// rest carry their offending ID in the message.
var (
	errNoIngredients = errors.New("recipe: no ingredients")
	errNoSteps       = errors.New("recipe: no steps")
)

// normalize assigns the IDs the author left off, resolves each step's inputs,
// and rejects anything that cannot be laid out — an unknown reference, a
// cycle, an ingredient nothing consumes.
func (r *Recipe) normalize() error {
	if len(r.Ingredients) == 0 {
		return errNoIngredients
	}
	if len(r.Steps) == 0 {
		return errNoSteps
	}

	// Ingredient IDs: explicit, else a slug of the item, deduplicated so two
	// "salt" rows stay addressable as salt and salt-2.
	taken := map[string]bool{}
	for i := range r.Ingredients {
		ing := &r.Ingredients[i]
		if strings.TrimSpace(ing.Item) == "" {
			return fmt.Errorf("recipe: ingredient %d has no item", i+1)
		}
		id := ing.ID
		if id == "" {
			id = slug(ing.Item)
		}
		if id == "" {
			id = fmt.Sprintf("i%d", i+1)
		}
		base := id
		for n := 2; taken[id]; n++ {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		taken[id] = true
		ing.ID = id
	}

	// Step IDs, in a second pass so a step may not collide with an ingredient.
	for i := range r.Steps {
		st := &r.Steps[i]
		if strings.TrimSpace(st.Do) == "" {
			return fmt.Errorf("recipe: step %d has no `do`", i+1)
		}
		id := st.ID
		if id == "" {
			id = fmt.Sprintf("s%d", i+1)
		}
		if taken[id] {
			return fmt.Errorf("recipe: duplicate id %q", id)
		}
		taken[id] = true
		st.ID = id
	}

	// Default inputs: a step with no `in` continues from the previous
	// grid-bearing step. The first one has nothing to continue from.
	var prev string
	for i := range r.Steps {
		st := &r.Steps[i]
		if st.Prep {
			continue
		}
		if len(st.In) == 0 {
			if prev == "" {
				return fmt.Errorf("recipe: step %q is the first step and needs an `in`", st.ID)
			}
			st.In = []string{prev}
		}
		prev = st.ID
	}

	return r.validateRefs()
}

// validateRefs checks that every `in` names something real, that no step
// depends on itself, and that every ingredient ends up in the grid.
func (r *Recipe) validateRefs() error {
	kind := map[string]string{}
	for _, ing := range r.Ingredients {
		kind[ing.ID] = "ingredient"
	}
	stepAt := map[string]int{}
	for i, st := range r.Steps {
		kind[st.ID] = "step"
		stepAt[st.ID] = i
	}

	used := map[string]bool{}
	for _, st := range r.Steps {
		if st.Prep && len(st.In) > 0 {
			return fmt.Errorf("recipe: step %q is `prep` and cannot take an `in`", st.ID)
		}
		seen := map[string]bool{}
		for _, ref := range st.In {
			if kind[ref] == "" {
				return fmt.Errorf("recipe: step %q refers to unknown id %q", st.ID, ref)
			}
			if ref == st.ID {
				return fmt.Errorf("recipe: step %q refers to itself", st.ID)
			}
			if seen[ref] {
				return fmt.Errorf("recipe: step %q lists %q twice", st.ID, ref)
			}
			seen[ref] = true
			used[ref] = true
		}
	}

	// Cycles. A step may legitimately feed two later steps (a base split into
	// two finishes), so this is a DAG walk, not a tree walk.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var walk func(id string) error
	walk = func(id string) error {
		if kind[id] == "ingredient" {
			return nil
		}
		switch color[id] {
		case grey:
			return fmt.Errorf("recipe: steps form a cycle at %q", id)
		case black:
			return nil
		}
		color[id] = grey
		for _, ref := range r.Steps[stepAt[id]].In {
			if err := walk(ref); err != nil {
				return err
			}
		}
		color[id] = black
		return nil
	}
	for _, st := range r.Steps {
		if err := walk(st.ID); err != nil {
			return err
		}
	}

	// Every ingredient has to be consumed by something, or it would sit in a
	// grid row with nothing to its right.
	var orphans []string
	for _, ing := range r.Ingredients {
		if !used[ing.ID] {
			orphans = append(orphans, ing.ID)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return fmt.Errorf("recipe: no step uses %s", strings.Join(orphans, ", "))
	}
	return nil
}

// Roots are the steps nothing else consumes — each one finishes a dish, and
// each one gets its own grid. A recipe that ends in a single step has one.
func (r *Recipe) Roots() []Step {
	used := map[string]bool{}
	for _, st := range r.Steps {
		for _, ref := range st.In {
			used[ref] = true
		}
	}
	var roots []Step
	for _, st := range r.Steps {
		if !st.Prep && !used[st.ID] {
			roots = append(roots, st)
		}
	}
	return roots
}

// slug lowercases an item name into an ID: alphanumerics kept, everything else
// collapsed to single hyphens.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return b.String()
}
