// structure.js — progressive enhancement for /art/structure: swaps the
// server-rendered SVG (today's slice) for a live three.js scene of the
// whole structure — every cell accreted since the epoch. The SVG remains
// the no-JS / no-WebGL fallback; if anything here fails, the SVG stays put.
//
// Every non-empty w-slice renders as a solid terraced massif: each cell is
// its own day's heightfield (downsampled by the server; the client never
// re-derives noise) terraced into the cell's upper band, with band-ink
// cliff walls; faces between occupied neighbors are culled so a contiguous
// region reads as one mass. The massifs stand in a single row, earliest
// volume on the left through the frontier on the right, a ghost outline
// one volume further hinting at future growth. The accretion reveal builds
// the entire row in day order; geometry, lights, and orbit come from
// terrain.js, shared with the plate page — one world, zoomed out.

import * as THREE from 'three';
import { readTheme, reduceMotion, TileBuilder, terrainMaterial, createRig } from 'terrain';

const plate = document.getElementById('structure-plate');
if (plate && plate.dataset.api) {
  enhance(plate).catch((err) => {
    // Keep the SVG fallback; just note why the canvas did not come up.
    console.warn('[structure] keeping SVG fallback:', err);
  });
}

async function enhance(plate) {
  const still = reduceMotion();
  const theme = readTheme();

  const res = await fetch(plate.dataset.api, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error('structure API ' + res.status);
  const data = await res.json();
  const side = data.side || 16;
  const half = side / 2;

  // The row: one slot per non-empty slice in w order, ~5 lattice units of
  // air between volumes, plus one empty slot past the frontier.
  const gapU = 5;
  const pitch = side + gapU;
  const slices = (data.slices || []).slice().sort((a, b) => a.w - b.w);
  const slots = slices.length + 1; // the ghost volume
  const rowLen = slots * side + (slots - 1) * gapU;
  const slotX0 = (s) => s * pitch - rowLen / 2; // volume spans x ∈ [slotX0, slotX0+side]

  const rig = createRig(plate, { theme, viewSize: rowLen / 2, aspect: 5 / 2 });
  const { scene } = rig;

  // One shared ground grid under the whole row, restyled to the hairlines.
  {
    const pos = [];
    for (let x = 0; x <= rowLen; x++) pos.push(x - rowLen / 2, -half, -half, x - rowLen / 2, -half, half);
    for (let z = -half; z <= half; z++) pos.push(-rowLen / 2, -half, z, rowLen / 2, -half, z);
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.Float32BufferAttribute(pos, 3));
    scene.add(new THREE.LineSegments(
      geo,
      new THREE.LineBasicMaterial({ color: theme.ink, transparent: true, opacity: 0.1 }),
    ));
  }

  // Volume outlines: a faint box around the frontier (the slice still
  // filling — the one today's cell lands in) and a fainter ghost for the
  // next, still-empty volume. Full massifs need no box — they are the box.
  const tw = (data.today && data.today.coord && data.today.coord[3]) ?? -1;
  const frontierSlot = slices.findIndex((sl) => sl.w === tw);
  const volumeBox = (slot, opacity) => {
    const box = new THREE.LineSegments(
      new THREE.EdgesGeometry(new THREE.BoxGeometry(side, side, side)),
      new THREE.LineBasicMaterial({ color: theme.ink, transparent: true, opacity }),
    );
    box.position.set(slotX0(slot) + half, 0, 0);
    scene.add(box);
    return box;
  };
  if (frontierSlot >= 0) volumeBox(frontierSlot, 0.28);
  volumeBox(slots - 1, 0.12); // the ghost — future growth

  // ── cells: full-height terrain-topped columns ────────────────────────────
  // Lattice (x, y, z) with z up maps to scene (x, z, y) with Y up; each
  // slice's volume is offset along scene X by its slot. Age fade tracks the
  // SVG, compressed so the 2020 core stays clearly inked.
  const ageBands = data.bands || 5;
  const hfBands = data.hfBands || 6;
  const fade = (band) => 1 - (band / Math.max(ageBands - 1, 1)) * 0.45;
  const palettes = (data.biomes || []).map((b) => b.colors.map((c) => new THREE.Color(c)));

  // Per-slice occupancy, so faces between two occupied cells are culled —
  // a contiguous region reads as one solid mass, not stacked trays. Cells
  // arrive grouped by slice; the reveal wants them in one day-ordered run.
  const key = (x, y, z) => (x << 8) | (y << 4) | z;
  const all = [];
  for (let s = 0; s < slices.length; s++) {
    const occ = new Set(slices[s].cells.map((c) => key(c.x, c.y, c.z)));
    for (const c of slices[s].cells) all.push({ c, slot: s, occ });
  }
  all.sort((a, b) => a.c.i - b.c.i);

  const inLattice = (x, y, z) => x >= 0 && y >= 0 && z >= 0 && x < side && y < side && z < side;
  const openAt = (occ, x, y, z) => !inLattice(x, y, z) || !occ.has(key(x, y, z));

  // Each cell fills its unit lattice cell: cliff walls from the cell floor,
  // the terraced terrain carved into the upper ~40%.
  const cellOpts = ({ c, slot, occ }) => ({
    levels: c.hf,
    bands: hfBands,
    palette: palettes[c.biome] || [theme.ink, theme.ink, theme.ink],
    surface: theme.surface,
    fade: fade(c.age),
    x: slotX0(slot) + c.x + 0.5,
    z: c.y - half + 0.5,
    y: c.z - half,
    size: 1,
    height: 1,
    relief: 0.42,
    open: {
      top: openAt(occ, c.x, c.y, c.z + 1),
      bottom: openAt(occ, c.x, c.y, c.z - 1),
      px: openAt(occ, c.x + 1, c.y, c.z),
      nx: openAt(occ, c.x - 1, c.y, c.z),
      pz: openAt(occ, c.x, c.y + 1, c.z),
      nz: openAt(occ, c.x, c.y - 1, c.z),
    },
  });

  // One merged geometry for every past cell, appended in day order across
  // the whole row; the accretion animation is just a growing draw range,
  // and a raycast hit maps back to its cell through the recorded vertex
  // ranges. Buried cells ship no tile and render nothing — skip them.
  const builder = new TileBuilder();
  const ranges = [];
  let todayEntry = null;
  for (const entry of all) {
    if (entry.c.today) {
      todayEntry = entry;
      continue;
    }
    if (!entry.c.hf || entry.c.hf.length === 0) continue; // fully buried
    const start = builder.vertexCount;
    builder.column(cellOpts(entry));
    ranges.push({ start, end: builder.vertexCount, cell: entry.c });
  }
  const mass = new THREE.Mesh(builder.geometry(), terrainMaterial());
  mass.visible = ranges.length > 0;
  scene.add(mass);

  // Today's cell — its own near-plate-resolution column, marked by an
  // accent hairline around the unit cell: an instrument pointer, not a
  // beacon. Built at a local origin so its slight pulse scales in place.
  let todayMesh = null;
  const todayCell = todayEntry ? todayEntry.c : null;
  if (todayEntry) {
    const o = cellOpts(todayEntry);
    const tb = new TileBuilder();
    tb.column({ ...o, x: 0, z: 0, y: 0 });
    todayMesh = new THREE.Mesh(tb.geometry(), terrainMaterial());
    todayMesh.position.set(o.x, o.y, o.z);
    todayMesh.userData.cell = todayCell;
    const rim = new THREE.LineSegments(
      new THREE.EdgesGeometry(new THREE.BoxGeometry(1, 1, 1)),
      new THREE.LineBasicMaterial({ color: theme.accent }),
    );
    rim.position.y = 0.5;
    todayMesh.add(rim);
    scene.add(todayMesh);
  }

  // ── swap the SVG for the canvas ──────────────────────────────────────────
  rig.mount();

  // Frame the whole row: the occupied cells plus the frontier and ghost
  // volumes, fit to the camera with a little padding, viewed from a gentle
  // three-quarter front so the timeline reads left to right.
  {
    const bbox = new THREE.Box3();
    for (const { c, slot } of all) {
      const x0 = slotX0(slot) + c.x;
      bbox.expandByPoint(new THREE.Vector3(x0, c.z - half, c.y - half));
      bbox.expandByPoint(new THREE.Vector3(x0 + 1, c.z - half + 1, c.y - half + 1));
    }
    for (const slot of [frontierSlot, slots - 1]) {
      if (slot < 0) continue;
      bbox.expandByPoint(new THREE.Vector3(slotX0(slot), -half, -half));
      bbox.expandByPoint(new THREE.Vector3(slotX0(slot) + side, half, half));
    }
    const center = bbox.getCenter(new THREE.Vector3());
    const sizeV = bbox.getSize(new THREE.Vector3());
    const vFov = (rig.camera.fov * Math.PI) / 180;
    const hFov = 2 * Math.atan(Math.tan(vFov / 2) * rig.camera.aspect);
    // Fit width against the horizontal field and height/depth against the
    // vertical — a row this wide overflows a bounding-sphere fit's frame.
    const dist = Math.max(
      (sizeV.x / 2) * 1.18 / Math.tan(hFov / 2),
      (Math.max(sizeV.y, sizeV.z) / 2) * 1.3 / Math.tan(vFov / 2),
    );
    rig.controls.target.copy(center);
    rig.camera.position.set(0.12, 0.45, 1).normalize().multiplyScalar(dist).add(center);
    rig.controls.minDistance = dist * 0.25;
    rig.controls.maxDistance = dist * 2;
    rig.controls.update();
  }

  const tip = document.createElement('div');
  tip.className = 'structure-tip';
  tip.hidden = true;
  plate.appendChild(tip);

  // ── hover tooltip ────────────────────────────────────────────────────────
  const raycaster = new THREE.Raycaster();
  const pointer = new THREE.Vector2();
  const epoch = Date.UTC(...(data.epoch || '2020-01-01').split('-').map(Number).map((v, i) => (i === 1 ? v - 1 : v)));
  const dateOf = (i) => new Date(epoch + i * 86400000).toISOString().slice(0, 10);
  const describe = (c) => {
    const biome = data.biomes[c.biome];
    return 'DAY ' + c.i + ' · ' + dateOf(c.i) + ' · ' + (biome ? biome.name.toUpperCase() : '');
  };
  const cellAtVertex = (v) => {
    let lo = 0;
    let hi = ranges.length - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (ranges[mid].end <= v) lo = mid + 1;
      else hi = mid;
    }
    return ranges[lo] && v >= ranges[lo].start ? ranges[lo].cell : null;
  };

  rig.renderer.domElement.addEventListener('pointermove', (ev) => {
    const r = rig.renderer.domElement.getBoundingClientRect();
    pointer.set(((ev.clientX - r.left) / r.width) * 2 - 1, -((ev.clientY - r.top) / r.height) * 2 + 1);
    raycaster.setFromCamera(pointer, rig.camera);
    const targets = todayMesh ? [mass, todayMesh] : [mass];
    const hit = raycaster.intersectObjects(targets, false)[0];
    let cell = null;
    if (hit) cell = hit.object === todayMesh ? todayCell : cellAtVertex(hit.faceIndex * 3);
    if (cell) {
      tip.textContent = describe(cell);
      tip.style.left = ev.clientX - r.left + 12 + 'px';
      tip.style.top = ev.clientY - r.top + 12 + 'px';
      tip.hidden = false;
    } else {
      tip.hidden = true;
    }
  });
  rig.renderer.domElement.addEventListener('pointerleave', () => { tip.hidden = true; });

  // ── animation ────────────────────────────────────────────────────────────
  // Accretion: the whole history builds in day order over ~3s (the merged
  // buffer keeps day order, so the reveal is one growing draw range
  // sweeping along the row), then today's cell lands with its accent rim
  // and a very slight breathing pulse. Instead of the plate's full
  // turntable — which would put a row this wide end-on — the camera sways
  // gently about its azimuth. Reduced motion renders the final state.
  const accretionMs = 3000;
  const t0 = performance.now();
  if (!still) {
    mass.geometry.setDrawRange(0, 0);
    mass.visible = false;
    if (todayMesh) todayMesh.visible = false;
  }

  rig.start((now) => {
    if (still) return;
    rig.controls.autoRotateSpeed = 0.4 * Math.cos((now - t0) / 6500); // ±~10° sway
    const t = now - t0;
    const k = Math.min(1, t / accretionMs);
    const shown = Math.floor(k * ranges.length);
    mass.geometry.setDrawRange(0, shown > 0 ? ranges[shown - 1].end : 0);
    mass.visible = shown > 0;
    if (todayMesh) {
      todayMesh.visible = k >= 1;
      const s = 1 + 0.02 * Math.sin((now - t0) / 700); // subtle instrument pulse
      todayMesh.scale.setScalar(s);
    }
  });

  console.log('[structure] three r' + THREE.REVISION + ' · ' + slices.length +
    ' volumes · ' + all.length + ' cells live');
}
