---
name: farfield-style
description: Applies the farfield product-UI style — the precision half of docs/BRAND.md. Paper ground with Deep Space text (dark mode inverts to Deep Space + Paper, Horizon-orange accent), Inter for interface, Newsreader only for rendered documents, IBM Plex Mono for data and tiny uppercase labels, thin borders, compact radii, orange rare. Semantic HTML and vanilla CSS3 only; no frameworks, no build step, no font CDN. Use when building web pages, components, or artifacts for farfield app consoles.
---

# Farfield product UI

## Overview

The product half of `docs/BRAND.md` ("distance made legible"): **a scientific
instrument sitting at the edge of something enormous.** The marketing world
(apex, the 404 plate, posters) carries the wonder; the app consoles carry the
precision. The interface stays cleaner than the environment around it — flat
fills, thin borders, generous internal spacing, one accent used rarely.

The single source of truth is `lib/theme/theme.css` (v4 "far field") — apps
consume it via the theme handlers. This document describes the system; when
they disagree, the stylesheet wins. For brand/marketing surfaces read
`docs/BRAND.md` instead.

No CSS frameworks. No utility classes. No build step. Semantic HTML + CSS3.
Fonts are vendored into the stylesheet as data URIs — no CDN.

## Core tokens

Brand tokens; light and dark are both first-class (`prefers-color-scheme`).
`--ink` means "text": Deep Space on Paper by day, Paper on Deep Space by
night. Derived tints flow from these via `color-mix()` — never hardcode an
rgba tint in a component.

| Token | Light | Dark | Usage |
|---|---|---|---|
| `--surface` | `#f3e5d1` Paper | `#0e222d` Deep Space | Page ground |
| `--panel` | `#eee0cc` | `#132f3d` | Cards, bars, raised surfaces |
| `--ink` | `#0e222d` | `#f3e5d1` | All text |
| `--accent` | `#0d3560` Farfield Blue | `#e59f67` Horizon | Primary actions, live states, focus |
| `--accent-contrast` | `#f3e5d1` | `#0e222d` | Text on accent |
| `--alarm` | `#b3271e` | `#f2907d` | Errors and destruction **only** |

| Derived | Recipe | Usage |
|---|---|---|
| `--hairline` | ink 16% (14% dark) | Borders and dividers |
| `--line-strong` | ink 38% | The masthead rule, hover borders |
| `--wash` | ink 4–5% | Subtle fills — code, hover rows |
| `--accent-soft` / `--accent-line` | accent 9% / 32% | Tinted fills / accent borders |
| `--alarm-soft` / `--alarm-line` | alarm 9% / 34% | Error fills / error borders |
| `--shadow-s` / `--shadow-m` | faintest lift | Minimal — depth comes from borders |
| `--r-s` / `--r-m` | `3px` / `6px` | Controls / panels; buttons use 4px |

Orange behaves like a signal, not decoration: in dark mode the accent IS
Horizon orange — primary buttons, live badges, focus rings. Keep it rare.
`--alarm` red means something broke; never use it for emphasis.

### Hierarchy via opacity — not gray values

`opacity` on `--ink`, never a separate gray hex: primary 1.0, labels/meta
0.65–0.72, timestamps/table heads 0.55–0.65, placeholders 0.3.

## Typography

Three vendored families, each with one role (BRAND.md §3):

- **Inter** (`--font-sans`) — the interface: body, nav, buttons, forms,
  labels, screen titles. Weights 400/500, 600 sparingly.
- **Newsreader** (`--font-serif`) — rendered documents only: `.prose`,
  `.doc-rich`, `.title-input`. The chrome around a document is interface; the
  document itself is writing. Serif headings stay 400–500, never bold,
  letter-spacing −0.02em.
- **IBM Plex Mono** (`--font-mono`) — observation: data values, slugs, CIDs,
  timestamps, telemetry, and tiny uppercase labels (`th`, `.badge`,
  stacked-table captions) at ~0.66–0.72rem with 0.07–0.08em tracking.

