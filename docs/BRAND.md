# Farfield Visual Style Guide

## Brand idea

**Distance made legible.**

Farfield should feel like software built for looking farther out: quiet,
precise, expansive, technically capable, and slightly uncanny.

The visual language combines deep-space scale, scientific observation,
mid-century optimism, and remote terrestrial landscapes. The product itself
stays restrained so the broader world around it can carry the emotion.

Recurring visual ideas:

- vast fields and negative space
- distant horizons
- isolated points of light
- trajectories, paths, arcs, and signals
- planetary scale
- scientific imagery treated poetically
- one unusual focal element surrounded by immense space
- human-scale objects used sparingly to communicate distance

The central visual idea is:

**an immense field + a distant signal + one reason to keep looking**

---

## 1. Visual principles

**Vast** — Space is a primary design element. Pages should feel unusually open
for SaaS. Allow large areas of background, imagery, and atmosphere to exist
without filling them with cards or copy.

**Precise** — Typography, controls, diagrams, and data should feel engineered.
Use alignment, fine rules, small technical labels, and careful spacing to
counterbalance the more romantic illustration system.

**Quiet** — Avoid visual noise. Farfield should rarely have more than one
dominant idea competing for attention in a section.

**Curious** — There should often be one element that creates a small amount of
tension or mystery: a distant object, unexpected scale shift, strange signal,
impossible celestial body, or unexplained trajectory.

**Material** — The marketing world should feel printed rather than rendered.
Halftone, ink texture, paper grain, crosshatching, and slight imperfections
give the brand physical character without making the product UI itself feel
distressed.

---

## 2. Color

### Core palette

| Token | Hex | Use |
|---|---|---|
| Deep Space | `#0E222D` | Primary dark background |
| Farfield Blue | `#0D3560` | Brand field |
| Observatory Blue | `#1C4773` | Secondary surfaces |
| Atmosphere | `#2D5A86` | Elevated blue elements |
| Signal Mist | `#6A8699` | Muted text and diagrams |
| Horizon | `#E59F67` | Primary accent |
| Distant Sun | `#D1AA83` | Secondary warm accent |
| Paper | `#F3E5D1` | Primary light surface |
| Terrain | `#565859` | Neutral utility |
| Oxide | `#9A7962` | Tertiary accent |

Primary combination: **Farfield Blue + Paper + Horizon**

```css
:root {
  --ff-space: #0e222d;
  --ff-blue: #0d3560;
  --ff-blue-2: #1c4773;
  --ff-blue-3: #2d5a86;
  --ff-mist: #6a8699;
  --ff-horizon: #e59f67;
  --ff-sun: #d1aa83;
  --ff-paper: #f3e5d1;
  --ff-terrain: #565859;
  --ff-oxide: #9a7962;
}
```

### Color behavior

Blue creates the field. Paper carries typography and light surfaces. Orange
behaves like a signal rather than decoration.

Use Horizon for: primary actions, selected states, active data points, alerts
requiring attention, trajectory markers, small moments of emphasis.

Avoid spreading orange evenly throughout a page. It becomes more effective
when rare.

### Light mode

```css
--background: #f3e5d1;
--surface: #eee0cc;
--text: #0e222d;
--text-muted: #565859;
--border: rgba(14, 34, 45, 0.16);
--accent: #0d3560;
```

### Dark mode

```css
--background: #0e222d;
--surface: #132f3d;
--text: #f3e5d1;
--text-muted: #9babb3;
--border: rgba(243, 229, 209, 0.14);
--accent: #e59f67;
```

---

## 3. Typography

Farfield uses three open-source type families, each with a distinct role.

### Display and headings — Newsreader

Use Newsreader for hero typography, major headings, editorial moments, and
large statements. Newsreader gives Farfield a literary and exploratory quality
while retaining enough restraint to sit beside technical software.

Preferred weights: 400 for large display typography; 500 where more presence
is needed; 600 only at smaller heading sizes. Use Roman as the default.
Newsreader Italic can be used occasionally for a single word or phrase,
especially when the language becomes more conceptual or editorial. Avoid bold
serif headlines.

