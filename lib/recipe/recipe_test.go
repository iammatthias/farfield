package recipe

import (
	"strings"
	"testing"
)

// krispies is the Cooking for Engineers "Rice Krispies Treats" recipe, whose
// published table this package's layout has to reproduce exactly:
//
//	butter        melt  ┐ stir until melted ┐ stir until coated ┐ press ┐ cool ┐ cut
//	marshmallows        ┘                   │                   │       │      │
//	rice cereal                             ┘                   ┘       ┘      ┘
const krispies = `
yield: 24 squares
ingredients:
  - {item: butter, amount: 3 Tbs. (43 g)}
  - {item: marshmallows, amount: 10 oz. (280 g)}
  - {item: Rice Krispies cereal, amount: 6 cups (160 g), id: cereal}
steps:
  - {id: melt, in: [butter], do: melt}
  - {id: soften, in: [melt, marshmallows], do: stir until melted}
  - {id: coat, in: [soften, cereal], do: stir until coated}
  - {do: press into 13x9-in. pan}
  - {do: cool}
  - {do: cut}
`

func TestLayoutMatchesCookingForEngineers(t *testing.T) {
	rec, err := Parse(krispies)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	grids, err := rec.Layout()
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if len(grids) != 1 {
		t.Fatalf("got %d grids, want 1", len(grids))
	}
	g := grids[0]
	if g.Width != 6 {
		t.Errorf("width = %d, want 6", g.Width)
	}
	if len(g.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(g.Rows))
	}

	// Row 1 opens every operation: melt spans only the butter, each later one
	// reaches down over the rows it has absorbed.
	want := []struct {
		text string
		span int
	}{
		{"melt", 1},
		{"stir until melted", 2},
		{"stir until coated", 3},
		{"press into 13x9-in. pan", 3},
		{"cool", 3},
		{"cut", 3},
	}
	if len(g.Rows[0].Cells) != len(want) {
		t.Fatalf("row 0 has %d cells, want %d", len(g.Rows[0].Cells), len(want))
	}
	for i, w := range want {
		got := g.Rows[0].Cells[i]
		if got.Text != w.text || got.RowSpan != w.span {
			t.Errorf("row 0 cell %d = %q rowspan %d, want %q rowspan %d",
				i, got.Text, got.RowSpan, w.text, w.span)
		}
	}

	// Row 2 needs one filler under "melt"; row 3 needs a two-wide filler under
	// "melt" and "stir until melted". These are CFE's righthide cells.
	for _, tc := range []struct{ row, colspan int }{{1, 1}, {2, 2}} {
		cells := g.Rows[tc.row].Cells
		if len(cells) != 1 {
			t.Fatalf("row %d has %d cells, want 1 filler", tc.row, len(cells))
		}
		if !cells[0].Gap || cells[0].ColSpan != tc.colspan {
			t.Errorf("row %d filler = gap:%v colspan:%d, want gap:true colspan:%d",
				tc.row, cells[0].Gap, cells[0].ColSpan, tc.colspan)
		}
	}
}

func TestRenderHasGridAndTraditionalView(t *testing.T) {
	rec, err := Parse(krispies)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	html := string(Render(rec))
	for _, want := range []string{
		`class="ff-recipe-grid"`,   // the tabular view
		`rowspan="3"`,              // an operation bracketing three rows
		`class="ff-r-gap"`,         // filler under a shorter bracket
		`class="ff-recipe-detail"`, // the method, from the same data
		`<ol class="ff-recipe-steps">`,
		`24 squares`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML missing %q", want)
		}
	}

	// The grid's left column is the ingredient list — amount and item both
	// live in it, and nothing repeats them in a second table underneath.
	for _, want := range []string{
		`class="ff-r-amt">3 Tbs. (43 g)`,
		`class="ff-r-item">butter`,
		`class="ff-r-item">Rice Krispies cereal`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("ingredient column missing %q", want)
		}
	}
	if strings.Contains(html, "ff-recipe-ingredients") {
		t.Error("a laid-out recipe should not also render a separate ingredients list")
	}
	if n := strings.Count(html, "marshmallows"); n != 1 {
		t.Errorf("marshmallows appears %d times, want 1 — its grid row and nowhere else", n)
	}
	// Every step reaches the numbered list, including the ones the grid draws
	// as a single narrow column.
	for _, do := range []string{"melt", "stir until coated", "cut"} {
		if strings.Count(html, do) < 2 {
			t.Errorf("step %q should appear in both the grid and the step list", do)
		}
	}
}

