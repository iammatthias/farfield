// terrain.js — the shared fabric language of the art pages.
//
// Both /art and /art/structure draw exactly one material: a glyph-printed
// fabric sheet. The day's quantized heightfield is draped smoothly
// (smoothstep-bilinear between samples — cloth flexing and folding, never
// stairs) and the surface carries the day's actual ramp glyphs as printed
// ink, the same chars, the same band→ink mapping the SVG plate uses
// (zone inks[⌊lv·len/bands⌋] — the biome gives the glyphs, the day's zone
// gives every color). Age fades every ink toward the page surface, like
// the SVG's opacity bands. The plate page is one sheet up close; the
// structure is the archive of all of them, stacked into the curve — the
// same cloth at two zooms.

import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';

// readTheme pulls the page inks so a scene always matches the surface.
export function readTheme() {
  const css = getComputedStyle(document.documentElement);
  const v = (name, fb) => css.getPropertyValue(name).trim() || fb;
  return {
    surface: new THREE.Color(v('--surface', '#fafaf7')),
    ink: new THREE.Color(v('--ink', '#0a0a0a')),
    accent: new THREE.Color(v('--accent', '#d93a00')),
  };
}

export const reduceMotion = () =>
  matchMedia('(prefers-reduced-motion: reduce)').matches;

// ── glyph ramps ────────────────────────────────────────────────────────────
// Mirrors biomes[] in biome.go, keyed by biome name — the approved API
// shapes carry each biome's inks but not its glyph vocabulary, so the ramps
// live here. Low elevation → high, exactly the SVG plate's characters.
export const RAMPS = {
  basin: '·:░▒▓█',
  dune: '˙‥∴∷▒▓',
  ridge: '·▁▂▄▆█',
  mire: '·⠂⠆⠖⠶⠿',
  shoal: '·~≈≋▒▓',
  steppe: '·∙▪▮▓█',
  karst: '·○◍◉●█',
  caldera: '·∘≡▒▓█',
};

// The SVG plates' monospace stack — the printed glyphs must match the page.
const MONO = `ui-monospace, 'SF Mono', Menlo, Consolas, monospace`;

// css renders a (linear, working-space) THREE.Color back to the sRGB hex the
// 2-D canvas wants, so canvas inks and lit vertex inks agree on screen.
export const css = (c) => '#' + c.getHexString(THREE.SRGBColorSpace);

// ── smooth drape sampling ──────────────────────────────────────────────────

const smooth01 = (t) => t * t * (3 - 2 * t);

// levelSampler interpolates a flat row-major g×g band field continuously:
// smoothstep-bilinear between cell-center samples, clamped at the rim. This
// is the fold of the fabric — the quantized survey data read as a flexing
// surface instead of stairs.
export function levelSampler(levels) {
  const g = Math.round(Math.sqrt(levels.length));
  const at = (i, j) =>
    levels[Math.min(g - 1, Math.max(0, j)) * g + Math.min(g - 1, Math.max(0, i))];
  return (u, v) => {
    const gx = u * g - 0.5;
    const gy = v * g - 0.5;
    const i = Math.floor(gx);
    const j = Math.floor(gy);
    const fx = smooth01(gx - i);
    const fy = smooth01(gy - j);
    const a = at(i, j);
    const b = at(i + 1, j);
    const c = at(i, j + 1);
    const d = at(i + 1, j + 1);
    return a + (b - a) * fx + (c - a) * fy + (a - b + d - c) * fx * fy;
  };
}

// drapeHeights samples the smooth drape at the (g+1)² cell corners of a
// sheet — one shared height table, so the printed top and its side skirts
// meet exactly. `lift` raises the drape's floor (0 = the full band span,
// the plate's pure mapping; >0 compresses the folds onto a thicker body —
// the structure's sheets, which must stay calm at six samples per cell).
export function drapeHeights(levels, bands, relief, lift = 0) {
  const g = Math.round(Math.sqrt(levels.length));
  const sample = levelSampler(levels);
  const H = new Float32Array((g + 1) * (g + 1));
  for (let j = 0; j <= g; j++) {
    for (let i = 0; i <= g; i++) {
      const t = (sample(i / g, j / g) + 1) / bands;
      H[j * (g + 1) + i] = (lift + (1 - lift) * t) * relief;
    }
  }
  return { g, H };
}

// ── glyph textures ─────────────────────────────────────────────────────────

function makeCanvas(w, h) {
  const c = document.createElement('canvas');
  c.width = w;
  c.height = h;
  return c;
}

