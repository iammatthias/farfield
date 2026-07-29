package recipe

import (
	"html/template"
	"strconv"
	"strings"
)

// Renderer turns a parsed recipe into HTML. The zero value works and escapes
// all prose; a host that wants emphasis and links inside step details supplies
// Inline.
type Renderer struct {
	// Inline renders a prose fragment as inline markdown. nil escapes it.
	Inline func(string) template.HTML
}

// Render is the whole recipe: the grid (or grids) first, then the method.
//
// The grid's left column carries the amount, the item and its note, so it *is*
// the ingredient list — there is no second table repeating it underneath. Both
// halves read the same parsed data, so editing one edits the other.
func (rr *Renderer) Render(rec *Recipe) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="ff-recipe">`)
	rr.meta(&b, rec)

	grids, err := rec.Layout()
	if err == nil {
		for _, g := range grids {
			rr.grid(&b, g)
		}
	} else {
		// No table can express this shape, but the ingredients still have to
		// reach the reader.
		rr.ingredientList(&b, rec)
	}

	b.WriteString(`<div class="ff-recipe-detail">`)
	rr.steps(&b, rec)
	rr.footer(&b, rec)
	b.WriteString(`</div></div>`)
	return template.HTML(b.String())
}

// Render renders with the default escaping-only prose treatment.
func Render(rec *Recipe) template.HTML { return (&Renderer{}).Render(rec) }

func (rr *Renderer) prose(s string) string {
	if rr.Inline != nil {
		return string(rr.Inline(s))
	}
	return template.HTMLEscapeString(s)
}

func esc(s string) string { return template.HTMLEscapeString(s) }

// leadsWith reports whether detail opens with the label, ignoring case,
// punctuation and spacing — the test for whether repeating the label as a
// lead-in would only stutter.
func leadsWith(detail, label string) bool {
	fold := func(s string) string {
		var b strings.Builder
		space := false
		for _, r := range strings.ToLower(s) {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				if space && b.Len() > 0 {
					b.WriteByte(' ')
				}
				space = false
				b.WriteRune(r)
			default:
				space = true
			}
		}
		return b.String()
	}
	l := fold(label)
	return l != "" && strings.HasPrefix(fold(detail), l)
}

func (rr *Renderer) meta(b *strings.Builder, rec *Recipe) {
	if rec.Yield == "" && rec.Time == "" {
		return
	}
	b.WriteString(`<dl class="ff-recipe-meta">`)
	if rec.Yield != "" {
		b.WriteString(`<div><dt>Yield</dt><dd>` + esc(rec.Yield) + `</dd></div>`)
	}
	if rec.Time != "" {
		b.WriteString(`<div><dt>Time</dt><dd>` + esc(rec.Time) + `</dd></div>`)
	}
	b.WriteString(`</dl>`)
}

// ingredientCell writes the grid's left column: the amount, the item, its note
// and — when it changes — the group it belongs to. This cell is the recipe's
// only ingredient list, so everything a cook needs before starting has to be
// in it.
func (rr *Renderer) ingredientCell(b *strings.Builder, ing Ingredient, group string) {
	b.WriteString(`<th scope="row" class="ff-r-ing">`)
	if ing.Group != "" && ing.Group != group {
		b.WriteString(`<span class="ff-r-group">` + esc(ing.Group) + `</span>`)
	}
	b.WriteString(`<span class="ff-r-line">`)
	if ing.Amount != "" {
		b.WriteString(`<span class="ff-r-amt">` + esc(ing.Amount) + `</span>`)
	}
	b.WriteString(`<span class="ff-r-item">` + esc(ing.Item) + `</span></span>`)
	if ing.Note != "" {
		b.WriteString(`<span class="ff-r-note">` + rr.prose(ing.Note) + `</span>`)
	}
	b.WriteString(`</th>`)
}

// grid writes one tabular-format table. The wrapper scrolls rather than
// overflowing, which is the whole story on a phone — a six-operation grid is
// wider than any handset. The ingredient column pins to the left edge while
// the operations scroll under it, so a row never loses its label.
func (rr *Renderer) grid(b *strings.Builder, g Grid) {
	b.WriteString(`<figure class="ff-recipe-grid-wrap">`)
	if g.Title != "" {
		b.WriteString(`<figcaption>` + esc(g.Title) + `</figcaption>`)
	}
	b.WriteString(`<div class="ff-recipe-scroll" tabindex="0" role="region" aria-label="Recipe grid">`)
	b.WriteString(`<table class="ff-recipe-grid"><tbody>`)
	group := ""
	for _, row := range g.Rows {
		b.WriteString(`<tr>`)
		rr.ingredientCell(b, row.Ingredient, group)
		group = row.Ingredient.Group
		for _, c := range row.Cells {
			b.WriteString(`<td`)
			if c.RowSpan > 1 {
				b.WriteString(` rowspan="` + strconv.Itoa(c.RowSpan) + `"`)
			}
			if c.ColSpan > 1 {
				b.WriteString(` colspan="` + strconv.Itoa(c.ColSpan) + `"`)
			}
			switch {
			case c.Gap:
				b.WriteString(` class="ff-r-gap"></td>`)
			case c.Vertical:
				b.WriteString(` class="ff-r-op ff-r-vert"><span>` + esc(c.Text) + `</span></td>`)
			default:
				b.WriteString(` class="ff-r-op"><span>` + esc(c.Text) + `</span></td>`)
			}
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></div></figure>`)
}