```css
font-family: "Newsreader", serif;
font-weight: 400;
letter-spacing: -0.025em;
line-height: 0.96;
```

### Interface and body — Inter

Use Inter for: navigation, buttons, product UI, body copy, forms, tables,
supporting marketing copy.

Preferred weights: 400 body; 500 controls and labels; 600 sparingly for
strong UI hierarchy.

```css
font-family: "Inter", sans-serif;
```

### Technical typography — IBM Plex Mono

Use IBM Plex Mono for: timestamps, coordinates, chart labels, technical
metadata, telemetry, tiny uppercase labels, code-like identifiers, instrument
readouts.

```css
font-family: "IBM Plex Mono", monospace;
```

Keep mono typography secondary. Farfield should feel technical through detail
and structure rather than turning the entire brand into terminal UI.

---

## 4. Type scale

### Marketing

| Style | Size | Line height |
|---|---|---|
| Hero | 72–88px | 0.92–0.98 |
| Display | 56–64px | 1.0 |
| H1 | 44–52px | 1.05 |
| H2 | 32–40px | 1.10 |
| H3 | 24–28px | 1.20 |
| Lead | 20–22px | 1.45 |
| Body | 16–18px | 1.55 |

### Product

| Style | Size |
|---|---|
| Screen title | 24–28px |
| Section title | 18–20px |
| Body | 15–16px |
| UI | 14px |
| Small | 12–13px |
| Technical | 11–12px |

Hero and major section headings use Newsreader. Most product screen titles
should remain Inter. This keeps the product from feeling editorial everywhere.

---

## 5. Layout

Farfield layouts should mimic landscapes. Use: low horizons, large fields,
strong horizontal divisions, asymmetrical focal points, long leading lines,
generous margins.

A typical marketing composition:

```
┌─────────────────────────────────────────────┐
│                                             │
│       LARGE OPEN FIELD                      │
│                                             │
│   Headline                                  │
│   Supporting copy                           │
│   CTA                                       │
│                                             │
├────────────────────────── distant horizon ──┤
│                                             │
│           imagery / product / proof         │
│                                             │
└─────────────────────────────────────────────┘
```

Grid: 12-column desktop grid. Recommended maximum content width 1280–1440px.
Marketing gutters 32–64px; product gutters 24–32px. Section spacing should be
unusually generous: 120–200px desktop, 72–112px mobile.

---

## 6. Hero system

The hero should establish scale immediately.

Preferred arrangement: text occupies approximately 35–40%; artwork occupies
the full canvas; visual focal point sits away from the copy; horizon remains
relatively low; sky carries most of the negative space.

A strong Farfield hero might include: headline in Newsreader, one restrained
paragraph, one primary action, one secondary textual action, a tiny technical
annotation, the landscape.

Avoid floating piles of product cards over the illustration. If product UI
appears, treat it like instrumentation observing the environment: a small
coordinate panel, a measurement readout, a trajectory overlay, an observation
window, a single cropped product surface. The software should appear to belong
to the world rather than being pasted over it.

---

## 7. Illustration

The illustration system is one of the primary brand assets.

### Composition formula

Most Farfield illustrations should contain:

1. one enormous field
2. one horizon
3. one leading line
4. one isolated focal element
5. one tiny scale reference

The focal object should usually sit off-center.

### Subject matter

Useful recurring subjects: remote observatories, antennas, unknown monuments,
solitary structures, tiny explorers, roads, rivers, survey markers, planetary
arcs, eclipses, distant lights, signals, geometric celestial objects,
reflective plains, impossible moons, geological formations.

Avoid conventional sci-fi imagery such as spaceships, robots, holographic
HUDs, glowing cyberpunk cities, or intricate futuristic machinery. Farfield is
closer to observing something unknown than depicting science fiction.

---

## 8. Illustration rendering