// fabricTexture wraps a glyph canvas for the GPU: sRGB so the inks match
// the page, max anisotropy so the print stays crisp across the drape.
export function fabricTexture(renderer, canvas) {
  const tex = new THREE.CanvasTexture(canvas);
  tex.colorSpace = THREE.SRGBColorSpace;
  tex.anisotropy = renderer.capabilities.getMaxAnisotropy();
  return tex;
}

// coverageTexture wraps a NEUTRAL atlas canvas — an ink-coverage mask, not
// a picture — so no color-space decode bends the stored intensities.
export function coverageTexture(renderer, canvas) {
  const tex = new THREE.CanvasTexture(canvas);
  tex.colorSpace = THREE.NoColorSpace;
  tex.anisotropy = renderer.capabilities.getMaxAnisotropy();
  return tex;
}

// plateCanvas prints one full day onto cloth: the 24×24 glyph field, one
// ramp glyph per cell in its zone band ink on a faint band wash over the
// zone's paper tint — the SVG plate's exact language, rasterized as the
// fabric's print. 24 cells × 64 px = a 1536² canvas.
export function plateCanvas({ levels, bands, ramp, palette, surface, wash, cellPx = 64 }) {
  const g = Math.round(Math.sqrt(levels.length));
  const canvas = makeCanvas(g * cellPx, g * cellPx);
  const ctx = canvas.getContext('2d');
  const glyphs = [...ramp];
  ctx.font = Math.round(cellPx * 0.78) + 'px ' + MONO;
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  const paper = wash || surface;
  const inks = [];
  const washes = [];
  for (let b = 0; b < bands; b++) {
    const ink = palette[Math.floor((b * palette.length) / bands)];
    inks.push(css(ink));
    washes.push(css(paper.clone().lerp(ink, 0.05 + (0.08 * b) / Math.max(bands - 1, 1))));
  }
  for (let j = 0; j < g; j++) {
    for (let i = 0; i < g; i++) {
      const lv = levels[j * g + i];
      ctx.fillStyle = washes[lv];
      ctx.fillRect(i * cellPx, j * cellPx, cellPx, cellPx);
      ctx.fillStyle = inks[lv];
      ctx.fillText(glyphs[lv], (i + 0.5) * cellPx, (j + 0.5) * cellPx);
    }
  }
  return canvas;
}

// ── animated plate print ───────────────────────────────────────────────────

