// structure.js — progressive enhancement for /art/structure: swaps the
// server-rendered SVG (today's slice) for the hypercastle — every day of
// the archive as one flat glyph-printed plate, hung in 3-D space where the
// 4-D Hilbert walk put it. The SVG remains the no-JS / no-WebGL fallback;
// if anything here fails, the SVG stays put.
//
// The default scene (enhanceCastle) is a floating field of plates: each
// day's 6×6 heightfield draped gently over a small horizontal sheet,
// printed from the shared neutral per-biome glyph atlases and inked in the
// day's own generated zone palette (every day carries its own inks in the
// payload — there is no zone table) — the castle is quietly polychrome,
// zone color identifying each day — with air on every side.
// The lattice z axis becomes the vertical, stretched so the z-levels read
// as floors of one airy castle; the w axis is a gentle diagonal offset, so
// the four w-generations of a floor interleave instead of colliding. A
// hairline thread runs through the plate centers in day order — the
// worldline made quiet — and today's plate sits at its tip under an accent
// rim. ?view=line keeps the older worldline scene (enhanceLine): the same
// archive swept into ONE continuous fabric ribbon along the smoothed walk.
// The print, atlases, lights, and orbit come from terrain.js, shared with
// the plate page: the plate page is one day up close; the structure is all
// of them, assembled.

import * as THREE from 'three';
import {
  readTheme, reduceMotion, createRig,
  RAMPS, glyphAtlas, coverageTexture, zonedMaterial, drapeHeights,
} from 'terrain';

const plate = document.getElementById('structure-plate');
if (plate && plate.dataset.api) {
  const view = new URLSearchParams(location.search).get('view');
  const enhance = view === 'line' ? enhanceLine : enhanceCastle;
  enhance(plate).catch((err) => {
    // Keep the SVG fallback; just note why the canvas did not come up.
    console.warn('[structure] keeping SVG fallback:', err);
  });
}

// ═══ the castle: a floating field of glyph plates (default view) ═══════════

