---
name: Farfield Styles
description: Applies the farfield "engraved plate" aesthetic — an 18th-century French copperplate print. Deep cobalt ink on ivory laid paper, toile de Jouy monochrome, old-style serif, small-caps labels, rules instead of fills, madder red reserved for alarm. Function-first. Semantic HTML and vanilla CSS3 only; no frameworks, no build step, no external fonts. Use when building web pages, components, or artifacts for farfield apps.
---

# Farfield

## Overview

An 18th-century French copperplate print: deep cobalt ink on ivory laid paper,
toile de Jouy monochrome. Function still comes first — the engraving is a way
of drawing structure, not decoration. One ink at two densities: the settled
ink is the text, the fresh ink is the signal. Rules do the work that colour
used to.

The single source of truth is `lib/theme/theme.css` — apps consume it via the
theme handlers. This document describes the system; when they disagree, the
stylesheet wins.

No CSS frameworks. No utility classes. No build step. Semantic HTML + CSS3.

## Core tokens

Five base colors; everything else derives from them with `color-mix()`, and a
`@supports (color: color(display-p3 1 1 1))` block upgrades the base colors
on wide-gamut displays. To iterate on the palette, touch only the base block
and its P3 override — never hardcode an rgba tint in a component.

| Token | sRGB | Usage |
|---|---|---|
| `--surface` | `#f7f3e8` (P3 richer) | Page ground — ivory laid paper |
| `--panel` | `#fdfbf4` | Cards, inputs, bars — a fresh sheet over the ground |
| `--ink` | `#17265c` (P3 deeper) | All text — cobalt engraving ink, never black |
| `--accent` | `#2c53c8` (P3 more vivid) | Fresh cobalt — hover, selection, live status; never body text |
| `--alarm` | `#b3271e` | The rubricator's second plate — errors and destruction *only* |

A toile is monochrome, but a printed book of the same century is not: the
rubricator came back over the sheet in madder red for the things that must not
be missed. That is the entire licence for `--alarm`. Everything else — links,
focus, selection, live badges — is cobalt.

| Derived | Recipe | Usage |
|---|---|---|
| `--hairline` | ink 16% | Borders and dividers |
| `--line-strong` | ink 38% | Rules, hover/focus borders, link underlines |
| `--wash` | ink 5% | Subtle fills — code, hover rows |
| `--accent-soft` / `--accent-line` | accent 9% / 32% | Tinted fills / accent borders |
| `--alarm-soft` / `--alarm-line` | alarm 9% / 34% | Error fills / error borders |
| `--ring` | accent 16% ×3px | Input focus ring |
| `--shadow-s` / `--shadow-m` | faint, ink-tinted | Panel depth / modals & hover lift |
| `--r-s` / `--r-m` | `3px` / `5px` | Controls / panels & cards |

Blue ink at low alpha reads fainter than warm near-black did, so the rules
carry a little more weight than in v2. Depth comes from panel-on-ground plus a
whisper of shadow — ink does not cast one. Radii are tight: an engraved plate
has crisp corners, and the only real curve on the page is the badge cartouche.

The ground is flat ivory — a colour, not a texture. Do not simulate paper
grain, laid lines or tooth: nothing decorative earns its place unless it
carries information, and a drawn tooth carries none.

Dark mode inverts the plate into a cyanotype — pale ivory lines on a deep
Prussian ground (`#101833` / `#ece6d6`).

### Visual hierarchy via opacity — not gray values

Use `opacity` on `--ink`, never a separate gray hex:

| Level | Opacity | Usage |
|---|---|---|
| Primary | 1.0 | Headings, body, links |
| Secondary | 0.65–0.72 | Labels, captions, meta |
| Tertiary | 0.55 | Timestamps, table headers |
| Muted | 0.3 | Placeholders, empty states |

## Typography

System fonts. An old-style serif (`--font-serif`: `ui-serif`, Iowan Old Style,
Palatino, Georgia…) for everything human; **mono only for data** — slugs,
counts, CIDs, timestamps, telemetry. `--font-sans` still exists for the rare
app that needs it, but nothing in the shared theme reaches for it.

Engraved labels take `font-variant-caps: small-caps` with ~`0.05em` tracking —
**not** `text-transform: uppercase`. The markup stays sentence case and the
caps come from the face, so a slug or a proper noun keeps its real casing. It
applies to `.label`, field labels, `th`, `.section-head h2`, `.page-title`,
`.bar h1`, `.badge`, and the recipe captions. Small caps run optically small,
so these sizes sit a notch above their v2 values.