// seedHash folds the day's seed material (its date string) into a uint32 —
// FNV-1a. Same date, same hash, same animation personality.
export function seedHash(str) {
  let h = 0x811c9dc5;
  for (let i = 0; i < str.length; i++) {
    h ^= str.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

// mulberry32 — a tiny deterministic PRNG, only for deriving the day's
// animation parameters (tempo, direction, pulse interval) from its seed.
function mulberry32(a) {
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// plateAnimation is plateCanvas made to live: the same 24×24 print, but
// paint(t) redraws every cell for time t so the characters course through
// the landform. The motion is elevation-driven:
//
//   · a primary wave travels through ELEVATION — each cell's phase is its
//     normalized height ×2.2 (plus a tiny spatial jitter), so the character
//     cycling (base band ±1 step along the ramp) sweeps coherently from the
//     peaks down through the valleys, or upward — the day's seed decides.
//   · troughs FLOW — cells in the lowest two bands take an extra per-row
//     phase term advancing with t, so basins read as a lateral current.
//   · a PULSE every 8–14 s (seed-derived) — a ring radiating outward from
//     the highest cell over ~2 s, brightening ink ~25% and pushing the glyph
//     one step up the ramp as it passes.
//   · the ink BREATHES — per-cell opacity rides the same wave ±12%, so the
//     light moves with the characters. Washes stay put: stable ground under
//     a moving print.
//
// Everything per-cell is precomputed (layout, phases, ink strings); paint
// allocates nothing. The day's seed sets ω (tempo), wave direction, trough
// current, and pulse interval — each date animates with its own character,
// and the same date always animates the same way.
export function plateAnimation({ levels, bands, ramp, palette, surface, wash, seed, cellPx = 64 }) {
  const g = Math.round(Math.sqrt(levels.length));
  const canvas = makeCanvas(g * cellPx, g * cellPx);
  const ctx = canvas.getContext('2d');
  const glyphs = [...ramp];
  const font = Math.round(cellPx * 0.78) + 'px ' + MONO;
  const paper = wash || surface;
  const inks = [];
  const washes = [];
  for (let b = 0; b < bands; b++) {
    const ink = palette[Math.floor((b * palette.length) / bands)];
    inks.push(css(ink));
    washes.push(css(paper.clone().lerp(ink, 0.05 + (0.08 * b) / Math.max(bands - 1, 1))));
  }

  // The day's animation personality, drawn deterministically from the seed.
  const rng = mulberry32(seed >>> 0);
  const omega = 0.55 + rng() * 0.4;                       // tempo, rad/s (T ≈ 7–11 s)
  const dir = rng() < 0.5 ? 1 : -1;                       // wave: peaks→valleys or back
  const pulseEveryMs = 8000 + rng() * 6000;               // a pulse every 8–14 s
  const flow = (0.9 + rng() * 0.7) * (rng() < 0.5 ? 1 : -1); // trough current, rad/s
  const tickMs = 90 + Math.round(rng() * 40);             // deliberate cadence, 90–130 ms
  const pulseMs = 2000;

  // Per-cell precompute: phase from elevation (+ tiny jitter), trough flag,
  // distance from the day's peak (highest band, nearest center on ties).
  const TAU = Math.PI * 2;
  const n = g * g;
  const phase = new Float32Array(n);
  const low = new Uint8Array(n);
  const distPeak = new Float32Array(n);
  let peakI = 0;
  let peakJ = 0;
  let peakScore = -Infinity;
  for (let j = 0; j < g; j++) {
    for (let i = 0; i < g; i++) {
      const c = j * g + i;
      const lv = levels[c];
      const h = Math.imul((seed ^ Math.imul(i, 73856093) ^ Math.imul(j, 19349663)) | 0, 2654435761);
      const jitter = (((h >>> 16) & 1023) / 1023 - 0.5) * 0.1; // ±0.05
      phase[c] = (lv / Math.max(bands - 1, 1)) * 2.2 * TAU + jitter * TAU;
      low[c] = lv < 2 ? 1 : 0;
      const score = lv * 1e6 - Math.hypot(i - (g - 1) / 2, j - (g - 1) / 2);
      if (score > peakScore) {
        peakScore = score;
        peakI = i;
        peakJ = j;
      }
    }
  }
  let ringSpan = 0;
  for (let j = 0; j < g; j++) {
    for (let i = 0; i < g; i++) {
      const d = Math.hypot(i - peakI, j - peakJ);
      distPeak[j * g + i] = d;
      if (d > ringSpan) ringSpan = d;
    }
  }
  ringSpan += 2; // the ring fully exits the field before the next pulse

  // paint redraws the whole field at time tMs. 576 fillRect + fillText at
  // ~10 Hz; no allocation in here.
  const paint = (tMs) => {
    const t = tMs / 1000;
    const pt = tMs % pulseEveryMs;
    const ringR = pt < pulseMs ? (pt / pulseMs) * ringSpan : -1e9;
    ctx.font = font;
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    for (let j = 0; j < g; j++) {
      for (let i = 0; i < g; i++) {
        const c = j * g + i;
        const lv = levels[c];
        let arg = t * omega * dir - phase[c];
        if (low[c]) arg += i * 0.85 + j * 0.4 - t * flow;
        const active = Math.sin(arg);
        let idx = lv + Math.round(active * 1.4);
        let alpha = 0.85 * (1 + 0.12 * active);
        const d = Math.abs(distPeak[c] - ringR);
        if (d < 1.6) {
          const k = 1 - d / 1.6;
          if (k > 0.4) idx += 1;
          alpha += 0.25 * k;
        }
        if (idx < 0) idx = 0;
        else if (idx >= bands) idx = bands - 1;
        ctx.globalAlpha = 1;
        ctx.fillStyle = washes[lv];
        ctx.fillRect(i * cellPx, j * cellPx, cellPx, cellPx);
        ctx.globalAlpha = alpha > 1 ? 1 : alpha;
        ctx.fillStyle = inks[idx];
        ctx.fillText(glyphs[idx], (i + 0.5) * cellPx, (j + 0.5) * cellPx);
      }
    }
    ctx.globalAlpha = 1;
  };

  return { canvas, paint, tickMs, omega, pulseEveryMs };
}

// glyphAtlas prints one biome's whole vocabulary as a NEUTRAL coverage
// mask: one row of bands tiles, each tile its ramp glyph at a per-band ink
// intensity over a faint per-band wash intensity. The texel's green channel
// is ink coverage — zonedMaterial mixes the page surface toward each
// vertex's zone ink by it — so eight biome atlases serve all sixteen zones
// (color lives per vertex, age fade folded in there too), never biome ×
// zone textures. uvFor insets the rect slightly so mip filtering never
// bleeds a neighboring tile in.
export function glyphAtlas({ ramp, bands, tilePx = 192 }) {
  const canvas = makeCanvas(bands * tilePx, tilePx);
  const ctx = canvas.getContext('2d');
  const glyphs = [...ramp];
  ctx.font = Math.round(tilePx * 0.94) + 'px ' + MONO;
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  const gray = (v) => {
    const b = Math.round(v * 255);
    return 'rgb(' + b + ',' + b + ',' + b + ')';
  };
  for (let b = 0; b < bands; b++) {
    const t = b / Math.max(bands - 1, 1);
    // The band wash runs a touch stronger than the old painted atlases so
    // each plate carries its zone tint even where mips average the print.
    ctx.fillStyle = gray(0.12 + 0.14 * t);
    ctx.fillRect(b * tilePx, 0, tilePx, tilePx);
    ctx.fillStyle = gray(0.82 + 0.18 * t); // per-band glyph intensity
    ctx.fillText(glyphs[b], (b + 0.5) * tilePx, 0.5 * tilePx);
  }
  const inset = 0.08;
  const uvFor = (b) => ({
    u0: (b + inset) / bands,
    u1: (b + 1 - inset) / bands,
    vLo: inset,
    vHi: 1 - inset,
  });
  return { canvas, uvFor };
}

// zonedMaterial draws geometry printed from a neutral glyphAtlas: the map's
// green channel is ink coverage, the vertex color is the cell's zone ink
// (age fade already folded in), and the fragment mixes the page surface
// toward that ink by the coverage — band intensity × zone ink. The material
// color still multiplies the result, so the dark back-side trick works
// unchanged. patchVertex lets a scene add its motion shader on top; pass a
// distinct cacheKey per patch shape so three never shares programs across
// different patches.
export function zonedMaterial({ map, surface, back = false, patchVertex = null, cacheKey = '' }) {
  const mat = new THREE.MeshLambertMaterial(back
    ? { map, vertexColors: true, side: THREE.BackSide, color: new THREE.Color(0.42, 0.42, 0.4) }
    : { map, vertexColors: true });
  mat.onBeforeCompile = (sh) => {
    sh.uniforms.uSurface = { value: surface };
    sh.fragmentShader = sh.fragmentShader
      .replace('#include <common>', '#include <common>\nuniform vec3 uSurface;')
      .replace('#include <map_fragment>', '')
      .replace('#include <color_fragment>',
        'vec4 inkTexel = texture2D( map, vMapUv );\n' +
        'diffuseColor.rgb *= mix( uSurface, vColor, inkTexel.g );');
    if (patchVertex) patchVertex(sh);
  };
  mat.customProgramCacheKey = () => 'zoned:' + cacheKey;
  return mat;
}

// ── fabric sheet geometry ──────────────────────────────────────────────────

// FabricBuilder accumulates glyph-printed drape surfaces into one merged
// non-indexed soup with positions, smooth normals (from the drape's own
// gradient, so the cloth shades as folds, not facets), and per-cell UVs into
// a glyph atlas. One builder per atlas texture — build once, draw as one
// mesh.
export class FabricBuilder {
  constructor() {
    this.pos = [];
    this.nrm = [];
    this.uv = [];
  }

  get vertexCount() {
    return this.pos.length / 3;
  }

  vert(p, n, u, v) {
    this.pos.push(p[0], p[1], p[2]);
    this.nrm.push(n[0], n[1], n[2]);
    this.uv.push(u, v);
  }

  // sheet drapes one glyph-printed square of fabric: the g×g band field
  // smoothly draped over `relief` above the sheet floor y, each cell mapped
  // to its band's atlas tile at the sheet's age.
  //   levels  flat row-major band indices, side = √length
  //   bands   quantization level count (the ramp's length)
  //   uvFor   (band, age) → atlas rect, from glyphAtlas
  sheet({ levels, bands, x, z, y, size, relief, lift = 0, uvFor, age = 0 }) {
    const { g, H } = drapeHeights(levels, bands, relief, lift);
    const cs = size / g;
    const x0 = x - size / 2;
    const z0 = z - size / 2;
    const hAt = (i, j) => H[j * (g + 1) + i];
    const nAt = (i, j) => {
      const il = Math.max(i - 1, 0);
      const ir = Math.min(i + 1, g);
      const jl = Math.max(j - 1, 0);
      const jr = Math.min(j + 1, g);
      const dx = (hAt(ir, j) - hAt(il, j)) / ((ir - il) * cs);
      const dz = (hAt(i, jr) - hAt(i, jl)) / ((jr - jl) * cs);
      const inv = 1 / Math.hypot(dx, 1, dz);
      return [-dx * inv, inv, -dz * inv];
    };
    for (let j = 0; j < g; j++) {
      for (let i = 0; i < g; i++) {
        const r = uvFor(levels[j * g + i], age);
        const xa = x0 + i * cs;
        const xb = xa + cs;
        const za = z0 + j * cs;
        const zb = za + cs;
        const p00 = [xa, y + hAt(i, j), za];
        const p01 = [xa, y + hAt(i, j + 1), zb];
        const p11 = [xb, y + hAt(i + 1, j + 1), zb];
        const p10 = [xb, y + hAt(i + 1, j), za];
        const n00 = nAt(i, j);
        const n01 = nAt(i, j + 1);
        const n11 = nAt(i + 1, j + 1);
        const n10 = nAt(i + 1, j);
        this.vert(p00, n00, r.u0, r.vHi);
        this.vert(p01, n01, r.u0, r.vLo);
        this.vert(p11, n11, r.u1, r.vLo);
        this.vert(p00, n00, r.u0, r.vHi);
        this.vert(p11, n11, r.u1, r.vLo);
        this.vert(p10, n10, r.u1, r.vHi);
      }
    }
  }

  geometry() {
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.Float32BufferAttribute(this.pos, 3));
    geo.setAttribute('normal', new THREE.Float32BufferAttribute(this.nrm, 3));
    geo.setAttribute('uv', new THREE.Float32BufferAttribute(this.uv, 2));
    return geo;
  }
}

// fabricMaterial draws the printed cloth — Lambert, no shine; the key light
// and the drape normals give the folds.
export function fabricMaterial(map) {
  return new THREE.MeshLambertMaterial({ map });
}

// InkBuilder accumulates the unprinted parts of the sheets — side skirts and
// dark undersides — as flat-inked soup with per-vertex colors, so a stack of
// sheets reads as laminated strata.
export class InkBuilder {
  constructor() {
    this.pos = [];
    this.col = [];
  }

  get vertexCount() {
    return this.pos.length / 3;
  }

  // quad pushes one face as two flat-colored triangles, a→b→c→d CCW seen
  // from outside.
  quad(a, b, c, d, color) {
    this.pos.push(...a, ...b, ...c, ...a, ...c, ...d);
    for (let i = 0; i < 6; i++) this.col.push(color.r, color.g, color.b);
  }

  // sheetSides closes one fabric sheet into a laminate layer: a skirt on
  // every side from the drape edge down to the sheet floor, in the rim
  // cell's band ink (the stratified sides), and a darkened flat underside —
  // the back of the cloth, seen in the shadow gaps between stacked days.
  sheetSides({ levels, bands, palette, surface, fade = 1, x, z, y, size, relief, lift = 0 }) {
    const { g, H } = drapeHeights(levels, bands, relief, lift);
    const cs = size / g;
    const x0 = x - size / 2;
    const z0 = z - size / 2;
    const x1 = x0 + size;
    const z1 = z0 + size;
    const hAt = (i, j) => y + H[j * (g + 1) + i];
    const lvAt = (i, j) => levels[j * g + i];
    const walls = [];
    for (let b = 0; b < bands; b++) {
      const ink = palette[Math.floor((b * palette.length) / bands)];
      walls.push(surface.clone().lerp(ink, 0.9 * fade));
    }
    const under = surface.clone().lerp(palette[palette.length - 1], 0.85 * fade);
    // underside, facing down
    this.quad([x0, y, z0], [x1, y, z0], [x1, y, z1], [x0, y, z1], under);
    for (let k = 0; k < g; k++) {
      const za = z0 + k * cs;
      const zb = za + cs;
      const xa = x0 + k * cs;
      const xb = xa + cs;
      this.quad([x1, y, zb], [x1, y, za], [x1, hAt(g, k), za], [x1, hAt(g, k + 1), zb], walls[lvAt(g - 1, k)]);
      this.quad([x0, y, za], [x0, y, zb], [x0, hAt(0, k + 1), zb], [x0, hAt(0, k), za], walls[lvAt(0, k)]);
      this.quad([xa, y, z1], [xb, y, z1], [xb, hAt(k + 1, g), z1], [xa, hAt(k, g), z1], walls[lvAt(k, g - 1)]);
      this.quad([xb, y, z0], [xa, y, z0], [xa, hAt(k, 0), z0], [xb, hAt(k + 1, 0), z0], walls[lvAt(k, 0)]);
    }
  }

  geometry() {
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.Float32BufferAttribute(this.pos, 3));
    geo.setAttribute('color', new THREE.Float32BufferAttribute(this.col, 3));
    geo.computeVertexNormals();
    return geo;
  }
}

// inkMaterial draws the skirts and undersides — Lambert, vertex-colored.
export function inkMaterial() {
  return new THREE.MeshLambertMaterial({ vertexColors: true });
}

// ── the shared rig: scene, lights, camera, orbit ───────────────────────────

// createRig assembles the identical viewing instrument both pages use:
// surface-colored scene, the flat survey light (mostly ambient, one soft
// key), a damped orbit with a slow auto-turntable that pauses while the
// visitor drives. Construction throws early if WebGL is unavailable, so
// callers can keep their SVG fallback before fetching anything; call
// mount() to swap the fallback for the canvas once the scene is built.
export function createRig(plate, { theme, viewSize, aspect, camDist = 2.06 }) {
  let renderer;
  try {
    renderer = new THREE.WebGLRenderer({ antialias: true });
  } catch (err) {
    throw new Error('WebGL unavailable: ' + err.message);
  }
  if (!renderer.getContext()) throw new Error('WebGL context is null');
  renderer.setPixelRatio(Math.min(devicePixelRatio || 1, 2));

  const scene = new THREE.Scene();
  scene.background = theme.surface;

  // One viewing direction for every art scene — a survey elevation of
  // ~33°, close to the SVG plates' isometric pitch; pages only choose how
  // far out they sit (the structure frames its whole row, the plate fills
  // its frame with one sheet).
  const camera = new THREE.PerspectiveCamera(35, aspect, viewSize / 100, viewSize * 100);
  camera.position.set(0.593, 0.545, 0.593).multiplyScalar(viewSize * camDist);
  camera.lookAt(0, 0, 0);

  // Flat survey light: mostly ambient, one soft key so the drape's folds
  // read. Intensities carry the ×π of three's physical lighting mode
  // (r155+); the total on an upward face stays at unity, so the biome inks
  // render true — over- or under-lit rigs gray the palette.
  scene.add(new THREE.AmbientLight(0xffffff, 0.72 * Math.PI));
  const key = new THREE.DirectionalLight(0xffffff, 0.35 * Math.PI);
  key.position.set(viewSize, viewSize * 1.6, viewSize * 0.5);
  scene.add(key);

  const controls = new OrbitControls(camera, renderer.domElement);
  controls.enableDamping = true;
  controls.dampingFactor = 0.08;
  controls.enablePan = false;
  controls.minDistance = viewSize * 1.1;
  controls.maxDistance = viewSize * 4;
  const still = reduceMotion();
  controls.autoRotate = !still;
  controls.autoRotateSpeed = 0.5; // ≈3°/s — a survey turntable, not a spinner

  let lastInteract = -Infinity;
  const noteInteraction = () => {
    lastInteract = performance.now();
  };
  renderer.domElement.addEventListener('pointerdown', noteInteraction);
  renderer.domElement.addEventListener('wheel', noteInteraction, { passive: true });

  return {
    renderer,
    scene,
    camera,
    controls,
    // mount swaps the SVG fallback for the live canvas.
    mount() {
      plate.classList.add('live');
      const svg = plate.querySelector('svg');
      if (svg) svg.remove();
      plate.appendChild(renderer.domElement);
      const resize = () => {
        const w = plate.clientWidth || 400;
        const h = plate.clientHeight || Math.round(w / aspect);
        renderer.setSize(w, h, false);
        camera.aspect = w / h;
        camera.updateProjectionMatrix();
      };
      new ResizeObserver(resize).observe(plate);
      resize();
    },
    // start runs the loop: the page's tick, then orbit + render. Auto-orbit
    // resumes a few seconds after the visitor lets go.
    start(tick) {
      renderer.setAnimationLoop((now) => {
        if (tick) tick(now);
        if (!still) controls.autoRotate = now - lastInteract > 5000;
        controls.update();
        renderer.render(scene, camera);
      });
    },
  };
}
