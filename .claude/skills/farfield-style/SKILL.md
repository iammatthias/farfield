---
name: Farfield Styles
description: Applies a Braun × JPL × vintage NASA aesthetic — warm off-white surface, black foreground, sharp ruled dividers, function-first typography. Semantic HTML and vanilla CSS3 only; no frameworks, no build step, no external fonts. Use when building web pages, components, or artifacts that should read as a precision instrument or mission-control document rather than decorative web design.
---

# Farfield

## Overview

Function and form, with function first. The aesthetic borrows from Dieter Rams (Braun), JPL/NASA technical documentation, and Swiss typography — high-contrast monochrome, generous whitespace, ruled dividers, restrained type. Nothing decorative earns its place unless it carries information.

No CSS frameworks. No utility classes. No build step. Semantic HTML + CSS3.

## Core palette

| Token | Value | Usage |
|---|---|---|
| `--surface` | `#fafaf7` | Page background — slightly warm, paper-like |
| `--ink` | `#0a0a0a` | All text, primary rules |
| `--rule` | `1px solid #0a0a0a` | Primary dividers, frames |
| `--hairline` | `1px solid rgba(10, 10, 10, 0.15)` | Secondary dividers |
| `--accent` | `#d93a00` | NASA red — status, callouts; never body text |

No gradients. No shadows. No additional colors.

### Visual hierarchy via opacity — not gray values

Use `opacity` on `--ink`, never a separate gray:

| Level | Opacity | Usage |
|---|---|---|
| Primary | 1.0 | Headings, body, links |
| Secondary | 0.7 | Labels, captions, meta |
| Tertiary | 0.5 | Timestamps, table headers, footnotes |
| Muted | 0.3 | Placeholders, disabled, empty states |

**Do NOT use `color: #666` or `color: gray` for secondary text.** Always opacity on the foreground.

## Typography

System fonts. The browser already has good ones.

```css
:root {
  --font-sans: ui-sans-serif, system-ui, -apple-system, "Helvetica Neue", Helvetica, Arial, sans-serif;
  --font-mono: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
}
body { font-family: var(--font-sans); }
code, kbd, samp, pre, .data, .label { font-family: var(--font-mono); }
```

Mono is for numerals, codes, identifiers, telemetry — anywhere alignment matters. Sans for prose.

### Type scale

Body 16px / line-height 1.5. Headings fluid via `clamp()`.

| Role | Size | Weight | Notes |
|---|---|---|---|
| h1 | `clamp(2rem, 4vw, 3rem)` | 500 | `letter-spacing: -0.01em` |
| h2 | `clamp(1.5rem, 3vw, 2rem)` | 500 | |
| h3 | `1.25rem` | 500 | |
| body | `1rem` | 400 | |
| small, meta | `0.875rem` | 400 | |
| `.label` | `0.75rem` | 500 | mono, uppercase, `letter-spacing: 0.08em` |

The mono `.label` (uppercase, tracked, often paired with a hairline) is the JPL/instrument-panel signature. Use sparingly — section headers, data tags, status.

## Layout: components position children

**A component must not add margin around itself.** Spacing is the parent's job. This keeps components portable and composable (per Brad Woods / Braid).

### Consistent gaps — `gap` with flex or grid

```css
.stack    { display: flex; flex-direction: column; gap: 1rem; }
.row      { display: flex; flex-wrap: wrap; gap: 1rem; align-items: center; }
.grid     { display: grid; gap: 1rem;
            grid-template-columns: repeat(auto-fit, minmax(min(20rem, 100%), 1fr)); }
.cluster  { display: flex; flex-wrap: wrap; gap: 0.5rem; }
```

The `auto-fit` + `minmax` grid is responsive with zero media queries.

### Inconsistent gaps — adjacent sibling combinator

For prose where the gap depends on what's next to what:

```css
.prose > * + *   { margin-top: 1rem; }       /* default rhythm */
.prose > * + h2  { margin-top: 2.5rem; }     /* extra air before h2 */
.prose > h2 + *  { margin-top: 0.5rem; }     /* tighten after h2 */
```

Avoids margin collapse. Reads top-to-bottom like the document does.

### Container

One container, don't nest them.

```css
.container { max-width: 64rem; margin-inline: auto; padding-inline: 1.5rem; }
.measure   { max-width: 65ch; }  /* for prose */
```

## Dividers and rules

The rule is a primary design element, not an afterthought.

```css
hr             { border: 0; border-top: 1px solid currentColor; margin-block: 2rem; }
.rule-thick    { border-top: 2px solid var(--ink); }
.hairline      { border-top: 1px solid rgba(10, 10, 10, 0.15); }
```

Use rules to terminate mastheads, separate sections, frame data tables. Full-bleed within their container.

## Components

Minimum viable set. Extend with the same vocabulary.

```css
button, .btn {
  font: inherit; padding: 0.5rem 1rem;
  background: var(--surface); color: var(--ink);
  border: 1px solid var(--ink); border-radius: 0;
  cursor: pointer;
}
button:hover { background: var(--ink); color: var(--surface); }

input, textarea, select {
  font: inherit; padding: 0.5rem 0.75rem;
  background: var(--surface); color: var(--ink);
  border: 1px solid var(--ink); border-radius: 0;
}
input:focus-visible { outline: 2px solid var(--ink); outline-offset: 2px; }

a              { color: var(--ink); text-underline-offset: 0.2em; }
a:hover        { text-decoration-thickness: 2px; }

code, pre      { background: rgba(10, 10, 10, 0.05); padding: 0.1em 0.3em; }
pre            { padding: 1rem; overflow-x: auto; }

table          { border-collapse: collapse; width: 100%; }
th, td         { padding: 0.5rem 0.75rem; text-align: left;
                 border-bottom: 1px solid rgba(10, 10, 10, 0.15); }
th             { opacity: 0.5; font: 500 0.75rem/1 var(--font-mono);
                 text-transform: uppercase; letter-spacing: 0.08em; }
```

## Responsive

Mobile-first. Start single-column, layer up.

- `min-width` queries only. Never `max-width`.
- Prefer **intrinsic** responsiveness (`auto-fit`, `clamp()`, `min()`, `flex-wrap`) over breakpoints.
- One breakpoint is usually enough: `@media (min-width: 48rem)`.
- `rem` for type, `ch` for measure, `%`/`fr` for layout.

## Minimal reset

```css
*, *::before, *::after { box-sizing: border-box; }
* { margin: 0; }
html { -webkit-text-size-adjust: 100%; color-scheme: light; }
body { background: var(--surface); color: var(--ink); line-height: 1.5;
       -webkit-font-smoothing: antialiased; }
img, picture, video, canvas, svg { display: block; max-width: 100%; }
input, button, textarea, select { font: inherit; color: inherit; }
p, h1, h2, h3, h4, h5, h6 { overflow-wrap: break-word; }
:focus-visible { outline: 2px solid var(--ink); outline-offset: 2px; }
```

That's the reset. Don't add more.

## What NOT to do

- No `border-radius` above 0. Sharp corners.
- No `box-shadow`. Use rules and contrast.
- No gradients.
- No icon fonts or decorative SVG flourishes. Glyphs only when functional.
- No gray hex values for hierarchy — `opacity` only.
- No CSS frameworks (Tailwind, Bootstrap, etc.).
- No external font CDNs or `@font-face`.
- No animations longer than 200ms. Easing: `ease-out`.
- No `<div>` where `<section>`, `<header>`, `<nav>`, `<article>`, `<aside>`, `<figure>`, or `<main>` fits.
- No margin on component roots — let the parent space them.
