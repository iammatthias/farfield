# farfield recipes

Recipe entries in the `recipes` collection carry their structure in a fenced
` ```recipe ` block of YAML inside the entry `body`. That block is the single
source of truth for two views of the same recipe:

1. **The grid** — the tabular format from
   [Cooking for Engineers](https://www.cookingforengineers.com): ingredients
   down the left, operations bracketing them rightward, each operation spanning
   every row it absorbs. Read a row left to right and you see everything that
   happens to that ingredient; read a column top to bottom and you see one
   operation's whole input. Its left column carries the amount, the item and
   its note, so it *is* the ingredient list.
2. **The method** — the numbered steps, in full prose.

Both are derived from the same block, so they cannot disagree. Everything
outside the block is ordinary markdown and renders as it always has; a body may
contain several recipe blocks (a document with two independent sub-recipes gets
one block each).

## What changed, and what the site has to do

**All 37 entries in the `recipes` collection have already been converted in
production.** Their bodies no longer contain `**Yield:**`, a `## Ingredients`
table, or a `## Steps` list — that structure now lives inside the block. Any
site-side code that parsed those headings will find nothing and should be
deleted.

The work on the website is:

1. Lift ` ```recipe ` blocks out of the body **before** the markdown renderer
   sees them, in the same pass that resolves `blob://` and `series://`.
   Otherwise they render as a wall of YAML in a code block.
2. Parse each block, lay it out, render the grid and the method.
   §4 is a drop-in implementation; §5 is the markup it should produce.
3. Ship the CSS in §5. Four of its rules are load-bearing — the grid is
   unreadable without them.

Nothing else about the content API changed: entries are still
`GET /api/entries/{slug}`, bodies are still markdown, `blob://` and
`series://` still work the same way, and the per-record `cid` is still the
cache key (every recipe's `cid` changed in the migration, so a content-hash
build cache will correctly rebuild exactly these 37).

---

## 1. The block

````markdown
```recipe
yield: "4 servings"
time: "45 min, plus 4 h chilling"
source: "The Weekend Mixologist"
sourceURL: "https://example.com/post"

ingredients:
  - { id: cream, item: "Heavy cream", amount: "2 cups", note: "or half-and-half" }
  - { id: vanilla, item: "Vanilla bean", amount: "1, split" }
  - { item: "Salt", amount: "⅛ teaspoon" }
  - { id: yolks, item: "Egg yolks", amount: "5" }
  - { id: sugar, item: "Granulated sugar", amount: "½ cup" }

steps:
  - id: infuse
    in: [cream, vanilla, salt]
    do: "combine the cream"
    detail: "Cook over low heat until just hot, then steep off the heat and discard the bean."
  - id: ribbon
    in: [yolks, sugar]
    do: "beat until creamy"
  - in: [infuse, ribbon]
    do: "temper and combine"
    detail: "Stream a quarter of the hot cream into the yolks, then return it all to the pan."
  - do: "bake in a water bath"
  - do: "chill, then brûlée"
```
````

### Top level

| Field | Type | Meaning |
|---|---|---|
| `yield` | string | "4 servings". Shown in the meta strip. |
| `time` | string | Free text — "45 min, plus 4 h chilling". |
| `source` | string | Attribution label. |
| `sourceURL` | string | Makes `source` a link. Alone, it is its own label. |
| `notes` | string | Prose rendered under the method. |
| `ingredients` | list | Required, non-empty. |
| `steps` | list | Required, non-empty. |

### `ingredients[]`

| Field | Type | Meaning |
|---|---|---|
| `item` | string | **Required.** "Heavy cream". |
| `amount` | string | "2 cups", "306 g (2 cups plus 2 tbsp)". |
| `note` | string | "or half-and-half". Rendered under the item in the grid's ingredient column. |
| `id` | string | How steps refer to it. Defaults to a slug of `item`. |
| `group` | string | Labels a run of rows ("Dough", "Filling"). Shown in the ingredient column when it changes; it does not affect the tree. |

Default IDs are the lowercased item with non-alphanumerics collapsed to single
hyphens: `Heavy cream` → `heavy-cream`, `00 flour` → `00-flour`. A repeat gets
`-2`, `-3`, and so on, so two `Salt` rows are `salt` and `salt-2`.

