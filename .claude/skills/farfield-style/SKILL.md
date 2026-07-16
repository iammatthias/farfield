---
name: Farfield Styles
description: Applies the farfield "warm instrument" aesthetic — warm paper ground, soft white panels, ink text, NASA-red accent, rounded corners, hairline borders, soft shadows. Function-first but kind. Semantic HTML and vanilla CSS3 only; no frameworks, no build step, no external fonts. Use when building web pages, components, or artifacts for farfield apps.
---

# Farfield

## Overview

Function and form, with function first — but kind. The aesthetic keeps its
JPL/Braun lineage (warm paper, ink, one signal red, restrained type) and
softens the delivery: rounded corners, hairline borders, soft white panels on
a warm ground, gentle shadows for depth. Instrument, not bunker.

The single source of truth is `lib/theme/theme.css` — apps consume it via the
theme handlers. This document describes the system; when they disagree, the
stylesheet wins.

No CSS frameworks. No utility classes. No build step. Semantic HTML + CSS3.

## Core tokens

Four base colors; everything else derives from them with `color-mix()`, and a
`@supports (color: color(display-p3 1 1 1))` block upgrades the base colors
on wide-gamut displays. To iterate on the palette, touch only the base block
and its P3 override — never hardcode an rgba tint in a component.

| Token | sRGB | Usage |
|---|---|---|
| `--surface` | `#fcfbf8` (P3 warmer) | Page ground — bright warm white |
| `--panel` | `#ffffff` | Cards, inputs, bars |
| `--ink` | `#1f1b16` (P3 warmer) | All text — warm near-black, never pure #000 |
| `--accent` | `#e63d00` (P3 more vivid) | Signal red — status, selection, destructive; never body text |

| Derived | Recipe | Usage |
|---|---|---|
| `--hairline` | ink 13% | Borders and dividers |
| `--line-strong` | ink 36% | Hover/focus borders, link underlines |
| `--wash` | ink 4.5% | Subtle fills — code, hover rows |
| `--accent-soft` | accent 8% | Tinted fills — badges, errors, danger hover |
| `--accent-line` | accent 32% | Accent borders |
| `--ring` | accent 15% ×3px | Input focus ring |
| `--shadow-s` / `--shadow-m` | soft, low-alpha | Panel depth / modals & hover lift |
| `--r-s` / `--r-m` | `6px` / `10px` | Controls / panels & cards |

Depth comes from panel-on-ground plus a soft shadow — not from hard black
frames. Full-ink borders are reserved for moments of real emphasis.

### Visual hierarchy via opacity — not gray values

Use `opacity` on `--ink`, never a separate gray hex:

| Level | Opacity | Usage |
|---|---|---|
| Primary | 1.0 | Headings, body, links |
| Secondary | 0.6–0.7 | Labels, captions, meta |
| Tertiary | 0.5 | Timestamps, table headers |
| Muted | 0.3 | Placeholders, empty states |

## Typography

System fonts. Sans for everything human; **mono only for data** — slugs,
counts, CIDs, timestamps, telemetry. The old uppercase-tracked mono label is
retired; labels are quiet sentence-case sans (`0.8rem`, weight 500, opacity
~0.62).

| Role | Size | Weight |
|---|---|---|
| h1 | `clamp(1.5rem, 3vw, 2.1rem)` | 600 |
| h2 | `clamp(1.15rem, 2.2vw, 1.4rem)` | 600 |
| h3 | `1.05rem` | 600 |
| body | `1rem` | 400 |
| label / meta | `0.8rem` | 500, opacity 0.62 |
| `.mono` data | `0.8rem` mono | 400–500 |

Body line-height 1.55. Links underline with a softened decoration color that
darkens on hover — no color change, no inversion.

## Components

All in `lib/theme/theme.css`; extend with the same vocabulary.

- **Buttons** — panel background, hairline border, `--r-s`, whisper of
  shadow; hover darkens the border and lifts the shadow. `[type="submit"]` /
  `.primary` is solid ink. `.danger` is accent-tinted, filling softly red on
  hover. **Never invert to black-on-hover** — that was v1.
- **Inputs** — panel background, hairline border, `--r-s`; focus swaps the
  outline for a warm accent ring (`--ring`) plus a darker border.
- **Tables** — live inside `.table-wrap`: panel, `--r-m`, soft shadow;
  sentence-case headers at opacity 0.55; wash hover on rows.
- **Cards** (`.card`, `.rail-panel`, `.doc-card`) — panel, hairline, `--r-m`,
  `--shadow-s`; hover lifts to `--shadow-m` when clickable.
- **Badges** — soft pills (`border-radius: 999px`), tinted backgrounds:
  accent-tinted for live/active, wash for neutral. No borders.
- **Masthead `.bar`** — hairline bottom border (not a 2px rule), sans nav at
  opacity 0.65.
- **Segmented control `.seg`** — wash track, panel-colored active thumb with
  a soft shadow.
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

- No pure black (`#000`) or pure white page grounds — warm neutrals only.
- No gray hex values for hierarchy — `opacity` on ink only.
- No heavy black borders as the default component frame; hairlines + panels.
- No hover inversion (black fill / white text) on standard controls.
- No radius above `--r-m` except pills; no shadows darker than the tokens.
- No icon fonts or decorative SVG flourishes. Glyphs only when functional.
- No CSS frameworks, external font CDNs, or `@font-face`.
- No animations longer than 200ms. Easing: `ease-out`.
- No `<div>` where `<section>`, `<header>`, `<nav>`, `<article>`, `<aside>`,
  `<figure>`, or `<main>` fits.
- No margin on component roots — the parent spaces children.