The wordmark (`.bar h1`) is understated: Inter 500, slightly tight. No
small caps anywhere — labels are Inter 500 sentence case; only the tiny
technical mono labels uppercase.

| Role | Face | Size | Weight |
|---|---|---|---|
| Screen title (h1) | Inter | `clamp(1.4rem, 2.6vw, 1.75rem)` | 600 |
| Section (h2/h3) | Inter | 1.25rem / 1rem | 600 |
| Body | Inter | 1rem, lh 1.55 | 400 |
| Label / field name | Inter | 0.8rem, opacity 0.7 | 500 |
| Column head / badge | Plex Mono, uppercase | 0.66–0.7rem, tracked | 500 |
| Document prose | Newsreader | 1.05rem, lh 1.6 | 400 |

## Rules and lines

Fine lines, few weights (BRAND.md §16): the masthead `.bar` closes with the
page's one **2px** rule — its horizon — and every rule below it is a **1px**
hairline (section heads, table rows, panel borders, `hr`). Never heavier;
no double rules; horizontal rules echo horizons.

## Components

All in `lib/theme/theme.css`; extend with the same vocabulary.

- **Buttons** — compact and deliberate, radius 4px. Default is the quiet
  secondary: transparent fill, thin ink-alpha border, wash on hover.
  `[type="submit"]` / `.primary` is solid `--accent` with
  `--accent-contrast` text. `.danger` is alarm-tinted, filling softly on
  hover. No pill CTAs.
- **Inputs** — instrument-like: transparent fill, thin border, radius 4px;
  focus is a visible 2px `--accent` outline (offset 2px), not a glow.
  ≥16px on touch screens (iOS zoom).
- **Tables** — `.table-wrap` panel; mono uppercase column heads over a
  `--line-strong` rule; wash hover. `.stack` + `data-label` collapses to
  labelled cards on phones.
- **Cards** (`.card`, `.rail-panel`, `.doc-card`) — panel, hairline,
  `--r-m`, whisper shadow.
- **Badges** — mono uppercase readout chips in a quiet capsule; accent tint
  for live, wash for neutral.
- **Code** — `pre` on wash inside a fine rule; mono.
- **Modals / editor** — panel surfaces, hairline borders, `--r-m`; the
  full-screen editor sits on `--surface` with panel bars.

## Layout: components position children

A component must not add margin around itself — spacing is the parent's job
(gap with flex/grid; adjacent-sibling rules for prose rhythm). Container
`max-width: 72rem`; prose measure `42rem`. Be dense where the user works:
main-column + sticky-rail (`.edit-layout`) over long single-column forms.

## Responsive

Mobile-first, intrinsic (`auto-fit`, `clamp()`, `min()`, `flex-wrap`) over
breakpoints. Rails stack above the main column on phones (`order: -1`).
Every farfield UI must work on a phone.

## What NOT to do

- No pure white or pure black grounds — Paper and Deep Space.
- No third accent. Accent carries every signal; alarm red means error or
  destruction and nothing else.
- No gray hex values for hierarchy — opacity on ink only.
- No small caps, no `text-transform: uppercase` outside the tiny mono labels.
- No serif in the chrome — Newsreader belongs to rendered documents.
- No excessive monospace: mono is for values and tiny labels, or the brand
  turns into terminal UI.
- No heavy borders, big radii (>6px except capsules), glows, glassmorphism,
  or shadows darker than the tokens.
- No grain or texture in the working interface — that belongs to the brand
  world outside.
- No icon fonts or decorative SVG; glyphs only when functional.
- No CSS frameworks, external font CDNs, or animations over 200ms
  (ease-out only).
- No `<div>` where `<section>`, `<header>`, `<nav>`, `<article>`, `<aside>`,
  `<figure>`, or `<main>` fits.