### `steps[]`

| Field | Type | Meaning |
|---|---|---|
| `do` | string | **Required.** The terse grid label — "melt", "stir until coated". |
| `in` | string[] | Ingredient and step IDs this step consumes, in the order their rows should appear. |
| `detail` | string | Full prose for the numbered list. Empty falls back to `do`. |
| `id` | string | How later steps refer to this result. Defaults to `s1`, `s2`, … in order. |
| `phase` | string | Groups numbered steps under a subheading; numbering restarts per phase. |
| `prep` | bool | A step with no place in the grid — preheating, chilling a glass. Still numbered. |
| `title` | string | Captions this step's grid. Only meaningful on a finishing step. |
| `vertical` | bool | Overrides whether the grid label is rotated. |

`in` is what builds the grid. Omit it and the step continues from the previous
non-`prep` step, which is what a plain linear chain wants — only the steps where
ingredients *join* need to say so.

---

## 2. Validation

A block is only renderable if it satisfies all of these. Farfield rejects the
block and shows the error rather than drawing a misleading grid; your site
should do the same (or fall back to the method alone).

- `ingredients` and `steps` are both non-empty; every ingredient has an `item`;
  every step has a `do`.
- IDs are unique across ingredients *and* steps.
- Every `in` entry names a known ID, no step names itself, and no `in` repeats
  an ID.
- The first non-`prep` step has an `in` (there is nothing to continue from).
- A `prep` step has no `in`.
- No cycles.
- Every ingredient is consumed by some step — otherwise its row would sit in the
  grid with nothing to its right.

One more rule applies at **layout** time rather than parse time: within a single
grid, no step may feed two branches that later rejoin. Its rows would have to
appear in two places at once. Steps may still feed two *different* grids — that
is how a recipe that finishes two ways works, and the shared prefix is simply
drawn in both tables.

---

## 3. The layout algorithm

This is the part worth getting exactly right, because it is what makes the grid
read as nesting.

1. **Roots.** Every non-`prep` step that no other step consumes finishes a dish.
   Each root gets its own table. With more than one, caption each with the
   root's `title` (falling back to its `do`).
2. **Rows.** Walk the tree from the root depth-first, visiting each step's `in`
   in declared order and collecting ingredient leaves. That order *is* the row
   order — and it guarantees every subtree's ingredients land on consecutive
   rows, which is the property that lets an operation be a single `rowspan`.
3. **Spans.** A step's `rowSpan` is the number of ingredient leaves beneath it;
   its first row is the index of its first leaf.
4. **Columns.** An ingredient sits at column `-1` (the row header). A step sits
   at `1 + max(column of its inputs)`, so a step consuming only ingredients is
   column `0`. The grid's width is the root's column `+ 1`, and the root is
   always the rightmost cell.
5. **Fillers.** Place every step into a `rows × width` matrix and read it back
   row by row. Squares covered by a `rowspan` from an earlier row emit nothing.
   Runs of squares covered by nothing become a single filler cell with that
   `colSpan` — and the filler drops its right border, so the row visually runs
   into the bracket that claims it. (Cooking for Engineers calls these
   `righthide` cells; they are what makes the nesting legible.)
6. **Rotation.** A cell's label is rotated when its `rowSpan` is at least 2 and
   `do` is at most 34 characters, unless `vertical` says otherwise. Rotation is
   what lets six or eight operations fit across a page — and it is what keeps
   the grid narrow enough to swipe rather than zoom on a phone. A horizontal
   cell costs its full text width in every grid it appears in.

Worked example — the Rice Krispies Treats grid that farfield's tests pin
against the published original:

```
butter        │ melt ┐ stir until  ┐ stir until ┐ press into ┐ cool ┐ cut
marshmallows  │      ┘ melted      │ coated     │ 13x9 pan   │      │
rice cereal   │                    ┘            ┘            ┘      ┘
```