async function enhanceCastle(plate) {
  // ── tuning ────────────────────────────────────────────────────────────
  // The 4-D → 3-D mapping: p = SCALE·((x, z·LEVEL_GAP, y) + w·DIAG). The
  // lattice z axis is the vertical, stretched by LEVEL_GAP so each z-level
  // reads as a floor of the castle. DIAG carries the w axis as a gentle
  // diagonal. The walk so far fills w-generations 0–3 of the 8³ corner
  // COMPLETELY (plus a sparse frontier at w 4–7), so any two generations
  // can land in overlapping plan positions — which forces the vertical
  // component to do the separating: 0.125/generation clears the plate
  // relief (0.10) plus bob headroom for every Δw, while the four dense
  // generations of a floor stay a thin laminate (≈0.48 of a cell) well
  // under LEVEL_GAP, so the floor bands and the air between them both
  // read. The horizontal components are small and incommensurate — the
  // generations shear into an organic drift, never a staircase.
  const LEVEL_GAP = 1.5;
  const DIAG = [0.26, 0.125, 0.19]; // w offset per generation, lattice units
  const SCALE = 2.1; // lattice spacing in scene units
  const PLATE = 0.78 * SCALE; // plate side — 0.78 of a cell, air all around
  const RELIEF = 0.10 * SCALE; // drape span — low: plates, not mounds
  const THREAD_SPD = 4; // thread samples per day segment
  const REVEAL_MS = 3000; // the castle assembles plate by plate over ~3s

  const still = reduceMotion();
  const theme = readTheme();

  const res = await fetch(plate.dataset.api, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error('path API ' + res.status);
  const data = await res.json();
  const days = data.days || [];
  if (!days.length) throw new Error('empty path');
  const N = days.length; // day count: epoch through today
  const n = N - 1; // today's index
  const hfBands = data.hfBands || 6;
  const ageBands = data.bands || 5;

  // ── plate centers: the lattice addresses, mapped and centered ───────────
  const centers = days.map((d) => new THREE.Vector3(
    (d.coord[0] + d.coord[3] * DIAG[0]) * SCALE,
    (d.coord[2] * LEVEL_GAP + d.coord[3] * DIAG[1]) * SCALE,
    (d.coord[1] + d.coord[3] * DIAG[2]) * SCALE,
  ));
  // Frame the inhabited region — the corner the walk has reached so far —
  // never the empty full lattice.
  const box = new THREE.Box3().setFromPoints(centers);
  const center = box.getCenter(new THREE.Vector3());
  for (const p of centers) p.sub(center);

  const radius = box.getSize(new THREE.Vector3()).length() / 2 + PLATE;
  const rig = createRig(plate, { theme, viewSize: radius, aspect: 16 / 10, camDist: 1.9 });
  rig.controls.minDistance = radius * 0.16; // close enough to read one plate
  // Fog toward the page surface: depth without darkness — far strata recede
  // into the paper, light passes through the gaps.
  rig.scene.fog = new THREE.Fog(theme.surface, radius * 1.5, radius * 4.4);

  // ── ink: age fade, per-day zone palettes, per-biome NEUTRAL atlases ──────
  // Oldest plates faintest, floored at 0.55 of full ink, same as everywhere.
  // The atlases are coverage masks (biome glyphs at per-band intensity); the
  // color is per vertex — each day carries its own generated zone palette
  // ({ n, c, w } in the payload), the zone ink for its band is lerped from
  // the surface by the age fade, and zonedMaterial multiplies coverage ×
  // ink. Eight atlases serve every day's palette.
  const fade = (band) => 1 - (band / Math.max(ageBands - 1, 1)) * 0.45;
  const ageOf = (i) => Math.min(ageBands - 1, Math.floor(((n - i) * ageBands) / (n + 1)));

  // inkFor caches the per-band vertex inks for one day — its own palette at
  // its own age fade.
  const inkCache = new Map();
  const inkFor = (i) => {
    let inks = inkCache.get(i);
    if (!inks) {
      const f = fade(ageOf(i));
      const zc = (days[i].zone && days[i].zone.c) || [];
      const palette = zc.length ? zc.map((c) => new THREE.Color(c)) : [theme.ink];
      inks = [];
      for (let b = 0; b < hfBands; b++) {
        const ink = palette[Math.floor((b * palette.length) / hfBands)];
        inks.push(theme.surface.clone().lerp(ink, f));
      }
      inkCache.set(i, inks);
    }
    return inks;
  };

  // One neutral glyph atlas per biome, built on first use — never one per
  // day, never one per zone.
  const atlases = new Map();
  const atlasFor = (bi) => {
    let a = atlases.get(bi);
    if (!a) {
      const biome = data.biomes[bi] || { name: 'basin' };
      const made = glyphAtlas({
        ramp: RAMPS[biome.name] || RAMPS.basin,
        bands: hfBands,
      });
      a = { uvFor: made.uvFor, texture: coverageTexture(rig.renderer, made.canvas) };
      atlases.set(bi, a);
    }
    return a;
  };

  // ── plate geometry: per-biome soup, appended in day order ───────────────
  // One merged mesh per biome (one atlas each); within each, vertices land
  // in day order, so the assembly reveal is advancing draw ranges and a
  // raycast hit maps back to its day through recorded vertex ranges. The
  // `phase` attribute (one constant per plate) drives the independent bob
  // in the vertex shader; the `color` attribute carries each cell's zone
  // ink — geometry never updates after the build.
  class PlateBuilder {
    constructor() {
      this.pos = [];
      this.nrm = [];
      this.uv = [];
      this.col = [];
      this.phase = [];
      this.marks = []; // { ord, end } per day, for the reveal
      this.ranges = []; // { start, end, day } per day, for hover
    }

    get vertexCount() {
      return this.pos.length / 3;
    }

    // sheet drapes one day's 6×6 band field over a small horizontal plate
    // centered on c — the FabricBuilder sheet, with the bob phase attached
    // and each cell colored in its band's zone ink.
    sheet({ levels, c, uvFor, inks, ph }) {
      const { g, H } = drapeHeights(levels, hfBands, RELIEF, 0);
      const cs = PLATE / g;
      const x0 = c.x - PLATE / 2;
      const z0 = c.z - PLATE / 2;
      const y = c.y - RELIEF / 2; // drape centered on the lattice point
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
      const vert = (p, nv, u, v, ink) => {
        this.pos.push(p[0], p[1], p[2]);
        this.nrm.push(nv[0], nv[1], nv[2]);
        this.uv.push(u, v);
        this.col.push(ink.r, ink.g, ink.b);
        this.phase.push(ph);
      };
      for (let j = 0; j < g; j++) {
        for (let i = 0; i < g; i++) {
          const band = levels[j * g + i];
          const r = uvFor(band);
          const ink = inks[band];
          const xa = x0 + i * cs;
          const xb = xa + cs;
          const za = z0 + j * cs;
          const zb = za + cs;
          const p00 = [xa, y + hAt(i, j), za];
          const p01 = [xa, y + hAt(i, j + 1), zb];
          const p11 = [xb, y + hAt(i + 1, j + 1), zb];
          const p10 = [xb, y + hAt(i + 1, j), za];
          vert(p00, nAt(i, j), r.u0, r.vHi, ink);
          vert(p01, nAt(i, j + 1), r.u0, r.vLo, ink);
          vert(p11, nAt(i + 1, j + 1), r.u1, r.vLo, ink);
          vert(p00, nAt(i, j), r.u0, r.vHi, ink);
          vert(p11, nAt(i + 1, j + 1), r.u1, r.vLo, ink);
          vert(p10, nAt(i + 1, j), r.u1, r.vHi, ink);
        }
      }
    }

    geometry() {
      const geo = new THREE.BufferGeometry();
      geo.setAttribute('position', new THREE.Float32BufferAttribute(this.pos, 3));
      geo.setAttribute('normal', new THREE.Float32BufferAttribute(this.nrm, 3));
      geo.setAttribute('uv', new THREE.Float32BufferAttribute(this.uv, 2));
      geo.setAttribute('color', new THREE.Float32BufferAttribute(this.col, 3));
      geo.setAttribute('phase', new THREE.Float32BufferAttribute(this.phase, 1));
      return geo;
    }
  }

  const builders = new Map(); // biome → PlateBuilder
  for (let i = 0; i < N; i++) {
    const bi = days[i].biome;
    let b = builders.get(bi);
    if (!b) {
      b = new PlateBuilder();
      builders.set(bi, b);
    }
    const start = b.vertexCount;
    b.sheet({
      levels: days[i].hf,
      c: centers[i],
      uvFor: atlasFor(bi).uvFor,
      inks: inkFor(i),
      ph: (i * 2.399963) % (Math.PI * 2), // golden-angle phases — no beats
    });
    b.marks.push({ ord: i, end: b.vertexCount });
    b.ranges.push({ start, end: b.vertexCount, day: i });
  }

  // The almost-imperceptible float: each plate bobs ±1% of its own size
  // with its own phase, in the vertex shader — geometry stays static. uBob
  // eases the float in only after the assembly settles; reduced motion
  // never installs the patch at all.
  const uTime = { value: 0 };
  const uBob = { value: 0 };
  const bobAmp = (PLATE * 0.01).toFixed(5);
  const bobPatch = still ? null : (sh) => {
    sh.uniforms.uTime = uTime;
    sh.uniforms.uBob = uBob;
    sh.vertexShader = sh.vertexShader
      .replace('#include <common>',
        '#include <common>\nattribute float phase;\nuniform float uTime;\nuniform float uBob;')
      .replace('#include <begin_vertex>',
        '#include <begin_vertex>\ntransformed.y += uBob * ' + bobAmp + ' * sin(uTime + phase);');
  };
  const bobKey = still ? 'castle' : 'castle-bob';

  // Front: the printed face. Back: the same sheet seen from below — shared
  // geometry redrawn dark, so a plate overhead reads as a plate, not a hole.
  // Both are zonedMaterials: neutral atlas coverage × per-vertex zone ink.
  const meshes = []; // front meshes — raycast targets, reveal owners
  for (const [bi, b] of [...builders.entries()].sort((x, y) => x[0] - y[0])) {
    const geo = b.geometry();
    const front = new THREE.Mesh(geo, zonedMaterial({
      map: atlasFor(bi).texture, surface: theme.surface,
      patchVertex: bobPatch, cacheKey: bobKey,
    }));
    const back = new THREE.Mesh(geo, zonedMaterial({
      map: atlasFor(bi).texture, surface: theme.surface, back: true,
      patchVertex: bobPatch, cacheKey: bobKey,
    }));
    front.userData = { marks: b.marks, ranges: b.ranges };
    rig.scene.add(front, back);
    meshes.push(front);
  }

  // ── the thread of time: a hairline through the plate centers ────────────
  // The worldline made quiet — day order as one faint smoothed polyline,
  // fog-affected, present but never louder than the plates.
  let thread = null;
  let threadVerts = 0;
  if (N >= 2) {
    const curve = new THREE.CatmullRomCurve3(centers, false, 'centripetal');
    threadVerts = (N - 1) * THREAD_SPD + 1;
    const lp = new Float32Array(threadVerts * 3);
    for (let s = 0; s < threadVerts; s++) {
      curve.getPoint(s / (threadVerts - 1)).toArray(lp, s * 3);
    }
    const lgeo = new THREE.BufferGeometry();
    lgeo.setAttribute('position', new THREE.BufferAttribute(lp, 3));
    thread = new THREE.Line(lgeo, new THREE.LineBasicMaterial({
      color: theme.ink, transparent: true, opacity: 0.18,
    }));
    rig.scene.add(thread);
  }

  // ── today: full ink at the thread's tip, under the accent rim ───────────
  // The rim is a flat frame just outside today's plate, drawn through
  // occlusion (no depth test) — the survey locator, never lost in the mass.
  const rimMat = new THREE.MeshBasicMaterial({
    color: theme.accent, side: THREE.DoubleSide, transparent: true, opacity: 0.9,
    depthTest: false,
  });
  const rim = (() => {
    const c = centers[n];
    const a = PLATE * 0.57; // outer half-extent
    const b = PLATE * 0.49; // inner half-extent
    const y = c.y + RELIEF * 0.5 + PLATE * 0.02; // just above the drape
    const pos = [];
    const quad = (xa, za, xb, zb) => {
      pos.push(
        c.x + xa, y, c.z + za, c.x + xb, y, c.z + za, c.x + xb, y, c.z + zb,
        c.x + xa, y, c.z + za, c.x + xb, y, c.z + zb, c.x + xa, y, c.z + zb,
      );
    };
    quad(-a, -a, a, -b); // north band
    quad(-a, b, a, a); // south band
    quad(-a, -b, -b, b); // west band
    quad(b, -b, a, b); // east band
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.Float32BufferAttribute(pos, 3));
    const m = new THREE.Mesh(geo, rimMat);
    m.renderOrder = 1; // after the plates, so depthTest:false lands on top
    return m;
  })();
  rig.scene.add(rim);

  // ── swap the SVG for the canvas ──────────────────────────────────────────
  rig.mount();

  // ── hover tooltip: DAY n · date · biome · zone, raycast → vertex → day ───
  const tipEl = document.createElement('div');
  tipEl.className = 'structure-tip';
  tipEl.hidden = true;
  plate.appendChild(tipEl);

  const raycaster = new THREE.Raycaster();
  const pointer = new THREE.Vector2();
  const epochMs = Date.UTC(...(data.epoch || '2020-01-01').split('-')
    .map(Number).map((v, i) => (i === 1 ? v - 1 : v)));
  const dateOf = (i) => new Date(epochMs + i * 86400000).toISOString().slice(0, 10);
  const describe = (i) => {
    const biome = data.biomes[days[i].biome];
    const zone = days[i].zone;
    return 'DAY ' + i + ' · ' + dateOf(i) +
      ' · ' + (biome ? biome.name.toUpperCase() : '') +
      (zone && zone.n ? ' · ' + zone.n.toUpperCase() : '');
  };
  const dayAtVertex = (ranges, v) => {
    let lo = 0;
    let hi = ranges.length - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (ranges[mid].end <= v) lo = mid + 1;
      else hi = mid;
    }
    const r = ranges[lo];
    return r && v >= r.start ? r.day : -1;
  };
  rig.renderer.domElement.addEventListener('pointermove', (ev) => {
    const r = rig.renderer.domElement.getBoundingClientRect();
    pointer.set(((ev.clientX - r.left) / r.width) * 2 - 1, -((ev.clientY - r.top) / r.height) * 2 + 1);
    raycaster.setFromCamera(pointer, rig.camera);
    const hit = raycaster.intersectObjects(meshes, false)[0];
    const day = hit ? dayAtVertex(hit.object.userData.ranges || [], hit.faceIndex * 3) : -1;
    if (day >= 0) {
      tipEl.textContent = describe(day);
      tipEl.style.left = ev.clientX - r.left + 12 + 'px';
      tipEl.style.top = ev.clientY - r.top + 12 + 'px';
      tipEl.hidden = false;
    } else {
      tipEl.hidden = true;
    }
  });
  rig.renderer.domElement.addEventListener('pointerleave', () => { tipEl.hidden = true; });

  // ── animation ────────────────────────────────────────────────────────────
  // The reveal: the castle assembles plate by plate in day order over ~3s —
  // every buffer keeps day order, so growth is advancing draw ranges (the
  // back meshes share each geometry, so they grow in step for free; the
  // thread's draw range advances with the same day count). After settle the
  // rim lands with its pulse and the bob eases in. Geometry is static
  // throughout. Reduced motion renders the finished castle, holding still.
  const drawnEnd = (marks, shown) => {
    let lo = 0;
    let hi = marks.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (marks[mid].ord < shown) lo = mid + 1;
      else hi = mid;
    }
    return lo === 0 ? 0 : marks[lo - 1].end;
  };
  const t0 = performance.now();
  if (!still) {
    for (const m of meshes) m.geometry.setDrawRange(0, 0);
    if (thread) thread.geometry.setDrawRange(0, 0);
    rim.visible = false;
  }

  rig.start((now) => {
    if (still) return;
    uTime.value = now * 0.0011;
    const k = Math.min(1, (now - t0) / REVEAL_MS);
    const shown = Math.ceil(k * N);
    for (const m of meshes) {
      m.geometry.setDrawRange(0, drawnEnd(m.userData.marks, shown));
    }
    if (thread) {
      thread.geometry.setDrawRange(0, shown < 2 ? 0
        : Math.min(threadVerts, (shown - 1) * THREAD_SPD + 1));
    }
    rim.visible = k >= 1;
    rimMat.opacity = 0.62 + 0.3 * Math.sin((now - t0) / 900);
    uBob.value = k < 1 ? 0 : Math.min(1, (now - t0 - REVEAL_MS) / 1500);
  });

  let tris = 0;
  for (const m of meshes) tris += m.geometry.attributes.position.count / 3;
  console.log('[structure] three r' + THREE.REVISION + ' · castle · ' + N +
    ' plates · ' + meshes.length + ' biome meshes · ' + Math.round(tris / 1000) +
    'k tris ×2 sides · ' + atlases.size + ' neutral glyph atlases · ' +
    'per-day zone inks in vertex color');
}

