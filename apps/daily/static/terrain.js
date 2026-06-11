// terrain.js — the shared terraced-terrain language of the art pages.
//
// Both /art and /art/structure draw exactly one geometry: a quantized
// heightfield rendered as terraced steps — a flat top per cell at its
// elevation band, sheer walls where bands change — sitting on a thin slab.
// The band→ink mapping is the SVG glyph ramp's own (palette[⌊lv·3/bands⌋]);
// age fades every ink toward the page surface, like the SVG's opacity
// bands. One lighting rig, one camera feel, one background. The plate page
// is this tile at full sample density; the structure is a lattice of the
// same tile, smaller — the same instrument at two zooms.

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

// Ink weights, straight from the SVG's language: on the plate, low
// elevations are sparse light glyphs and peaks are dense dark ones, so
// terrace tops deepen with band — washed near the surface in the basins,
// close to full ink at the peaks — while every step wall carries its band
// ink near full strength (the SVG's stratified sides). The result is a
// survey plate that shades darker as it climbs, not a solid mass.
const topWashLo = 0.38;
const topWashHi = 0.88;
const wallInkStrength = 0.95;

// ── terraced tile geometry ─────────────────────────────────────────────────

// TileBuilder accumulates terraced tiles into one merged, non-indexed
// triangle soup with per-vertex colors — build once, draw as one mesh.
// Flat shading falls out of the soup: computeVertexNormals on non-indexed
// triangles yields face normals, so every terrace top and wall is a crisp
// plane, like the plate's discrete elevation bands.
export class TileBuilder {
  constructor() {
    this.pos = [];
    this.col = [];
  }

  get vertexCount() {
    return this.pos.length / 3;
  }

  // quad pushes one face as two triangles, flat-colored. Callers wind
  // a→b→c→d counter-clockwise as seen from outside.
  quad(a, b, c, d, color) {
    this.pos.push(...a, ...b, ...c, ...a, ...c, ...d);
    for (let i = 0; i < 6; i++) this.col.push(color.r, color.g, color.b);
  }

  // box pushes a full axis-aligned box — the slab under a tile.
  box(x0, y0, z0, x1, y1, z1, color) {
    this.quad([x0, y1, z0], [x0, y1, z1], [x1, y1, z1], [x1, y1, z0], color); // top
    this.quad([x0, y0, z0], [x1, y0, z0], [x1, y0, z1], [x0, y0, z1], color); // bottom
    this.quad([x1, y0, z1], [x1, y0, z0], [x1, y1, z0], [x1, y1, z1], color); // +x
    this.quad([x0, y0, z0], [x0, y0, z1], [x0, y1, z1], [x0, y1, z0], color); // -x
    this.quad([x0, y0, z1], [x1, y0, z1], [x1, y1, z1], [x0, y1, z1], color); // +z
    this.quad([x1, y0, z0], [x0, y0, z0], [x0, y1, z0], [x1, y1, z0], color); // -z
  }

  // tile pushes one terraced terrain tile on its slab.
  //   levels   flat row-major band indices, side = √length
  //   bands    quantization level count (the glyph ramp's length)
  //   palette  the biome's three elevation inks as THREE.Color
  //   surface  the page surface color (the fade target)
  //   fade     1 = full ink (newest) … toward 0 = washed to the surface
  //   x, z     tile center; y — slab bottom
  //   size     terrain footprint; slabSize ≥ size adds the plinth apron
  //   relief   height of the full band span above the slab
  //   slabH    slab thickness
  //   capH     optional build-animation clamp: terrace tops rise to at most
  //            capH above the slab, so elevations settle low bands first
  tile({ levels, bands, palette, surface, fade = 1, x, z, y, size, relief, slabSize = size, slabH, capH = Infinity }) {
    const g = Math.round(Math.sqrt(levels.length));
    const cs = size / g;
    const x0 = x - size / 2;
    const z0 = z - size / 2;
    const yTop = y + slabH;
    const stepH = relief / bands;

    // band → ink: the glyph ramp's mapping; tops deepen with elevation,
    // walls run near-full, everything fades toward the surface with age.
    const inks = [];
    const walls = [];
    for (let b = 0; b < bands; b++) {
      const ink = palette[Math.floor((b * palette.length) / bands)];
      const wash = topWashLo + ((topWashHi - topWashLo) * b) / Math.max(bands - 1, 1);
      inks.push(surface.clone().lerp(ink, wash * fade));
      walls.push(surface.clone().lerp(ink, wallInkStrength * fade));
    }

    // Slab — the darkest ink washed well back toward the surface, so the
    // plinth reads as mounting, not artwork.
    const slabInk = surface.clone().lerp(palette[palette.length - 1], 0.4 * fade);
    const sx0 = x - slabSize / 2;
    const sz0 = z - slabSize / 2;
    this.box(sx0, y, sz0, sx0 + slabSize, yTop, sz0 + slabSize, slabInk);

    const hAt = (i, j) =>
      i < 0 || j < 0 || i >= g || j >= g
        ? 0
        : Math.min((levels[j * g + i] + 1) * stepH, capH);

    for (let j = 0; j < g; j++) {
      for (let i = 0; i < g; i++) {
        const lv = levels[j * g + i];
        const h = hAt(i, j);
        const t = yTop + h;
        const cx0 = x0 + i * cs;
        const cz0 = z0 + j * cs;
        const cx1 = cx0 + cs;
        const cz1 = cz0 + cs;
        this.quad([cx0, t, cz0], [cx0, t, cz1], [cx1, t, cz1], [cx1, t, cz0], inks[lv]);
        // Walls — drawn by the higher cell, down to the neighbor's top
        // (or the slab at the tile rim).
        const e = hAt(i + 1, j);
        if (h > e) this.quad([cx1, yTop + e, cz1], [cx1, yTop + e, cz0], [cx1, t, cz0], [cx1, t, cz1], walls[lv]);
        const w = hAt(i - 1, j);
        if (h > w) this.quad([cx0, yTop + w, cz0], [cx0, yTop + w, cz1], [cx0, t, cz1], [cx0, t, cz0], walls[lv]);
        const s = hAt(i, j + 1);
        if (h > s) this.quad([cx0, yTop + s, cz1], [cx1, yTop + s, cz1], [cx1, t, cz1], [cx0, t, cz1], walls[lv]);
        const n = hAt(i, j - 1);
        if (h > n) this.quad([cx1, yTop + n, cz0], [cx0, yTop + n, cz0], [cx0, t, cz0], [cx1, t, cz0], walls[lv]);
      }
    }
  }