```yaml
ingredients:
  - { item: butter, amount: "3 Tbs. (43 g)" }
  - { item: marshmallows, amount: "10 oz. (280 g)" }
  - { item: "Rice Krispies cereal", amount: "6 cups (160 g)", id: cereal }
steps:
  - { id: melt,   in: [butter],             do: melt }
  - { id: soften, in: [melt, marshmallows], do: stir until melted }
  - { id: coat,   in: [soften, cereal],     do: stir until coated }
  - { do: press into 13x9-in. pan }
  - { do: cool }
  - { do: cut }
```

Width 6. Row 1 opens all six operations with spans 1, 2, 3, 3, 3, 3. Row 2 emits
one filler of `colSpan` 1 (under "melt"). Row 3 emits one filler of `colSpan` 2
(under "melt" and "stir until melted").

---

## 4. Client implementation

Recipe blocks must be lifted out **before** the markdown renderer sees the body
— otherwise they render as code blocks. Do it in the same pass as
`resolveBody()`.

Add a YAML parser (`yaml` or `js-yaml`), then drop this in as
`src/lib/recipe.ts`:

```ts
// farfield recipe blocks — parse, lay out, render.
// Mirrors github.com/iammatthias/farfield/lib/recipe.
import { parse as parseYAML } from "yaml";

export type Ingredient = {
  id: string; item: string; amount?: string; note?: string; group?: string;
};
export type Step = {
  id: string; in: string[]; do: string; detail?: string;
  phase?: string; prep?: boolean; title?: string; vertical?: boolean;
};
export type Recipe = {
  yield?: string; time?: string; source?: string; sourceURL?: string;
  notes?: string; ingredients: Ingredient[]; steps: Step[];
};
export type Cell = {
  step?: Step; text: string; rowSpan: number; colSpan: number;
  vertical: boolean; gap: boolean;
};
export type Row = { ingredient: Ingredient; cells: Cell[] };
export type Grid = { title: string; rows: Row[]; width: number };

const AUTO_VERTICAL_MAX = 34;

const slug = (s: string) =>
  s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");

/** Parse a block's YAML, fill in defaults, and validate. Throws on a block
 *  that cannot be laid out. */
export function parseRecipe(src: string): Recipe {
  const raw = parseYAML(src) ?? {};
  const rec: Recipe = {
    ...raw,
    ingredients: (raw.ingredients ?? []).map((i: any) => ({ ...i })),
    steps: (raw.steps ?? []).map((s: any) => ({ ...s, in: s.in ?? [] })),
  };
  if (!rec.ingredients.length) throw new Error("recipe: no ingredients");
  if (!rec.steps.length) throw new Error("recipe: no steps");

  const taken = new Set<string>();
  rec.ingredients.forEach((ing, i) => {
    if (!ing.item?.trim()) throw new Error(`recipe: ingredient ${i + 1} has no item`);
    let id = ing.id || slug(ing.item) || `i${i + 1}`;
    const base = id;
    for (let n = 2; taken.has(id); n++) id = `${base}-${n}`;
    taken.add(id);
    ing.id = id;
  });
  rec.steps.forEach((st, i) => {
    if (!st.do?.trim()) throw new Error(`recipe: step ${i + 1} has no \`do\``);
    const id = st.id || `s${i + 1}`;
    if (taken.has(id)) throw new Error(`recipe: duplicate id "${id}"`);
    taken.add(id);
    st.id = id;
  });

  // A step with no `in` continues from the previous grid-bearing step.
  let prev = "";
  for (const st of rec.steps) {
    if (st.prep) continue;
    if (!st.in.length) {
      if (!prev) throw new Error(`recipe: step "${st.id}" is first and needs an \`in\``);
      st.in = [prev];
    }
    prev = st.id;
  }

  const kind = new Map<string, "ingredient" | "step">();
  rec.ingredients.forEach((i) => kind.set(i.id, "ingredient"));
  rec.steps.forEach((s) => kind.set(s.id, "step"));
  const used = new Set<string>();
  for (const st of rec.steps) {
    if (st.prep && st.in.length) throw new Error(`recipe: step "${st.id}" is \`prep\` and cannot take an \`in\``);
    const seen = new Set<string>();
    for (const ref of st.in) {
      if (!kind.has(ref)) throw new Error(`recipe: step "${st.id}" refers to unknown id "${ref}"`);
      if (ref === st.id) throw new Error(`recipe: step "${st.id}" refers to itself`);
      if (seen.has(ref)) throw new Error(`recipe: step "${st.id}" lists "${ref}" twice`);
      seen.add(ref);
      used.add(ref);
    }
  }
  const orphans = rec.ingredients.filter((i) => !used.has(i.id)).map((i) => i.id);
  if (orphans.length) throw new Error(`recipe: no step uses ${orphans.sort().join(", ")}`);

  const stepAt = new Map(rec.steps.map((s, i) => [s.id, i]));
  const color = new Map<string, number>();
  const walk = (id: string) => {
    if (kind.get(id) === "ingredient") return;
    if (color.get(id) === 1) throw new Error(`recipe: steps form a cycle at "${id}"`);
    if (color.get(id) === 2) return;
    color.set(id, 1);
    rec.steps[stepAt.get(id)!].in.forEach(walk);
    color.set(id, 2);
  };
  rec.steps.forEach((s) => walk(s.id));
  return rec;
}