Use: flat screen-printed color fields, coarse halftone, stippling, etched line
work, engraved crosshatching, visible paper tooth, slight ink misregistration,
restrained color count, strong silhouettes, simplified geometry, printed
atmospheric transitions.

Lighting should often come from a horizon, distant object, or unexplained
signal.

Avoid: smooth digital gradients, ray-traced lighting, glossy rendering, lens
flares, photorealistic stars, 3D materials, synthetic glass, over-detailed
surfaces, high-frequency generative texture.

A Farfield illustration should plausibly survive being reproduced as a
five-color screen print.

---

## 9. Illustration palette

Most illustrations should use four to six inks. Recommended core:
ultramarine, deep cobalt, burnt orange, warm peach, forest green, pale cream.

Dark blue usually dominates. Warm tones create horizon light and points of
attention. Green should remain terrestrial and relatively subdued. Cream
replaces pure white. Avoid pure `#FFFFFF` inside brand illustrations.

---

## 10. Image prompt foundation

For consistency, Farfield image generation should preserve this core language:

> Wide cinematic landscape with enormous negative space and a low horizon. A
> single isolated focal element sits slightly off-center, surrounded by
> distant terrain, calm water, plains, or a winding path. The scene evokes
> astronomical observation, deep space, remote scientific expeditions, and the
> Pale Blue Dot. Rendered as a vintage mid-century travel poster crossed with
> retro-futurist editorial illustration, using flat screen-printed color
> fields, dense halftone stippling, engraved crosshatching, tactile paper
> grain, imperfect ink coverage, and subtle print misregistration. Limited
> cobalt, ultramarine, burnt orange, peach, dark green, and cream palette.
> Simplified shapes, sweeping curves, strong silhouettes, long shadows, and a
> strong leading line. Serene, mysterious, monumental, graphic, restrained. No
> text, border, logo, glossy 3D rendering, photorealism, neon cyberpunk
> aesthetics, or generic futuristic interface graphics.

Individual prompts should then specify the scene.

---

## 11. Product UI

The product should be cleaner than the brand world. The marketing site
communicates wonder. The application communicates precision.

Surfaces: flat fills, subtle contrast between surfaces, thin borders, minimal
shadows, compact radii, generous internal spacing.

```css
/* Dark card */
.card {
  background: rgba(243, 229, 209, 0.035);
  border: 1px solid rgba(243, 229, 209, 0.14);
  border-radius: 6px;
}

/* Light card */
.card {
  background: rgba(14, 34, 45, 0.025);
  border: 1px solid rgba(14, 34, 45, 0.14);
  border-radius: 6px;
}
```

Recommended radius scale: `--radius-sm: 3px; --radius-md: 6px;
--radius-lg: 10px;`

Avoid the oversized rounded rectangles common in current SaaS design.

---

## 12. Buttons

```css
.button-primary {
  background: #e59f67;
  color: #0e222d;
  border: 1px solid #e59f67;
  border-radius: 4px;
  font-family: "Inter", sans-serif;
  font-weight: 500;
}

.button-secondary {
  background: transparent;
  color: #f3e5d1;
  border: 1px solid rgba(243, 229, 209, 0.35);
  border-radius: 4px;
}
```

Buttons should feel compact and deliberate. Avoid pill-shaped CTAs except
where the component itself requires that geometry.

---

## 13. Form controls

Controls should feel instrument-like without becoming skeuomorphic. Use: thin
borders, clear labels, compact radius, visible keyboard focus, very restrained
hover effects, mono text only for genuinely technical inputs.

```css
.input {
  min-height: 44px;
  background: transparent;
  border: 1px solid rgba(243, 229, 209, 0.2);
  border-radius: 4px;
  padding: 0 12px;
}
/* Focus */
outline: 2px solid #e59f67;
outline-offset: 2px;
```

---

## 14. Data visualization

Data visualization should feel related to astronomical plots, survey maps,
and scientific plates. Use: sparse points, fine grid lines, contour lines,
small coordinate labels, long plotted trajectories, orange highlights on blue
fields, small circular markers, dotted paths, generous unused space.