// ═══ the worldline: one continuous ribbon (?view=line) ═════════════════════
//
// The Hilbert mapping makes consecutive days 4-D lattice neighbors, so the
// day sequence IS a single unbroken path through 16⁴. This scene renders
// that path directly: each day's 4-D cell projects to 3-D (the w axis
// smeared along a fixed diagonal, so w-steps become diagonal moves), the
// lattice walk is smoothed into an organic curve with centripetal
// Catmull-Rom, and a flat cloth cross-section sweeps along it on
// parallel-transport frames — minimal twist, no corner artifacts. The
// surface flexes with each day's own terrain (the server's downsampled
// heightfields read as one continuous strip — the plate's smoothstep
// drape, never stairs) and carries each day's ramp glyphs as printed ink
// from the shared neutral per-biome atlases, colored per day by its zone.
// Biomes drift in weeks-long stretches of shared glyphs while the zone
// inks turn day by day, age fades the print toward the oldest end, and
// today is the ribbon's living tip — an accent cap, pulsing slowly.

// ── tuning ──────────────────────────────────────────────────────────────────
// The 4-D → 3-D projection: p = SCALE · ((x, z, y) + w·DIAG). Every Hilbert
// step changes exactly one axis by 1; the diagonal is deliberately
// incommensurate with the lattice so distinct w-layers never collapse onto
// one another. SCALE spreads the lattice relative to the fixed ribbon
// width — the breathing room that keeps the tangle from fusing into a
// solid mass.
const DIAG = new THREE.Vector3(0.62, 0.46, 0.55);
const SCALE = 2.1; // lattice spacing in scene units
const SPD = 6; // curve samples per day segment
const WSEG = 5; // glyph cells across the ribbon
const WIDTH = 1.18; // ribbon width, scene units
const RELIEF = 0.52; // terrain displacement span along the frame normal
const REVEAL_MS = 3000; // the worldline writes itself over ~3s