/** One grid per finishing step. */
export function layout(rec: Recipe): Grid[] {
  const stepAt = new Map(rec.steps.map((s, i) => [s.id, i]));
  const ingAt = new Map(rec.ingredients.map((g, i) => [g.id, i]));
  const consumed = new Set(rec.steps.flatMap((s) => s.in));
  const roots = rec.steps.filter((s) => !s.prep && !consumed.has(s.id));

  return roots.map((root) => {
    const order: string[] = [];
    const first = new Map<string, number>();
    const span = new Map<string, number>();
    const col = new Map<string, number>();
    const seen = new Set<string>();

    const walk = (id: string) => {
      if (seen.has(id))
        throw new Error(`recipe: "${id}" feeds two branches of the same grid`);
      seen.add(id);
      if (ingAt.has(id)) {
        first.set(id, order.length); span.set(id, 1); col.set(id, -1);
        order.push(id);
        return;
      }
      const st = rec.steps[stepAt.get(id)!];
      const start = order.length;
      let best = -1;
      for (const ref of st.in) { walk(ref); best = Math.max(best, col.get(ref)!); }
      first.set(id, start);
      span.set(id, order.length - start);
      col.set(id, best + 1);
    };
    walk(root.id);

    const rows = order.length;
    const width = col.get(root.id)! + 1;
    const start: (Step | undefined)[][] =
      Array.from({ length: rows }, () => Array(width).fill(undefined));
    const covered: boolean[][] =
      Array.from({ length: rows }, () => Array(width).fill(false));

    for (const id of seen) {
      if (!stepAt.has(id)) continue;
      const st = rec.steps[stepAt.get(id)!];
      const c = col.get(id)!, r0 = first.get(id)!, n = span.get(id)!;
      start[r0][c] = st;
      for (let k = 0; k < n; k++) covered[r0 + k][c] = true;
    }

    const out: Row[] = [];
    for (let r = 0; r < rows; r++) {
      const cells: Cell[] = [];
      for (let c = 0; c < width; ) {
        const st = start[r][c];
        if (st) {
          const n = span.get(st.id)!;
          cells.push({
            step: st, text: st.do, rowSpan: n, colSpan: 1, gap: false,
            vertical: st.vertical ?? (n >= 2 && st.do.trim().length <= AUTO_VERTICAL_MAX),
          });
          c++;
          continue;
        }
        if (covered[r][c]) { c++; continue; }
        let run = 0;
        while (c + run < width && !covered[r][c + run]) run++;
        cells.push({ text: "", rowSpan: 1, colSpan: run, vertical: false, gap: true });
        c += run;
      }
      out.push({ ingredient: rec.ingredients[ingAt.get(order[r])!], cells });
    }
    return {
      title: roots.length > 1 ? (root.title || root.do) : "",
      rows: out,
      width,
    };
  });
}