The governing hierarchy is: **field → observation → signal**

Most charts should use one dominant color plus one highlight. Avoid rainbow
palettes unless categorical differentiation genuinely requires them.

Suggested chart palette:

```
Background       #0E222D
Grid             rgba(243,229,209,.10)
Primary data     #6A8699
Secondary data   #2D5A86
Signal           #E59F67
Text             #F3E5D1
```

---

## 15. Iconography

Icons should resemble drafting marks and scientific instrumentation. Use:
1–1.5px strokes, simple geometry, restrained rounding, circular plotting
motifs, sparse filled elements, optical rather than purely mathematical
alignment.

Useful forms: points, crosshairs, arcs, coordinates, trajectories, aperture
shapes, horizon lines, field markers.

### Signature motif

A small point surrounded by one or two thin circles can become a recurring
visual symbol.

```
       ○
      ·
```

Conceptually: **signal inside field**. Use it in loading indicators, active
navigation, chart highlights, favicon exploration, illustration overlays,
empty states.

---

## 16. Lines and borders

Fine lines are part of the visual language. Recommended widths: hairline 1px,
standard 1px, strong 2px. Avoid heavy box outlines.

Rules can extend far beyond the object they describe, creating a sense of
plotting or measurement. Horizontal rules are especially useful because they
echo horizons.

---

## 17. Texture

Texture should mostly remain outside the working interface. Use grain on hero
artwork, illustration sections, editorial backgrounds, campaign graphics,
social assets. Avoid strong grain on forms, tables, dashboards, long body
copy, dense UI.

For full-page atmospheric grain, keep opacity around 2–4%. The interface
should feel cleaner than the environment surrounding it.

---

## 18. Motion

Farfield motion should behave at two speeds.

**Interface motion** — fast and functional. Hover 120ms; controls 140–180ms;
panels 180–240ms. Use simple ease-out curves.

**Atmospheric motion** — extremely slow (8–40s). Examples: stars shifting
subtly over 30 seconds, a signal moving along a trajectory, faint halftone
density movement, extremely shallow parallax, a distant beacon appearing every
few seconds, chart lines gradually resolving.

Avoid: bouncing, floating cards, spring-heavy animations, rapid parallax,
looping glow effects, decorative animation everywhere. Movement should reward
looking closely.

---

## 19. Photography

Photography can coexist with illustration when treated like archival
scientific material. Good subject matter: observatories, radio telescopes,
remote landscapes, atmospheric phenomena, aerial geology, scientific
instruments, expedition equipment, distant figures, analog displays, archival
astronomy imagery.

Treatment may include: duotone, coarse screening, clipped blacks, grain,
limited palette reproduction, warm cream paper backgrounds. Avoid generic
corporate lifestyle imagery.

---

## 20. Marketing graphics

Marketing graphics should use one of three modes:

- **Landscape** — the primary emotional world; large cinematic poster scenes.
- **Observation** — a minimal field containing a plotted object, signal,
  diagram, or measurement. Useful for product feature sections.
- **Plate** — a more editorial composition inspired by scientific books and
  printed research material. Useful for case studies, technical explainers,
  reports, social assets, launch materials.

These three modes allow the brand to expand without every page requiring a
giant landscape.

---

## 21. Diagram language

Diagrams should feel drawn by someone mapping a phenomenon. Use: dots, thin
vectors, labels, coordinates, concentric circles, dashed trajectories, large
areas of empty space.

```
OBSERVATION 017
IBM Plex Mono / 11px
                     ○
                  .  │
             ·       │
        ·            │
─────────────────────┼────────
                     │
           anomaly detected
```

Avoid diagram components that resemble generic flowchart software.

---

## 22. Navigation

Navigation should be simple. Recommended desktop treatment: wordmark left,
3–5 links, secondary sign-in action, compact primary CTA. Keep it visually
light enough that the landscape remains dominant.

A transparent navigation over the hero works well as long as contrast remains
accessible. Sticky navigation can transition into a solid Deep Space
background after scrolling.

---

## 23. Wordmark direction