func TestDefaultsAndIDs(t *testing.T) {
	rec, err := Parse(`
ingredients:
  - {item: All-purpose flour, amount: 2 cups}
  - {item: Salt, amount: 1 tsp}
  - {item: Salt, amount: a pinch}
steps:
  - {in: [all-purpose-flour, salt, salt-2], do: whisk}
  - {do: rest}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := rec.Ingredients[0].ID; got != "all-purpose-flour" {
		t.Errorf("auto id = %q, want all-purpose-flour", got)
	}
	if got := rec.Ingredients[2].ID; got != "salt-2" {
		t.Errorf("duplicate id = %q, want salt-2", got)
	}
	// A step with no `in` continues from the one before it.
	if got := rec.Steps[1].In; len(got) != 1 || got[0] != "s1" {
		t.Errorf("default in = %v, want [s1]", got)
	}
}

func TestVerticalHeuristic(t *testing.T) {
	rec, err := Parse(krispies)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g, err := rec.Layout()
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	byText := map[string]Cell{}
	for _, c := range g[0].Rows[0].Cells {
		byText[c.Text] = c
	}
	// One row tall: rotating would gain nothing.
	if byText["melt"].Vertical {
		t.Error("single-row cell should stay horizontal")
	}
	// Tall and short: rotate, which is what keeps six columns on the page.
	// CFE rotates every multi-row cell in this recipe, "press into 13x9-in.
	// pan" included, and so does the heuristic.
	for _, text := range []string{"cut", "cool", "press into 13x9-in. pan"} {
		if !byText[text].Vertical {
			t.Errorf("tall cell %q should rotate", text)
		}
	}

	// Tall but too wordy to read sideways — the column widens instead.
	long, err := Parse(`
ingredients: [{item: flour}, {item: water}]
steps:
  - {in: [flour, water], do: knead on a floured surface until smooth and elastic}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	lg, err := long.Layout()
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if lg[0].Rows[0].Cells[0].Vertical {
		t.Error("long label should stay horizontal")
	}
}

func TestDurations(t *testing.T) {
	rec, err := Parse(`
ingredients:
  - {item: butter, amount: 3 Tbs.}
  - {item: marshmallows, amount: 10 oz.}
steps:
  - {id: melt, in: [butter], do: melt, for: 2 min}
  - {in: [melt, marshmallows], do: stir until melted, for: 3–4 min}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g, err := rec.Layout()
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if got := g[0].Rows[0].Cells[0].For; got != "2 min" {
		t.Errorf("cell duration = %q, want %q", got, "2 min")
	}

	html := string(Render(rec))
	for _, want := range []string{
		`class="ff-r-lbl">melt`,
		`class="ff-r-for">2 min`,
		`class="ff-r-for">3–4 min`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML missing %q", want)
		}
	}
	// The duration reaches the numbered list too, not only the grid.
	if n := strings.Count(html, `>2 min<`); n != 2 {
		t.Errorf("2 min appears %d times, want 2 (grid cell and method)", n)
	}
}

func TestDurationCountsTowardRotation(t *testing.T) {
	// A rotated cell puts the label and the duration side by side, so the
	// cell's height is the longer of the two — not their sum. A short label
	// with a short duration must still rotate.
	rec, err := Parse(`
ingredients: [{item: flour}, {item: water}]
steps:
  - {in: [flour, water], do: rest, for: overnight}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g, _ := rec.Layout()
	if !g[0].Rows[0].Cells[0].Vertical {
		t.Error("short label with a short duration should still rotate")
	}

	long, err := Parse(`
ingredients: [{item: flour}, {item: water}]
steps:
  - {in: [flour, water], do: rest, for: "at least 12 hours, and up to 3 days in the fridge"}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	lg, _ := long.Layout()
	if lg[0].Rows[0].Cells[0].Vertical {
		t.Error("an over-long duration should keep the cell horizontal")
	}
}

func TestTwoWaysMakesTwoGrids(t *testing.T) {
	rec, err := Parse(`
ingredients:
  - {item: stock, amount: 6 cups}
  - {item: potatoes, amount: 3}
  - {item: beef, amount: 1 lb}
steps:
  - {id: base, in: [stock], do: simmer}
  - {in: [base, potatoes], do: add and stew, title: Vegetarian}
  - {in: [base, beef], do: add and braise, title: With beef}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	grids, err := rec.Layout()
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if len(grids) != 2 {
		t.Fatalf("got %d grids, want 2", len(grids))
	}
	if grids[0].Title != "Vegetarian" || grids[1].Title != "With beef" {
		t.Errorf("titles = %q, %q", grids[0].Title, grids[1].Title)
	}
	// The shared base is drawn in both grids — each table stands alone.
	for i, g := range grids {
		if len(g.Rows) != 2 {
			t.Errorf("grid %d has %d rows, want 2 (shared stock + its own)", i, len(g.Rows))
		}
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"unknown ref": `
ingredients: [{item: flour}]
steps: [{in: [sugar], do: whisk}]`,
		"orphan ingredient": `
ingredients: [{item: flour}, {item: sugar}]
steps: [{in: [flour], do: whisk}]`,
		"cycle": `
ingredients: [{item: flour}]
steps:
  - {id: a, in: [flour, b], do: one}
  - {id: b, in: [a], do: two}`,
		"first step has no input": `
ingredients: [{item: flour}]
steps: [{do: whisk}]`,
		"step without do": `
ingredients: [{item: flour}]
steps: [{in: [flour]}]`,
		"no ingredients": `
steps: [{do: whisk}]`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(src); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestDiamondIsRejected(t *testing.T) {
	// A step feeding two branches that rejoin in one grid would need its rows
	// in two places at once. There is no table for that, so it must not
	// silently render a wrong one.
	_, err := Parse(`
ingredients: [{item: flour}, {item: water}]
steps:
  - {id: mix, in: [flour, water], do: mix}
  - {id: left, in: [mix], do: half one}
  - {id: right, in: [mix], do: half two}
  - {in: [left, right], do: recombine}
`)
	if err != nil {
		t.Fatalf("parse should succeed; the shape is only a layout problem: %v", err)
	}
	rec, _ := Parse(`
ingredients: [{item: flour}, {item: water}]
steps:
  - {id: mix, in: [flour, water], do: mix}
  - {id: left, in: [mix], do: half one}
  - {id: right, in: [mix], do: half two}
  - {in: [left, right], do: recombine}
`)
	if _, err := rec.Layout(); err == nil {
		t.Fatal("expected a layout error for a rejoining branch")
	}
}