const RECIPE_FENCE = /^([ \t]*)(`{3,}|~{3,})[ \t]*recipe[ \t]*$/;

/** Lift recipe blocks out of a body before it reaches the markdown renderer.
 *  Returns the rewritten body and each block's YAML, in order. Substitute the
 *  rendered HTML back over each placeholder afterwards. */
export function extractRecipes(
  body: string,
  placeholder = (i: number) => `<!--ffrecipe${i}-->`,
): { body: string; blocks: string[] } {
  const lines = body.split("\n");
  const out: string[] = [];
  const blocks: string[] = [];
  for (let i = 0; i < lines.length; i++) {
    const m = RECIPE_FENCE.exec(lines[i]);
    if (!m) { out.push(lines[i]); continue; }
    const fence = m[2];
    const inner: string[] = [];
    let j = i + 1;
    for (; j < lines.length; j++) {
      const t = lines[j].trim();
      // CommonMark: same character, at least as long, nothing else.
      if (t.length >= fence.length && t.split(fence[0]).join("") === "") break;
      inner.push(lines[j]);
    }
    if (j >= lines.length) { out.push(lines[i]); continue; }  // unterminated
    out.push(placeholder(blocks.length));
    blocks.push(inner.join("\n"));
    i = j;
  }
  return { body: out.join("\n"), blocks };
}
```

Wiring it into an Astro page, alongside the existing `resolveBody`:

```ts
import { extractRecipes, parseRecipe, layout } from "../lib/recipe";

const entry = await getEntry(slug);
const { body, blocks } = extractRecipes(entry.body);
let html = render(await resolveBody(body));       // your existing markdown pass
blocks.forEach((src, i) => {
  let out: string;
  try {
    const rec = parseRecipe(src);
    out = renderRecipe(rec, layout(rec));         // your component or string builder
  } catch (e) {
    out = `<div class="ff-recipe-error"><p>${escapeHTML(String(e))}</p></div>`;
  }
  html = html.replaceAll(`<!--ffrecipe${i}-->`, out);
});
```

If you would rather render with a component than a string builder, `layout()`
returns plain data — map `grid.rows` to `<tr>` and `row.cells` to `<td>` with
`rowspan`/`colspan`, and you are done.

---

## 5. Markup contract

Farfield emits this structure; the CSS below is what styles it.

The grid's left column **is** the ingredient list — amount, item, note and
group all live in it — so nothing repeats it in a second table underneath. That
halves the page and makes the grid load-bearing rather than decorative.

```html
<div class="ff-recipe">
  <dl class="ff-recipe-meta"><div><dt>Yield</dt><dd>4 servings</dd></div></dl>

  <figure class="ff-recipe-grid-wrap">
    <figcaption>…</figcaption>                    <!-- only with 2+ grids -->
    <div class="ff-recipe-scroll" tabindex="0" role="region" aria-label="Recipe grid">
      <table class="ff-recipe-grid"><tbody>
        <tr>
          <th scope="row" class="ff-r-ing">
            <span class="ff-r-group">Dough</span>   <!-- only when it changes -->
            <span class="ff-r-line">
              <span class="ff-r-amt">2 cups</span><span class="ff-r-item">Heavy cream</span>
            </span>
            <span class="ff-r-note">or half-and-half</span>
          </th>
          <td rowspan="3" class="ff-r-op ff-r-vert"><span>combine the cream</span></td>
          <td colspan="2" class="ff-r-gap"></td>
        </tr>
      </tbody></table>
    </div>
  </figure>

  <div class="ff-recipe-detail">
    <h3 class="ff-recipe-h">Method</h3>
    <h4 class="ff-recipe-phase">Prep</h4>                  <!-- only with phases -->
    <ol class="ff-recipe-steps">
      <li><strong class="ff-r-do">combine the cream</strong> Cook over low heat…</li>
      <li>Shake well.</li>                        <!-- detail already led with the label -->
    </ol>
    <div class="ff-recipe-notes">…</div>
    <p class="ff-recipe-source">Source: <a href="…">…</a></p>
  </div>
</div>
```

A recipe whose shape no table can draw falls back to
`<ul class="ff-recipe-ingredients">` so the ingredients still reach the reader.

**Method rule.** Print `do` as a bold lead-in *only* when `detail` does not
already open with it. "shake well — Shake well." is a stutter; compare the two
folded to lowercase alphanumerics and drop the lead-in when `detail` starts
with `do`.

Four rules the CSS has to honour, whatever else you change:

- **`.ff-r-gap` drops its right border.** That join is the whole visual
  argument of the format — without it the grid reads as an arbitrary table.
- **`.ff-r-vert > span` uses `writing-mode: vertical-rl` plus
  `transform: rotate(180deg)`**, so rotated labels read bottom-to-top. Never
  put `overflow: hidden` on the table or the scroll box; it clips them.
- **`.ff-r-ing` is `position: sticky; left: 0`.** When a wide grid scrolls, the
  ingredient column stays put so a row never loses its label. In a
  `border-collapse: collapse` table a sticky cell loses its border — the border
  belongs to the table, not the cell — so draw its right edge with
  `box-shadow: inset -1px 0 0 …` and give it an opaque background.
- **`.ff-recipe-scroll` owns the horizontal overflow**, and the table is
  `width: auto` so it shrinks to fit rather than stretching. Most grids then
  need no scrolling at all even at 380px; the few that do scroll inside their
  own box and never push the page sideways. Don't `nowrap` the amount, or a
  long weight ("306 grams (2 cups plus 2 tbsp and 4 tsp)") will push through
  the next column.

### Reference CSS

Lifted verbatim from `lib/theme/theme.css`, which is what rendered the review
build. It depends only on the custom properties below — map them to your own
palette and the rest follows.

```css
:root {
  --panel: #ffffff;                     /* the grid's own surface           */
  --ink: #1f1b16;                       /* body text                        */
  --accent: #e63d00;                    /* captions, group tags             */
  --hairline: color-mix(in srgb, var(--ink) 13%, transparent);
  --line-strong: color-mix(in srgb, var(--ink) 36%, transparent);
  --accent-soft: color-mix(in srgb, var(--accent) 8%, transparent);
  --accent-line: color-mix(in srgb, var(--accent) 32%, transparent);
  --ring: 0 0 0 3px color-mix(in srgb, var(--accent) 15%, transparent);
  --r-s: 6px;
  --font-mono: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
}

@media (prefers-color-scheme: dark) {
  :root { --panel: #211f1b; --ink: #ece8e1; --accent: #ff5a1f; }
}

/* ── recipes ───────────────────────────────────────────────────────────── */
/* The tabular format from Cooking for Engineers: ingredients down the left,
   operations bracketing them rightward, each spanning the rows it absorbs.
   Read a row left to right and you see everything that happens to that
   ingredient; read a column top to bottom and you see one operation's input.

   The left column IS the ingredient list — amount, item and note — so nothing
   repeats it underneath. It pins to the left edge while the operations scroll,
   because the grid is wider than a phone and a row must never lose its label. */

.ff-recipe { margin: 1.75rem 0; }

.ff-recipe-meta {
	display: flex; flex-wrap: wrap; gap: 0.3rem 1.5rem;
	margin: 0 0 0.9rem; padding: 0;
}
.ff-recipe-meta div { display: flex; gap: 0.45rem; align-items: baseline; }
.ff-recipe-meta dt {
	font-size: 0.68rem; text-transform: uppercase; letter-spacing: 0.07em;
	opacity: 0.5;
}
.ff-recipe-meta dd { margin: 0; font-size: 0.88rem; }

.ff-recipe-grid-wrap { margin: 0 0 1.5rem; }
.ff-recipe-grid-wrap + .ff-recipe-grid-wrap { margin-top: -0.25rem; }
.ff-recipe-grid-wrap figcaption {
	font-size: 0.7rem; font-weight: 500; letter-spacing: 0.08em;
	text-transform: uppercase; color: var(--accent); margin-bottom: 0.45rem;
}

.ff-recipe-scroll {
	overflow-x: auto;
	-webkit-overflow-scrolling: touch;
	overscroll-behavior-x: contain;
	scrollbar-width: thin;
	border-radius: var(--r-s);
}
.ff-recipe-scroll:focus-visible { outline: none; box-shadow: var(--ring); }

/* display:table and width:auto both fight .prose table, which is the more
   specific selector — hence the .ff-recipe prefix. width:auto lets the grid
   shrink to its content instead of stretching to the column. Never
   overflow:hidden here: it clips the rotated labels. */
.ff-recipe .ff-recipe-grid {
	display: table; width: auto; border-collapse: collapse;
	font-size: 0.78rem; line-height: 1.25;
	background: var(--panel);
	border: 1px solid var(--line-strong);
}
.ff-recipe-grid th, .ff-recipe-grid td {
	border: 0;
	border-right: 1px solid var(--hairline);
	border-bottom: 1px solid var(--hairline);
	padding: 0.42rem 0.55rem;
	text-align: left; font-weight: 400; vertical-align: middle;
}
.ff-recipe-grid tr:last-child th, .ff-recipe-grid tr:last-child td {
	border-bottom: 0;
}

/* The ingredient column. A sticky cell in a collapsed table loses its border —
   the border belongs to the table, not the cell — so the right edge and the
   depth cue under the scrolling operations are drawn with shadows instead. */
.ff-recipe-grid .ff-r-ing {
	position: sticky; left: 0; z-index: 2;
	background: var(--panel);
	min-width: 9rem; max-width: 17rem;
	padding: 0.45rem 0.7rem;
	box-shadow: inset -1px 0 0 var(--line-strong),
		8px 0 10px -8px color-mix(in srgb, var(--ink) 30%, transparent);
}
.ff-recipe-grid .ff-r-line { display: block; text-wrap: pretty; }
.ff-recipe-grid .ff-r-amt {
	font-family: var(--font-mono); font-size: 0.9em; opacity: 0.6;
	margin-right: 0.4em;
}
.ff-recipe-grid .ff-r-note {
	display: block; font-size: 0.88em; opacity: 0.5; margin-top: 0.1rem;
}
.ff-recipe-grid .ff-r-group {
	display: block; font-size: 0.62rem; font-weight: 500;
	text-transform: uppercase; letter-spacing: 0.09em;
	color: var(--accent); margin-bottom: 0.25rem;
}

.ff-recipe-grid .ff-r-op { text-align: center; }

/* Rotation is what lets six or eight operations fit across a page, and what
   keeps the grid narrow enough to swipe rather than zoom. vertical-rl plus a
   half-turn reads bottom-to-top, the way a spanning bracket should. */
.ff-recipe-grid .ff-r-vert { padding: 0.55rem 0.3rem; }
.ff-recipe-grid .ff-r-vert > span {
	display: inline-block; white-space: nowrap;
	writing-mode: vertical-rl; transform: rotate(180deg);
}

/* A filler cell drops its right border so the row runs into the bracket that
   claims it. That join is the whole visual argument of the format. */
.ff-recipe-grid .ff-r-gap { border-right: 0; }

/* Fallback list for a recipe whose shape no table can draw. */
.ff-recipe-ingredients { list-style: none; padding: 0; margin: 0 0 1.5rem; }
.ff-recipe-ingredients li {
	padding: 0.35rem 0; border-bottom: 1px solid var(--hairline);
}
.ff-recipe-ingredients .ff-r-amt {
	font-family: var(--font-mono); font-size: 0.9em; opacity: 0.6;
	margin-right: 0.4em;
}
.ff-recipe-ingredients .ff-r-note { font-size: 0.88em; opacity: 0.5; }
.ff-recipe-ingredients .ff-r-grouprow {
	font-size: 0.65rem; font-weight: 500; text-transform: uppercase;
	letter-spacing: 0.09em; color: var(--accent);
	padding-top: 0.9rem; border-bottom: 0;
}

.ff-recipe-h {
	font-size: 0.7rem; font-weight: 500; letter-spacing: 0.08em;
	text-transform: uppercase; opacity: 0.5;
	margin: 1.75rem 0 0.7rem;
}
.ff-recipe-phase {
	font-size: 0.92rem; font-weight: 600; margin: 1.2rem 0 0.5rem;
}

.ff-recipe-steps { padding-left: 1.35rem; }
.ff-recipe-steps li { margin-top: 0.55rem; text-wrap: pretty; }
.ff-recipe-steps li::marker { font-variant-numeric: tabular-nums; opacity: 0.45; }
/* The grid label repeated as the step's lead-in, so the two halves are legible
   as the same recipe. */
.ff-recipe-steps .ff-r-do { font-weight: 600; }
.ff-recipe-steps .ff-r-do::after { content: " —"; font-weight: 400; opacity: 0.45; }

.ff-recipe-notes { margin-top: 1.2rem; font-size: 0.92rem; opacity: 0.8; }
.ff-recipe-source { margin-top: 1.2rem; font-size: 0.82rem; opacity: 0.65; }

.ff-recipe-error {
	border: 1px solid var(--accent-line); background: var(--accent-soft);
	border-radius: var(--r-s); padding: 0.75rem 0.9rem; margin: 1.5rem 0;
}
.ff-recipe-error p {
	margin: 0 0 0.5rem; color: var(--accent);
	font-family: var(--font-mono); font-size: 0.82rem;
}
.ff-recipe-error pre { margin: 0; font-size: 0.78rem; overflow-x: auto; }

@media (max-width: 40rem) {
	/* Tighten the grid rather than reflowing it: the rotated operations are
	   already narrow, so what buys the most room is a smaller ingredient
	   column and less padding. The column stays pinned, so the operations
	   scroll past a label that is always on screen. */
	.ff-recipe .ff-recipe-grid { font-size: 0.73rem; }
	.ff-recipe-grid .ff-r-ing {
		min-width: 7rem; max-width: 11rem; padding: 0.4rem 0.5rem;
	}
	.ff-recipe-grid th, .ff-recipe-grid td { padding: 0.35rem 0.4rem; }
	.ff-recipe-grid .ff-r-vert { padding: 0.45rem 0.25rem; }
	.ff-recipe-steps { padding-left: 1.15rem; }
}

@media (prefers-reduced-motion: no-preference) {
	.ff-recipe-scroll { scroll-behavior: smooth; }
}
```

The `.ff-recipe .ff-recipe-*` prefixes exist to out-specify a generic
`.prose table { display: block }` rule, which would strip the table layout the
rowspans depend on. If your prose styles do not do that, the bare classes are
enough.

---

## 6. Authoring notes

- **`do` is a label, not a sentence.** "melt", "stir until coated", "press into
  a 13x9-in. pan". Under ~34 characters it rotates and the column stays narrow;
  past that the column widens by the full width of the text. Put the prose in
  `detail`.
- **A column is an operation, not a sentence.** Prose recipes run 15–30
  numbered steps; a grid with 30 columns is unreadable. Fold the sentences that
  belong to one action into a single step's `detail` and give the step one
  label. Most recipes land at four to ten operations.
- **Mise en place is `prep: true`.** A step that dices and trims names half the
  ingredients without cooking any of them; letting it own them collapses the
  whole grid into one join at step one. Ingredients belong to the step that
  cooks them.
- **Only say `in` where something joins.** Steps that just continue the running
  result can omit it entirely.
- **Two ways?** Give each ending its own finishing step and a `title`. The
  shared prefix is drawn in both grids, so each table stands alone.

---

## 7. Verifying the port

- **Row order and spans.** The grid must reproduce the published Cooking for
  Engineers table for Rice Krispies Treats exactly — width 6, spans 1/2/3/3/3/3
  on the first row, a 1-wide filler on the second, a 2-wide filler on the
  third. The fixture is in §3. `lib/recipe`'s test suite pins this, and the
  TypeScript in §4 was diffed cell-for-cell against the Go across all 37
  production recipes with zero mismatches — so if your output disagrees, the
  port is wrong, not the spec.
- **No page-level horizontal scroll.** At a 380px viewport, check
  `document.documentElement.scrollWidth === clientWidth`. Most grids fit
  outright; a handful scroll inside `.ff-recipe-scroll` with the ingredient
  column pinned. If the *page* scrolls sideways, `width: auto` on the table or
  `overflow-x` on the wrapper is missing.
- **Nothing clipped.** Rotated labels overflow their cell if any ancestor sets
  `overflow: hidden`. Check the longest label in a two-row span.
- **Ingredients appear once.** If an ingredient name renders twice on one
  screen, a second ingredients table has crept back in.
- **Three recipes draw two grids** — goulash, summer chili, and brandy
  peppercorn sauce each finish two ways. Every other recipe draws one.