The word **farfield** should remain understated. A strong direction would use
lowercase, Inter Medium or a custom grotesk treatment, slightly tight
tracking, no overt space iconography.

The serif belongs to storytelling rather than the primary wordmark. This
separation keeps the identity contemporary while allowing Newsreader to create
the larger editorial character.

---

## 24. Accessibility

The restrained palette still needs clear functional hierarchy. Requirements:

- maintain WCAG AA contrast for standard text
- never communicate status using color alone
- preserve strong keyboard focus states
- respect `prefers-reduced-motion`
- keep technical labels readable rather than purely decorative
- avoid placing long copy directly over textured imagery

Reduced motion should remove ambient parallax and slow decorative movement
while retaining immediate interaction feedback.

---

## 25. Responsive behavior

On smaller screens, preserve the sense of scale rather than simply shrinking
everything.

```
Desktop:                          Mobile:

Copy            focal artwork     Copy
████████       ·                  ████████
████                              ████████
████                                    enormous open field
─────────────────── horizon                        ·
                                  ──────────────── horizon
```

Keep the focal object small. Do not crop aggressively until it becomes the
dominant object. The entire point is distance.

---

## 26. Design tokens

```css
:root {
  /* Color */
  --ff-space: #0e222d;
  --ff-blue: #0d3560;
  --ff-blue-2: #1c4773;
  --ff-blue-3: #2d5a86;
  --ff-mist: #6a8699;
  --ff-horizon: #e59f67;
  --ff-sun: #d1aa83;
  --ff-paper: #f3e5d1;
  --ff-terrain: #565859;
  --ff-oxide: #9a7962;
  /* Typography */
  --font-display: "Newsreader", serif;
  --font-sans: "Inter", sans-serif;
  --font-mono: "IBM Plex Mono", monospace;
  /* Radius */
  --radius-sm: 3px;
  --radius-md: 6px;
  --radius-lg: 10px;
  /* Spacing */
  --space-1: 4px;
  --space-2: 8px;
  --space-3: 12px;
  --space-4: 16px;
  --space-5: 24px;
  --space-6: 32px;
  --space-7: 48px;
  --space-8: 64px;
  --space-9: 96px;
  --space-10: 128px;
  /* Motion */
  --duration-fast: 120ms;
  --duration-ui: 180ms;
  --duration-panel: 240ms;
}
```

---

## 27. Font stack

All primary typefaces are open source.

```css
@import url("https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500&family=Inter:wght@400;500;600&family=Newsreader:opsz,wght@6..72,400;6..72,500;6..72,600&display=swap");
:root {
  --font-display: "Newsreader", Georgia, serif;
  --font-sans: "Inter", system-ui, sans-serif;
  --font-mono: "IBM Plex Mono", monospace;
}
```

Preferred hierarchy: Newsreader — exploration and narrative; Inter —
interface and clarity; IBM Plex Mono — observation and instrumentation.

---

## 28. Do

- Use immense negative space.
- Keep horizons low.
- Use Newsreader confidently at large sizes.
- Make orange rare enough to feel meaningful.
- Build illustrations from a restricted palette.
- Introduce one strange element at a time.
- Use subtle technical details to reward close inspection.
- Treat data as something being observed.
- Keep product surfaces precise and quiet.
- Let imagery carry emotion.

## 29. Avoid

- neon purple and cyan AI gradients
- glossy glass cards
- excessive blur
- huge rounded rectangles
- 3D chrome
- generic space imagery
- astronauts used as decoration
- dense starfields behind every section
- serif typography throughout the actual application
- excessive monospace
- overly distressed UI
- multiple competing accent colors
- generic stock photography
- dashboard screenshots floating in perspective
- overt NASA imitation
- retro styling applied literally to controls

---

## 30. Brand shorthand

When evaluating anything designed for Farfield, ask whether it feels like:

**a scientific instrument sitting at the edge of something enormous.**

That balance is the brand. The instrument is precise. The field is vast. The
signal is small. And the rest remains unexplained.