async function enhanceLine(plate) {
  const still = reduceMotion();
  const theme = readTheme();

  const res = await fetch(plate.dataset.api, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error('path API ' + res.status);
  const data = await res.json();
  const days = data.days || [];
  if (days.length < 2) throw new Error('path too short for a ribbon');
  const N = days.length; // day count: epoch through today
  const n = N - 1; // today's index
  const hfBands = data.hfBands || 6;
  const ageBands = data.bands || 5;
  const tile = Math.round(Math.sqrt((days[0].hf || []).length)) || 6;

  // ── the path: the 4-D walk, projected and centered ───────────────────────
  const pts = days.map((d) => new THREE.Vector3(
    (d.coord[0] + d.coord[3] * DIAG.x) * SCALE,
    (d.coord[2] + d.coord[3] * DIAG.y) * SCALE,
    (d.coord[1] + d.coord[3] * DIAG.z) * SCALE,
  ));
  const box = new THREE.Box3().setFromPoints(pts);
  const center = box.getCenter(new THREE.Vector3());
  for (const p of pts) p.sub(center);

  // Smooth the right-angled lattice walk into an organic curve. Centripetal
  // parameterization never overshoots or kinks at the walk's corners, and
  // getPoint(t) at t = (i + f)/(N − 1) lands inside day i's span — the
  // curve parameter stays day-addressable.
  const curve = new THREE.CatmullRomCurve3(pts, false, 'centripetal');
  const steps = (N - 1) * SPD;
  const P = new Array(steps + 1);
  for (let s = 0; s <= steps; s++) P[s] = curve.getPoint(s / steps);

  // Parallel-transport frames: tangents by central difference; the normal
  // carried forward with the minimal rotation between tangents (no twist
  // artifacts); the side vector completes the frame and spans the width.
  const T = new Array(steps + 1);
  for (let s = 0; s <= steps; s++) {
    T[s] = P[Math.min(steps, s + 1)].clone().sub(P[Math.max(0, s - 1)]).normalize();
  }
  const Nrm = new Array(steps + 1);
  const Side = new Array(steps + 1);
  const seed = Math.abs(T[0].y) < 0.9 ? new THREE.Vector3(0, 1, 0) : new THREE.Vector3(1, 0, 0);
  Nrm[0] = seed.addScaledVector(T[0], -T[0].dot(seed)).normalize();
  Side[0] = new THREE.Vector3().crossVectors(T[0], Nrm[0]);
  const axis = new THREE.Vector3();
  for (let s = 1; s <= steps; s++) {
    const nrm = Nrm[s - 1].clone();
    axis.crossVectors(T[s - 1], T[s]);
    if (axis.lengthSq() > 1e-12) {
      const ang = Math.acos(THREE.MathUtils.clamp(T[s - 1].dot(T[s]), -1, 1));
      nrm.applyAxisAngle(axis.normalize(), ang);
    }
    nrm.addScaledVector(T[s], -T[s].dot(nrm)).normalize();
    Nrm[s] = nrm;
    Side[s] = new THREE.Vector3().crossVectors(T[s], nrm);
  }

  // ── terrain: every day's tile, read as one continuous strip ─────────────
  // Day tiles stack end to end into an (N·tile) × tile field; smoothstep-
  // bilinear sampling between cell centers (the plate's drape kernel) lets
  // the cloth flex through one day's terrain into the next with no seam.
  const rows = N * tile;
  const strip = new Float32Array(rows * tile);
  for (let i = 0; i < N; i++) {
    const hf = days[i].hf;
    for (let k = 0; k < tile * tile; k++) strip[i * tile * tile + k] = hf[k];
  }
  const smooth01 = (t) => t * t * (3 - 2 * t);
  const stripAt = (r, c) => strip[
    THREE.MathUtils.clamp(r, 0, rows - 1) * tile + THREE.MathUtils.clamp(c, 0, tile - 1)
  ];
  // a: continuous day coordinate in [0, N]; v: across the ribbon in [0, 1].
  const stripSample = (a, v) => {
    const gy = a * tile - 0.5;
    const gx = v * tile - 0.5;
    const j = Math.floor(gy);
    const i = Math.floor(gx);
    const fy = smooth01(gy - j);
    const fx = smooth01(gx - i);
    const s00 = stripAt(j, i);
    const s10 = stripAt(j, i + 1);
    const s01 = stripAt(j + 1, i);
    const s11 = stripAt(j + 1, i + 1);
    return s00 + (s10 - s00) * fx + (s01 - s00) * fy + (s00 - s10 + s11 - s01) * fx * fy;
  };
  const stripA = (s) => (s / steps) * N; // strip day-coordinate at sample s

  // ── the swept surface: one shared vertex grid ────────────────────────────
  // Positions: the curve point, offset across by the side vector and out by
  // the terrain drape (centered on the curve, so the ribbon flexes about
  // its own spine). Normals: from the displaced surface itself, so the
  // folds shade as cloth, not as a flat band.
  const cols = WSEG + 1;
  const GP = new Float32Array((steps + 1) * cols * 3);
  {
    const v3 = new THREE.Vector3();
    for (let s = 0; s <= steps; s++) {
      const a = stripA(s);
      for (let j = 0; j < cols; j++) {
        const h = (stripSample(a, j / WSEG) + 1) / hfBands; // (0, 1]
        v3.copy(P[s])
          .addScaledVector(Side[s], (j / WSEG - 0.5) * WIDTH)
          .addScaledVector(Nrm[s], (h - 0.55) * RELIEF);
        v3.toArray(GP, (s * cols + j) * 3);
      }
    }
  }
  const GN = new Float32Array((steps + 1) * cols * 3);
  {
    const du = new THREE.Vector3();
    const dv = new THREE.Vector3();
    const nv = new THREE.Vector3();
    const pa = new THREE.Vector3();
    const pb = new THREE.Vector3();
    const at = (s, j, out) => out.fromArray(GP, (s * cols + j) * 3);
    for (let s = 0; s <= steps; s++) {
      for (let j = 0; j < cols; j++) {
        at(Math.min(steps, s + 1), j, pa);
        at(Math.max(0, s - 1), j, pb);
        du.subVectors(pa, pb);
        at(s, Math.min(WSEG, j + 1), pa);
        at(s, Math.max(0, j - 1), pb);
        dv.subVectors(pa, pb);
        nv.crossVectors(dv, du).normalize();
        if (nv.dot(Nrm[s]) < 0) nv.negate(); // keep the print side consistent
        nv.toArray(GN, (s * cols + j) * 3);
      }
    }
  }

  // ── ink: age fade, per-day zone palettes, per-biome NEUTRAL atlases ──────
  // Oldest end faintest, floored at 0.55 of full ink — the same compression
  // the SVG plates use, so the 2020 end stays clearly written. The atlases
  // are coverage masks; each day carries its own generated zone palette
  // ({ n, c, w } in the payload), its inks ride the vertex colors, and
  // zonedMaterial mixes surface → ink by coverage.
  const fade = (band) => 1 - (band / Math.max(ageBands - 1, 1)) * 0.45;
  const ageOf = (i) => Math.min(ageBands - 1, Math.floor(((n - i) * ageBands) / (n + 1)));

  // inkFor caches the per-band vertex inks for one day — its own palette at
  // its own age fade.
  const inkCache = new Map();
  const inkFor = (i) => {
    let inks = inkCache.get(i);
    if (!inks) {
      const f = fade(ageOf(i));
      const zc = (days[i].zone && days[i].zone.c) || [];
      const palette = zc.length ? zc.map((c) => new THREE.Color(c)) : [theme.ink];
      inks = [];
      for (let b = 0; b < hfBands; b++) {
        const ink = palette[Math.floor((b * palette.length) / hfBands)];
        inks.push(theme.surface.clone().lerp(ink, f));
      }
      inkCache.set(i, inks);
    }
    return inks;
  };

  const radius = box.getSize(new THREE.Vector3()).length() / 2 + WIDTH;
  const rig = createRig(plate, { theme, viewSize: radius, aspect: 16 / 10, camDist: 2.2 });
  rig.controls.minDistance = radius * 0.22; // close enough to read the print

  // One neutral glyph atlas per biome, built on first use — never one per
  // day, never one per zone.
  const atlases = new Map();
  const atlasFor = (bi) => {
    let a = atlases.get(bi);
    if (!a) {
      const biome = data.biomes[bi] || { name: 'basin' };
      const made = glyphAtlas({
        ramp: RAMPS[biome.name] || RAMPS.basin,
        bands: hfBands,
      });
      a = { uvFor: made.uvFor, texture: coverageTexture(rig.renderer, made.canvas) };
      atlases.set(bi, a);
    }
    return a;
  };

  // ── ribbon geometry: per-biome quad soup, appended in day order ──────────
  // One mesh per biome (one atlas each); within each, vertices land in day
  // order, so the growth reveal is just advancing draw ranges, and a raycast
  // hit maps back to its day through recorded vertex ranges. The `along`
  // attribute (continuous day coordinate) drives the traveling undulation
  // in the vertex shader; the `color` attribute carries each day's zone
  // ink — geometry never updates after the reveal.
  class RibbonBuilder {
    constructor() {
      this.pos = [];
      this.nrm = [];
      this.uv = [];
      this.col = [];
      this.along = [];
      this.marks = []; // { ord, end } per day, for the reveal
      this.ranges = []; // { start, end, day } per day, for hover
    }

    get vertexCount() {
      return this.pos.length / 3;
    }

    vert(o, u, v, a, ink) {
      this.pos.push(GP[o], GP[o + 1], GP[o + 2]);
      this.nrm.push(GN[o], GN[o + 1], GN[o + 2]);
      this.uv.push(u, v);
      this.col.push(ink.r, ink.g, ink.b);
      this.along.push(a);
    }

    geometry() {
      const geo = new THREE.BufferGeometry();
      geo.setAttribute('position', new THREE.Float32BufferAttribute(this.pos, 3));
      geo.setAttribute('normal', new THREE.Float32BufferAttribute(this.nrm, 3));
      geo.setAttribute('uv', new THREE.Float32BufferAttribute(this.uv, 2));
      geo.setAttribute('color', new THREE.Float32BufferAttribute(this.col, 3));
      geo.setAttribute('along', new THREE.Float32BufferAttribute(this.along, 1));
      return geo;
    }
  }

  const builders = new Map(); // biome → RibbonBuilder
  for (let i = 0; i < N - 1; i++) {
    const bi = days[i].biome;
    let b = builders.get(bi);
    if (!b) {
      b = new RibbonBuilder();
      builders.set(bi, b);
    }
    const start = b.vertexCount;
    const inks = inkFor(i);
    const { uvFor } = atlasFor(bi);
    for (let s = i * SPD; s < (i + 1) * SPD; s++) {
      const aMid = stripA(s + 0.5);
      const a0 = s / SPD;
      const a1 = (s + 1) / SPD;
      for (let j = 0; j < WSEG; j++) {
        const band = THREE.MathUtils.clamp(
          Math.round(stripSample(aMid, (j + 0.5) / WSEG)), 0, hfBands - 1,
        );
        const r = uvFor(band);
        const ink = inks[band];
        const o00 = (s * cols + j) * 3;
        const o01 = (s * cols + j + 1) * 3;
        const o10 = ((s + 1) * cols + j) * 3;
        const o11 = ((s + 1) * cols + j + 1) * 3;
        b.vert(o00, r.u0, r.vLo, a0, ink);
        b.vert(o01, r.u1, r.vLo, a0, ink);
        b.vert(o11, r.u1, r.vHi, a1, ink);
        b.vert(o00, r.u0, r.vLo, a0, ink);
        b.vert(o11, r.u1, r.vHi, a1, ink);
        b.vert(o10, r.u0, r.vHi, a1, ink);
      }
    }
    b.marks.push({ ord: i, end: b.vertexCount });
    b.ranges.push({ start, end: b.vertexCount, day: i });
  }

  // The barely-perceptible traveling undulation: a vertex-shader ripple
  // along the day coordinate — about a 12-day wavelength crawling slowly
  // toward the tip. Skipped wholesale under reduced motion.
  const uTime = { value: 0 };
  const ripplePatch = still ? null : (sh) => {
    sh.uniforms.uTime = uTime;
    sh.vertexShader = sh.vertexShader
      .replace('#include <common>', '#include <common>\nattribute float along;\nuniform float uTime;')
      .replace('#include <begin_vertex>',
        '#include <begin_vertex>\ntransformed += normal * 0.045 * sin(along * 0.53 - uTime);');
  };
  const rippleKey = still ? 'line' : 'line-ripple';

  // Front: the printed face. Back: the same cloth seen from behind —
  // shared geometry redrawn dark, the plate fabric's underside. Both are
  // zonedMaterials: neutral atlas coverage × per-vertex zone ink.
  const meshes = []; // front meshes — raycast targets, reveal owners
  for (const [bi, b] of [...builders.entries()].sort((x, y) => x[0] - y[0])) {
    const geo = b.geometry();
    const front = new THREE.Mesh(geo, zonedMaterial({
      map: atlasFor(bi).texture, surface: theme.surface,
      patchVertex: ripplePatch, cacheKey: rippleKey,
    }));
    const back = new THREE.Mesh(geo, zonedMaterial({
      map: atlasFor(bi).texture, surface: theme.surface, back: true,
      patchVertex: ripplePatch, cacheKey: rippleKey,
    }));
    front.userData = { marks: b.marks, ranges: b.ranges };
    rig.scene.add(front, back);
    meshes.push(front);
  }

  // Today: the ribbon's living end — an accent cap sealing the open
  // cross-section at the tip, pulsing slowly once the line has been written.
  // The walk's frontier usually sits deep inside the knot, so the cap draws
  // through occlusion (no depth test) — a survey locator, never lost, the
  // way the old scene's accent rim ignored the fog.
  const tipMat = new THREE.MeshBasicMaterial({
    color: theme.accent, side: THREE.DoubleSide, transparent: true, opacity: 0.9,
    depthTest: false,
  });
  const tip = (() => {
    const c = P[steps].clone().addScaledVector(T[steps], 0.03);
    const sw = WIDTH * 0.62;
    const nh = RELIEF * 0.75;
    const corner = (ks, kn) =>
      c.clone().addScaledVector(Side[steps], ks * sw).addScaledVector(Nrm[steps], kn * nh);
    const a = corner(-1, -1);
    const b = corner(1, -1);
    const d = corner(1, 1);
    const e = corner(-1, 1);
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.Float32BufferAttribute([
      ...a.toArray(), ...b.toArray(), ...d.toArray(),
      ...a.toArray(), ...d.toArray(), ...e.toArray(),
    ], 3));
    const m = new THREE.Mesh(geo, tipMat);
    m.renderOrder = 1; // after the cloth, so depthTest:false lands on top
    return m;
  })();
  rig.scene.add(tip);

  // ── swap the SVG for the canvas ──────────────────────────────────────────
  rig.mount();

  // ── hover tooltip: DAY n · date · biome · zone, raycast → vertex → day ───
  const tipEl = document.createElement('div');
  tipEl.className = 'structure-tip';
  tipEl.hidden = true;
  plate.appendChild(tipEl);

  const raycaster = new THREE.Raycaster();
  const pointer = new THREE.Vector2();
  const epochMs = Date.UTC(...(data.epoch || '2020-01-01').split('-')
    .map(Number).map((v, i) => (i === 1 ? v - 1 : v)));
  const dateOf = (i) => new Date(epochMs + i * 86400000).toISOString().slice(0, 10);
  const describe = (i) => {
    const biome = data.biomes[days[i].biome];
    const zone = days[i].zone;
    return 'DAY ' + i + ' · ' + dateOf(i) +
      ' · ' + (biome ? biome.name.toUpperCase() : '') +
      (zone && zone.n ? ' · ' + zone.n.toUpperCase() : '');
  };
  const dayAtVertex = (ranges, v) => {
    let lo = 0;
    let hi = ranges.length - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (ranges[mid].end <= v) lo = mid + 1;
      else hi = mid;
    }
    const r = ranges[lo];
    return r && v >= r.start ? r.day : -1;
  };
  rig.renderer.domElement.addEventListener('pointermove', (ev) => {
    const r = rig.renderer.domElement.getBoundingClientRect();
    pointer.set(((ev.clientX - r.left) / r.width) * 2 - 1, -((ev.clientY - r.top) / r.height) * 2 + 1);
    raycaster.setFromCamera(pointer, rig.camera);
    const hit = raycaster.intersectObjects(meshes, false)[0];
    const day = hit ? dayAtVertex(hit.object.userData.ranges || [], hit.faceIndex * 3) : -1;
    if (day >= 0) {
      tipEl.textContent = describe(day);
      tipEl.style.left = ev.clientX - r.left + 12 + 'px';
      tipEl.style.top = ev.clientY - r.top + 12 + 'px';
      tipEl.hidden = false;
    } else {
      tipEl.hidden = true;
    }
  });
  rig.renderer.domElement.addEventListener('pointerleave', () => { tipEl.hidden = true; });

  // ── animation ────────────────────────────────────────────────────────────
  // The reveal: the worldline writes itself from day 0 to today over ~3s —
  // every buffer keeps day order, so growth is advancing draw ranges (the
  // back mesh shares each geometry, so it grows in step for free). Then the
  // tip lands with its pulse and the slow undulation keeps the cloth alive.
  // Geometry is static throughout. Reduced motion renders the finished form.
  const total = N - 1;
  const drawnEnd = (marks, shown) => {
    let lo = 0;
    let hi = marks.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (marks[mid].ord < shown) lo = mid + 1;
      else hi = mid;
    }
    return lo === 0 ? 0 : marks[lo - 1].end;
  };
  const t0 = performance.now();
  if (!still) {
    for (const m of meshes) m.geometry.setDrawRange(0, 0);
    tip.visible = false;
  }

  rig.start((now) => {
    if (still) return;
    uTime.value = now * 0.0009;
    const k = Math.min(1, (now - t0) / REVEAL_MS);
    const shown = Math.ceil(k * total);
    for (const m of meshes) {
      m.geometry.setDrawRange(0, drawnEnd(m.userData.marks, shown));
    }
    tip.visible = k >= 1;
    tipMat.opacity = 0.62 + 0.3 * Math.sin((now - t0) / 900);
  });

  let tris = 0;
  for (const m of meshes) tris += m.geometry.attributes.position.count / 3;
  console.log('[structure] three r' + THREE.REVISION + ' · worldline · ' + N +
    ' days · ' + meshes.length + ' biome meshes · ' + Math.round(tris / 1000) +
    'k tris ×2 sides · ' + atlases.size + ' neutral glyph atlases · ' +
    'per-day zone inks in vertex color');
}