| Role | Size | Weight |
|---|---|---|
| h1 | `clamp(1.5rem, 3vw, 2.1rem)` | 600, tracking `0.005em` |
| h2 | `clamp(1.15rem, 2.2vw, 1.4rem)` | 600 |
| h3 | `1.05rem` | 600 |
| body | `1rem` serif | 400 |
| label / meta | `0.84rem` small caps | 500, opacity 0.72 |
| `.mono` data | `0.8rem` mono | 400–500 |

Body line-height 1.6 — a serif wants more air than a grotesque. Headings are
set open, not tight: the negative tracking a sans wants would close up the
serifs. Links underline with a softened decoration color and turn cobalt on
hover.

## Rules, not fills

Structure is drawn, not shaded. There is a hierarchy of rules:

- **Double rule** (`3px double var(--line-strong)`) — the masthead `.bar` and
  `hr`. The loudest lines on the page; a title-page device. Also the
  `blockquote` edge, in accent.
- **Hairline** — everything subordinate: `.section-head`, table rows, panel
  and card borders, the `th` row (which uses `--line-strong` to separate a
  legend from its entries).

Never introduce a third weight, and never use a double rule twice in the same
block — it stops meaning "this is the top".

## Components

All in `lib/theme/theme.css`; extend with the same vocabulary.

- **Buttons** — panel background, hairline border, `--r-s`, whisper of
  shadow; hover darkens the border and lifts the shadow. `[type="submit"]` /
  `.primary` is solid ink. `.danger` is **alarm**-tinted, filling softly red
  on hover. Never invert to black-on-hover.
- **Inputs** — panel background, hairline border, `--r-s`; focus swaps the
  outline for a cobalt ring (`--ring`) plus a darker border.
- **Tables** — live inside `.table-wrap`: panel, `--r-m`, faint shadow;
  small-caps headers over a `--line-strong` rule; wash hover on rows.
- **Cards** (`.card`, `.rail-panel`, `.doc-card`) — panel, hairline, `--r-m`,
  `--shadow-s`; hover lifts to `--shadow-m` when clickable.
- **Badges** — small-caps cartouches (`border-radius: 999px`) with a hairline
  border and a tinted fill: accent for live/active, wash for neutral.
- **Code** — `pre` is a plate of its own: wash ground inside a fine rule.
- **Segmented control `.seg`** — wash track, panel-colored active thumb.
- **Modals** — panel, `--r-m`, `--shadow-m`, hairline border, soft dim
  backdrop. Full-screen editors sit directly on `--surface` with panel bars.

## Layout: components position children

**A component must not add margin around itself.** Spacing is the parent's
job (gap with flex/grid; adjacent-sibling rules for prose rhythm).

```css
.row     { display: flex; flex-wrap: wrap; gap: 1rem; align-items: center; }
.cluster { display: flex; flex-wrap: wrap; gap: 0.5rem; }
.grid    { display: grid; gap: 1rem;
           grid-template-columns: repeat(auto-fit, minmax(min(18rem, 100%), 1fr)); }
```

Be dense where the user works. Prefer a main-column + sticky-rail split
(`.edit-layout`) over long single-column forms — wide screens should never
show a half-empty page. Hints go in placeholders, not paragraph rows, unless
the information must survive after the field is filled.

Container: `max-width: 72rem`. Prose measure: `42rem` (`.prose`), but let
document previews fill their panel.

## Responsive

Mobile-first; intrinsic responsiveness (`auto-fit`, `clamp()`, `min()`,
`flex-wrap`) over breakpoints. Rails stack above the main column on narrow
screens (`order: -1`). Form controls stay ≥16px on touch screens (iOS zoom).
Every farfield UI must work on a phone.

## What NOT to do

- No pure black (`#000`) or pure white page grounds — cobalt ink on ivory.
- No third hue. Cobalt carries every signal; madder red means error or
  destruction and nothing else. Never use `--alarm` for emphasis or decoration.
- No gray hex values for hierarchy — `opacity` on ink only.
- No `text-transform: uppercase` for labels — `font-variant-caps: small-caps`.
- No heavy black borders as the default component frame; hairlines + panels.
- No hover inversion (black fill / white text) on standard controls.
- No radius above `--r-m` except pills; no shadows darker than the tokens.
- No icon fonts or decorative SVG flourishes — the engraving is in the rules
  and the type, not in ornament. Glyphs only when functional.
- No CSS frameworks, external font CDNs, or `@font-face`.
- No animations longer than 200ms. Easing: `ease-out`.
- No `<div>` where `<section>`, `<header>`, `<nav>`, `<article>`, `<aside>`,
  `<figure>`, or `<main>` fits.
- No margin on component roots — the parent spaces children.
