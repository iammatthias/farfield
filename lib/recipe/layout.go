package recipe

import (
	"fmt"
	"strings"
)

// Grid is one Cooking-for-Engineers table: an ingredient per row down the left,
// operations to the right, each spanning the rows it consumes. A recipe that
// finishes more than one way lays out one Grid per finishing step.
type Grid struct {
	// Title captions the grid when a recipe has more than one.
	Title string
	Rows  []Row
	// Width is the number of operation columns, not counting the ingredient
	// column.
	Width int
}

// Row is one ingredient and the operation cells that begin on its line.
type Row struct {
	Ingredient Ingredient
	Cells      []Cell
}

// Cell is one operation cell, or the empty filler that holds a row's place
// under an operation that starts further right.
type Cell struct {
	Step *Step
	Text string
	// For is the operation's duration, shown under the label.
	For      string
	RowSpan  int
	ColSpan  int
	Vertical bool
	// Gap marks the filler cells. They carry no text and drop their right
	// border, so a row visually runs into the bracket that claims it.
	Gap bool
}

// autoVerticalMax is the longest label that still reads well rotated. Past it
// a vertical cell stretches the table taller than the recipe it describes, so
// the label stays horizontal and the column just gets wider.
//
// It is set generously because a horizontal cell costs its full text width in
// every grid it appears in, and a handful of wide columns is what turns a
// legible grid into a horizontal scroll. Rotation is the cheaper trade almost
// every time — which is why Cooking for Engineers rotates nearly everything.
const autoVerticalMax = 34

// Layout builds one grid per finishing step. It fails only on a shape no table
// can express — a step feeding two branches that rejoin, which would need its
// ingredients on two rows at once.
func (r *Recipe) Layout() ([]Grid, error) {
	stepAt := map[string]int{}
	for i, st := range r.Steps {
		stepAt[st.ID] = i
	}
	ingAt := map[string]int{}
	for i, ing := range r.Ingredients {
		ingAt[ing.ID] = i
	}

	roots := r.Roots()
	grids := make([]Grid, 0, len(roots))
	for _, root := range roots {
		g, err := r.layoutRoot(root, stepAt, ingAt)
		if err != nil {
			return nil, err
		}
		if len(roots) > 1 {
			g.Title = root.Title
			if g.Title == "" {
				g.Title = root.Do
			}
		}
		grids = append(grids, g)
	}
	return grids, nil
}

func (r *Recipe) layoutRoot(root Step, stepAt, ingAt map[string]int) (Grid, error) {
	// leaves walks the tree in declaration order, so a subtree's ingredients
	// always land on consecutive rows — the property that lets an operation be
	// a single rowspan.
	var order []string
	first := map[string]int{}
	span := map[string]int{}
	col := map[string]int{}
	seen := map[string]bool{}

	var walk func(id string) error
	walk = func(id string) error {
		if seen[id] {
			return fmt.Errorf("recipe: %q feeds two branches of the same grid; "+
				"split it into separate steps so each row appears once", id)
		}
		seen[id] = true
		if i, ok := ingAt[id]; ok {
			first[id] = len(order)
			span[id] = 1
			col[id] = -1
			order = append(order, r.Ingredients[i].ID)
			return nil
		}
		st := r.Steps[stepAt[id]]
		start := len(order)
		best := -1
		for _, ref := range st.In {
			if err := walk(ref); err != nil {
				return err
			}
			if col[ref] > best {
				best = col[ref]
			}
		}
		first[id] = start
		span[id] = len(order) - start
		col[id] = best + 1
		return nil
	}
	if err := walk(root.ID); err != nil {
		return Grid{}, err
	}

	rows := len(order)
	width := col[root.ID] + 1
	if rows == 0 || width == 0 {
		return Grid{}, fmt.Errorf("recipe: step %q has nothing under it", root.ID)
	}

	// Place every operation, then read the matrix back out row by row.
	start := make([][]*Step, rows)
	covered := make([][]bool, rows)
	for i := range start {
		start[i] = make([]*Step, width)
		covered[i] = make([]bool, width)
	}
	for id := range seen {
		i, ok := stepAt[id]
		if !ok {
			continue // an ingredient leaf
		}
		st := &r.Steps[i]
		c, r0, n := col[id], first[id], span[id]
		start[r0][c] = st
		for k := 0; k < n; k++ {
			covered[r0+k][c] = true
		}
	}

	g := Grid{Width: width, Rows: make([]Row, rows)}
	for ri := 0; ri < rows; ri++ {
		g.Rows[ri].Ingredient = r.Ingredients[ingAt[order[ri]]]
		for c := 0; c < width; {
			if st := start[ri][c]; st != nil {
				g.Rows[ri].Cells = append(g.Rows[ri].Cells, Cell{
					Step:     st,
					Text:     st.Do,
					For:      st.For,
					RowSpan:  span[st.ID],
					ColSpan:  1,
					Vertical: verticalFor(st, span[st.ID]),
				})
				c++
				continue
			}
			if covered[ri][c] {
				c++ // a rowspan from an earlier row already owns this square
				continue
			}
			run := 0
			for c+run < width && !covered[ri][c+run] {
				run++
			}
			g.Rows[ri].Cells = append(g.Rows[ri].Cells, Cell{
				Gap: true, RowSpan: 1, ColSpan: run,
			})
			c += run
		}
	}
	return g, nil
}

// verticalFor decides whether a cell's label is rotated. Rotation is what lets
// a grid carry six or eight operations without running off the page, but it
// only pays when the cell is tall enough to hold the text and the text is
// short enough to read.
// The duration sits beside the label in a rotated cell, so it counts toward
// how tall that cell has to be.
func verticalFor(st *Step, rowSpan int) bool {
	if st.Vertical != nil {
		return *st.Vertical
	}
	n := len([]rune(strings.TrimSpace(st.Do)))
	if d := len([]rune(strings.TrimSpace(st.For))); d > n {
		n = d
	}
	return rowSpan >= 2 && n <= autoVerticalMax
}
