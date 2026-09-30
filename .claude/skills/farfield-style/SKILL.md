---
name: farfield-style
description: Applies the farfield product-UI style (v5) — the precision half of docs/BRAND.md. An instrument on paper: structure from space and full-width horizon rules, not boxes; borders only for an input's edge and focus; shadows only on things that float. Paper/Deep Space (dark accent Horizon orange), Inter for interface, Newsreader only inside documents, IBM Plex Mono for technical readouts. Semantic HTML and vanilla CSS3; no frameworks, no build step, no font CDN. Use when building or changing any farfield app page, component, or artifact.
---

# Farfield product UI — v5

## Overview

The product half of `docs/BRAND.md` ("distance made legible"): **a scientific
instrument sitting at the edge of something enormous — an instrument on
paper.** The marketing world (apex, the 404 plate, posters) carries the
wonder; the app consoles carry the precision, and stay quieter than the world
around them.

The single source of truth is `lib/theme/theme.css` (v5). Apps consume it via
the theme handlers and the shared shell in `lib/web/templates/layout.html`.
**The component gallery renders every piece of it:**

    go run ./lib/editor/cmd/preview      # http://localhost:7070

Look there first; when this document and the stylesheet disagree, the
stylesheet wins. For brand/marketing surfaces read `docs/BRAND.md` instead.

No CSS frameworks. No utility soup. No build step. Semantic HTML + CSS3.
Fonts are vendored into the stylesheet as data URIs — no CDN.

## The three rules

1. **Space groups; lines separate.** Grouping comes from spacing. Separation
   comes from horizontal hairlines that run the width of their column — the
   top bar's rule runs the full window, like a horizon. No card plates, no
   framed panels, no outlined pills.
2. **Borders mean something.** An input's edge (one line under the value) and
   keyboard focus. A data grid or a game board may keep cell lines. Nothing
   else gets a border.
3. **Only floating things lift.** Menus, popovers, the fleet switcher, the
   editor's selection bar and `/` menu, toasts and the save pill get
   `--float` + `--shadow`. Nothing that sits on the page has a shadow.

## Tokens

Light and dark are both first-class: dark follows `prefers-color-scheme`, and
`data-theme` on `<html>` wins in either direction.

| Token | Light | Dark | Use |
|---|---|---|---|
| `--paper` | `#f3e5d1` | `#0e222d` | page ground |
| `--paper-2` | `#ecdcc5` | `#132f3d` | image wells, the rare fill |
| `--ink` / `--ink-2` / `--ink-3` | Deep Space / Terrain / lighter | Paper / … | text, secondary, tertiary |
| `--rule` / `--rule-strong` | ink 12% / 30% | | hairlines / legends, input edges |
| `--wash` | ink ~4.5% | | hover rows, textareas, code |
| `--accent` / `--accent-ink` | Farfield Blue | Horizon | primary action, focus in fields |
| `--signal` | Horizon | Horizon | the mark's dot, focus rings — rare |
| `--good` `--warn` `--bad` (`--bad-soft`) | | | status and destruction only |
| `--float` / `--shadow` | | | floating surfaces only |
| `--font-ui` `--font-doc` `--font-mono` | Inter / Newsreader / Plex Mono | | |
| `--s-1`…`--s-9` | 4 8 12 16 24 32 48 72 112px | | spacing scale |
| `--r-s` / `--r-m` | 3px / 6px | | controls / floating surfaces |

v4 names (`--surface --panel --hairline --line-strong --alarm --font-sans
--font-serif --shadow-s …`) still resolve as aliases so older app CSS keeps
working — use the v5 names in anything new. `--shadow-s` is now `none`.

**Hierarchy is `--ink-2` / `--ink-3`, never opacity.**

## Type

- **Inter** — the interface: body 15px, nav, buttons, forms, labels, screen
  titles. Headings are **500**, never bold.
- **Newsreader** — only inside writing: `.prose`, the editor's document, the
  `.title-input`, an `.empty .line`. Headlines 400; H2 500; never bold.
- **IBM Plex Mono** — technical readouts only: `.tech` (tiny uppercase,
  tracked), `th`, timestamps, IDs, ports, counts. Not for names or prose.

## Components (all in theme.css, all in the gallery)

- **Top bar** `.bar` — sticky, full-bleed, 56px, one hairline under it;
  `.mark` wordmark with the signal dot; nav links are words, the current one
  underlined in ink; fleet switcher is a floating `.menu`.
- **Sections** `.section-head` (title + horizon), `.page-title`, `.back`,
  `.filter-bar` (words; current = ink underline).
- **Buttons** — default is a quiet text button (wash on hover).
  `type=submit` / `.primary` = accent fill, one per view. `.danger` = red text.
  `.linklike` for verbs that must read as links.
- **Fields** — `input`/`select` are an underline; `textarea` is a soft wash;
  file input is a wash well. `.field` = label + control (+ `.hint`).
- **Lists** — `.list > .item` rows on hairlines (`.title`, `.sub`, `.side`).
- **Tables** — bare, inside `.table-wrap`; mono legend over `--rule-strong`;
  `td.actions` verbs read as links; `table.stack` + `data-label` for phones.
- **Status** — `.status` / `.badge`: a dot and a word (`.live .draft .warn
  .bad .signal`). Tags (`.tags`) are `#words`.
- **Readouts** — `.stats > .stat > .value` (light 300 numerals) + `.tech`.
- **Notices** — `.notice` / `.error`: a short rule on the left, never a box.
- **Empty states** — `.empty` with a Newsreader italic `.line`.
- **Cards** — `.card` is a group set off by a top rule, not a plate.
- **Floating** — `.menu`, `.chip-pop`, `.toast`, `.doc-pill`, `.modal .card`.

## The document page (content, feed, scrap)

The page is the document: `.doc-page` column at `--measure`, a Newsreader
`.title-input`, the `.meta-band` (chips are plain words that open popovers;
state is dot + word; word count in mono), then the WebAssembly editor from
`lib/editor` (`textarea[data-editor]` + host.css/host.js/mount.js), which
draws the text itself with a quiet sticky strip, a floating selection bar and
a `/` block menu. Save lives in the corner `.doc-pill`.

## Layout and responsive

`.container` max 72rem with `--gutter` (32px, 16px on phones). Components
don't add outer margin — the parent spaces children with gap. Intrinsic
layouts (`auto-fit`, `clamp()`, `min()`, `flex-wrap`) over breakpoints. Every
page must work at ~390px: no sideways page scroll, tap targets ≥ 40px,
inputs ≥ 16px on touch.

## What NOT to do

- No boxes: no bordered cards, panels, pills, tiles or framed wells.
- No shadow on anything that doesn't float.
- No gray hex or opacity for hierarchy — `--ink-2` / `--ink-3`.
- No bold headings; no serif in chrome; no mono for names or prose.
- No third accent; red means error or destruction only; orange stays rare.
- No pure white/black grounds, no grain in the interface, no glows,
  glassmorphism, radii > 6px (except the status dot), animations > 200ms.
- No CSS frameworks, font CDNs, or icon fonts.