  // column pushes one full-height lattice cell of the structure: a solid
  // unit block whose top is the terraced mini-terrain (carved into the
  // cell's upper `relief`) and whose sides are cliffs in the band's wall
  // ink, running from the terrain edge all the way down to the cell floor.
  // `open` says which faces border empty space; faces against occupied
  // neighbors are culled, so a contiguous occupied region reads as one
  // solid mass — terrain on the exposed tops, stratified-ink cliffs on the
  // exposed sides. A buried cell (occupied above) is a plain prism: walls
  // run to the cell ceiling and no terrain is drawn.
  //   levels   flat row-major band indices, side = √length
  //   bands    quantization level count
  //   palette  the biome's three elevation inks as THREE.Color
  //   surface  the page surface color (the fade target)
  //   fade     1 = full ink (newest) … toward 0 = washed to the surface
  //   x, z     cell center; y — cell floor
  //   size     cell footprint; height — cell floor → ceiling
  //   relief   depth of the terraced top below the cell ceiling
  //   open     { top, bottom, px, nx, pz, nz } — true = exposed, draw
  column({ levels, bands, palette, surface, fade = 1, x, z, y, size = 1, height = 1, relief, open }) {
    const g = Math.round(Math.sqrt(levels.length));
    const cs = size / g;
    const x0 = x - size / 2;
    const z0 = z - size / 2;
    const x1 = x0 + size;
    const z1 = z0 + size;
    const y1 = y + height;
    const base = y1 - relief; // the terraced top lives in the cell's upper band
    const stepH = relief / bands;

    const inks = [];
    const walls = [];
    for (let b = 0; b < bands; b++) {
      const ink = palette[Math.floor((b * palette.length) / bands)];
      const wash = topWashLo + ((topWashHi - topWashLo) * b) / Math.max(bands - 1, 1);
      inks.push(surface.clone().lerp(ink, wash * fade));
      walls.push(surface.clone().lerp(ink, wallInkStrength * fade));
    }
    const lvAt = (i, j) => levels[j * g + i];
    // A buried top flattens to the cell ceiling — the cell above continues
    // the column, so the seam must be flush.
    const topAt = (i, j) => (open.top ? base + (lvAt(i, j) + 1) * stepH : y1);

    // Exposed underside of a floating cell — the slab wash of the plate.
    if (open.bottom) {
      const under = surface.clone().lerp(palette[palette.length - 1], 0.4 * fade);
      this.quad([x0, y, z0], [x1, y, z0], [x1, y, z1], [x0, y, z1], under);
    }

    // Side cliffs, one strip per rim terrain column, in that column's wall
    // ink — the stratified sides. An exposed side runs the full height,
    // cell floor → terrain edge, so stacked cells meet flush. Against an
    // occupied neighbor everything below the terraced base is interior
    // (the neighbor's block fills it), so the strip starts at the base —
    // covering the sliver that shows wherever the neighbor's own terrain
    // sits lower than ours.
    for (let k = 0; k < g; k++) {
      const za = z0 + k * cs;
      const zb = za + cs;
      const xa = x0 + k * cs;
      const xb = xa + cs;
      const bPX = open.px ? y : base;
      const bNX = open.nx ? y : base;
      const bPZ = open.pz ? y : base;
      const bNZ = open.nz ? y : base;
      this.quad([x1, bPX, zb], [x1, bPX, za], [x1, topAt(g - 1, k), za], [x1, topAt(g - 1, k), zb], walls[lvAt(g - 1, k)]);
      this.quad([x0, bNX, za], [x0, bNX, zb], [x0, topAt(0, k), zb], [x0, topAt(0, k), za], walls[lvAt(0, k)]);
      this.quad([xa, bPZ, z1], [xb, bPZ, z1], [xb, topAt(k, g - 1), z1], [xa, topAt(k, g - 1), z1], walls[lvAt(k, g - 1)]);
      this.quad([xb, bNZ, z0], [xa, bNZ, z0], [xa, topAt(k, 0), z0], [xb, topAt(k, 0), z0], walls[lvAt(k, 0)]);
    }

    if (!open.top) return; // buried — the cell above draws the surface

    // The terraced top: flat band tops, interior terrace walls drawn by
    // the higher cell down to its neighbor's top (rim columns are the side
    // strips above).
    for (let j = 0; j < g; j++) {
      for (let i = 0; i < g; i++) {
        const lv = lvAt(i, j);
        const t = topAt(i, j);
        const cx0 = x0 + i * cs;
        const cz0 = z0 + j * cs;
        const cx1 = cx0 + cs;
        const cz1 = cz0 + cs;
        this.quad([cx0, t, cz0], [cx0, t, cz1], [cx1, t, cz1], [cx1, t, cz0], inks[lv]);
        if (i + 1 < g) {
          const e = topAt(i + 1, j);
          if (t > e) this.quad([cx1, e, cz1], [cx1, e, cz0], [cx1, t, cz0], [cx1, t, cz1], walls[lv]);
        }
        if (i > 0) {
          const w = topAt(i - 1, j);
          if (t > w) this.quad([cx0, w, cz0], [cx0, w, cz1], [cx0, t, cz1], [cx0, t, cz0], walls[lv]);
        }
        if (j + 1 < g) {
          const s = topAt(i, j + 1);
          if (t > s) this.quad([cx0, s, cz1], [cx1, s, cz1], [cx1, t, cz1], [cx0, t, cz1], walls[lv]);
        }
        if (j > 0) {
          const n = topAt(i, j - 1);
          if (t > n) this.quad([cx1, n, cz0], [cx0, n, cz0], [cx0, t, cz0], [cx1, t, cz0], walls[lv]);
        }
      }
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

// terrainMaterial: the one material both pages draw tiles with — Lambert,
// vertex-colored, no shine, no shadows. The merged soup's face normals
// give the flat shading.
export function terrainMaterial() {
  return new THREE.MeshLambertMaterial({ vertexColors: true });
}

// tileContours builds hairline contour segments along terrace steps — the
// top edge wherever a cell stands above a neighbor or the tile rim. The
// 3-D counterpart of the SVG's hairline strokes. Returns a LineSegments
// ready to add, or null when the tile is a single flat band.
export function tileContours({ levels, bands, x, z, y, size, relief, slabH, color, opacity = 0.3 }) {
  const g = Math.round(Math.sqrt(levels.length));
  const cs = size / g;
  const x0 = x - size / 2;
  const z0 = z - size / 2;
  const stepH = relief / bands;
  const lift = relief * 0.004; // float the line a hair above the crease
  const yOf = (lv) => y + slabH + (lv + 1) * stepH + lift;
  const lvAt = (i, j) => (i < 0 || j < 0 || i >= g || j >= g ? -1 : levels[j * g + i]);
  const pos = [];
  for (let j = 0; j < g; j++) {
    for (let i = 0; i < g; i++) {
      const lv = lvAt(i, j);
      const t = yOf(lv);
      const cx0 = x0 + i * cs;
      const cz0 = z0 + j * cs;
      if (lvAt(i + 1, j) < lv) pos.push(cx0 + cs, t, cz0, cx0 + cs, t, cz0 + cs);
      if (lvAt(i - 1, j) < lv) pos.push(cx0, t, cz0, cx0, t, cz0 + cs);
      if (lvAt(i, j + 1) < lv) pos.push(cx0, t, cz0 + cs, cx0 + cs, t, cz0 + cs);
      if (lvAt(i, j - 1) < lv) pos.push(cx0, t, cz0, cx0 + cs, t, cz0);
    }
  }
  if (!pos.length) return null;
  const geo = new THREE.BufferGeometry();
  geo.setAttribute('position', new THREE.Float32BufferAttribute(pos, 3));
  return new THREE.LineSegments(
    geo,
    new THREE.LineBasicMaterial({ color, transparent: true, opacity }),
  );
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
  // far out they sit (the structure frames its whole 16³ volume, the
  // plate fills its frame with one tile).
  const camera = new THREE.PerspectiveCamera(35, aspect, viewSize / 100, viewSize * 100);
  camera.position.set(0.593, 0.545, 0.593).multiplyScalar(viewSize * camDist);
  camera.lookAt(0, 0, 0);

  // Flat survey light: mostly ambient, one soft key so terrace tops and
  // walls read apart. Intensities carry the ×π of three's physical
  // lighting mode (r155+); the total on an upward face stays at unity, so
  // the biome inks render true — over- or under-lit rigs gray the palette.
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