// ingredientList is the fallback for a recipe whose steps no table can draw.
// It keeps the ingredients visible rather than leaving only the method.
func (rr *Renderer) ingredientList(b *strings.Builder, rec *Recipe) {
	b.WriteString(`<h3 class="ff-recipe-h">Ingredients</h3><ul class="ff-recipe-ingredients">`)
	group := ""
	for _, ing := range rec.Ingredients {
		if ing.Group != "" && ing.Group != group {
			group = ing.Group
			b.WriteString(`<li class="ff-r-grouprow">` + esc(group) + `</li>`)
		}
		b.WriteString(`<li><span class="ff-r-line">`)
		if ing.Amount != "" {
			b.WriteString(`<span class="ff-r-amt">` + esc(ing.Amount) + `</span>`)
		}
		b.WriteString(`<span class="ff-r-item">` + esc(ing.Item) + `</span></span>`)
		if ing.Note != "" {
			b.WriteString(`<span class="ff-r-note">` + rr.prose(ing.Note) + `</span>`)
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul>`)
}

// steps writes the numbered list. Numbering restarts inside each phase, the
// way the prose recipes it replaces were written.
func (rr *Renderer) steps(b *strings.Builder, rec *Recipe) {
	b.WriteString(`<h3 class="ff-recipe-h">Method</h3>`)
	phase := ""
	open := false
	for _, st := range rec.Steps {
		if st.Phase != phase {
			if open {
				b.WriteString(`</ol>`)
				open = false
			}
			phase = st.Phase
			if phase != "" {
				b.WriteString(`<h4 class="ff-recipe-phase">` + esc(phase) + `</h4>`)
			}
		}
		if !open {
			b.WriteString(`<ol class="ff-recipe-steps">`)
			open = true
		}
		b.WriteString(`<li>`)
		switch {
		case st.Detail == "":
			b.WriteString(rr.prose(st.Do))
		case leadsWith(st.Detail, st.Do):
			// The detail already opens with the label — "shake well" followed
			// by "Shake well." reads as a stutter, so the prose stands alone.
			b.WriteString(rr.prose(st.Detail))
		default:
			b.WriteString(`<strong class="ff-r-do">` + esc(st.Do) + `</strong> ` + rr.prose(st.Detail))
		}
		b.WriteString(`</li>`)
	}
	if open {
		b.WriteString(`</ol>`)
	}
}

func (rr *Renderer) footer(b *strings.Builder, rec *Recipe) {
	if rec.Notes != "" {
		b.WriteString(`<div class="ff-recipe-notes">` + rr.prose(rec.Notes) + `</div>`)
	}
	if rec.Source == "" && rec.SourceURL == "" {
		return
	}
	label := rec.Source
	if label == "" {
		label = rec.SourceURL
	}
	b.WriteString(`<p class="ff-recipe-source">Source: `)
	if rec.SourceURL != "" {
		b.WriteString(`<a href="` + esc(rec.SourceURL) + `" rel="noopener">` + esc(label) + `</a>`)
	} else {
		b.WriteString(esc(label))
	}
	b.WriteString(`</p>`)
}
