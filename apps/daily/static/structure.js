// structure.js — progressive enhancement for /art/structure: swaps the
// server-rendered SVG (today's slice) for a live three.js scene of the
// whole structure — every cell accreted since the epoch. The SVG remains
// the no-JS / no-WebGL fallback; if anything here fails, the SVG stays put.
//
// Every occupied cell renders as its own day's fabric sheet — the day's
// heightfield (downsampled by the server; the client never re-derives
// noise) smoothly draped across the cell, printed with the day's ramp
// glyphs, skirted and dark-backed so a column of days reads as a laminated
// sheaf of survey sheets. Glyphs come from per-biome texture atlases (one
// canvas per biome, tiles by elevation band × age fade — eight textures,
// never one per day). The volumes stand in a receding procession: the
// oldest completed volume looms nearest the camera, each younger volume a
// step further into depth and a little across, through the sparse frontier
// (the one holding today) to the ghost outline of the next empty volume
// deepest of all — history looms, today far. The accretion reveal stacks
// the sheets in day order, the procession building from the viewer's feet
// toward the horizon; the print,
// drape, lights, and orbit come from terrain.js, shared with the plate
// page — the plate is one sheet up close, this is the archive of all of
// them.

import * as THREE from 'three';
import {
  readTheme, reduceMotion, createRig,
  RAMPS, glyphAtlas, fabricTexture, fabricMaterial,
  FabricBuilder, InkBuilder, inkMaterial,
} from 'terrain';

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

  // The procession: one slot per non-empty slice in w order, with the same
  // ~5 lattice units of air between volumes, plus one empty slot past the
  // last slice. The oldest slot sits at the origin, nearest the camera;
  // each younger slot steps further along -Z and a little +X — a gentle
  // diagonal receding into depth — through the sparse frontier slot (the
  // slice today's sheet lands in) to the ghost, deepest of all.
  const gapU = 5;
  const pitch = side + gapU;
  const slices = (data.slices || []).slice().sort((a, b) => a.w - b.w);
  const slots = slices.length + 1; // the ghost volume
  const tw = (data.today && data.today.coord && data.today.coord[3]) ?? -1;
  let frontierSlot = slices.findIndex((sl) => sl.w === tw);
  if (frontierSlot < 0) frontierSlot = slices.length - 1;
  const drift = (12 * Math.PI) / 180; // mostly depth, a little across
  const stepX = pitch * Math.sin(drift);
  const stepZ = pitch * Math.cos(drift);
  // Volume center for a slot, by how many steps it sits behind the oldest.
  const slotPos = (s) => ({ x: s * stepX, z: -s * stepZ });

  const rig = createRig(plate, { theme, viewSize: (slots * pitch) / 2, aspect: 5 / 2 });
  const { scene } = rig;

  // One shared ground grid under the whole procession, restyled to the
  // hairlines — a survey sheet wide enough for every volume's footprint.
  {
    const near = slotPos(slots - 1);
    const far = slotPos(0);
    const x0 = Math.floor(Math.min(near.x, far.x) - half) - 2;
    const x1 = Math.ceil(Math.max(near.x, far.x) + half) + 2;
    const z0 = Math.floor(Math.min(near.z, far.z) - half) - 2;
    const z1 = Math.ceil(Math.max(near.z, far.z) + half) + 2;
    const pos = [];
    for (let x = x0; x <= x1; x++) pos.push(x, -half, z0, x, -half, z1);
    for (let z = z0; z <= z1; z++) pos.push(x0, -half, z, x1, -half, z);
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.Float32BufferAttribute(pos, 3));
    scene.add(new THREE.LineSegments(
      geo,
      new THREE.LineBasicMaterial({ color: theme.ink, transparent: true, opacity: 0.1 }),
    ));
  }

  // Volume outlines: a faint box around the frontier (the slice still
  // filling — the one today's sheet lands in) and a fainter ghost for the
  // next, still-empty volume. Full massifs need no box — they are the box.
  const volumeBox = (slot, opacity) => {
    const box = new THREE.LineSegments(
      new THREE.EdgesGeometry(new THREE.BoxGeometry(side, side, side)),
      new THREE.LineBasicMaterial({ color: theme.ink, transparent: true, opacity }),
    );
    const p = slotPos(slot);
    box.position.set(p.x, 0, p.z);
    scene.add(box);
    return box;
  };
  volumeBox(frontierSlot, 0.28);
  volumeBox(slots - 1, 0.12); // the ghost — future growth, deepest of all

  // ── cells: one fabric sheet per day ──────────────────────────────────────
  // Lattice (x, y, z) with z up maps to scene (x, z, y) with Y up; each
  // slice's volume is offset along scene X by its slot. Age fade tracks the
  // SVG, compressed so the 2020 core stays clearly inked.
  const ageBands = data.bands || 5;
  const hfBands = data.hfBands || 6;
  const fade = (band) => 1 - (band / Math.max(ageBands - 1, 1)) * 0.45;
  const palettes = (data.biomes || []).map((b) => b.colors.map((c) => new THREE.Color(c)));

  // Each sheet drapes the full cell height, so the top band touches the
  // sheet above — laminated sheaves of days; the hair of clearance keeps
  // touching faces from z-fighting. The lift compresses the folds onto a
  // thicker body: six samples per cell at full band span reads as crumpled
  // rag, not a survey sheet.
  const relief = 0.96;
  const lift = 0.35;

  // One glyph atlas per biome, built on first use — never one per day.
  const atlases = new Map();
  const atlasFor = (bi) => {
    let a = atlases.get(bi);
    if (!a) {
      const biome = data.biomes[bi] || { name: 'basin' };
      const made = glyphAtlas({
        ramp: RAMPS[biome.name] || RAMPS.basin,
        palette: palettes[bi] || [theme.ink, theme.ink, theme.ink],
        surface: theme.surface,
        bands: hfBands,
        ages: ageBands,
        fade,
      });
      a = { uvFor: made.uvFor, texture: fabricTexture(rig.renderer, made.canvas) };
      atlases.set(bi, a);
    }
    return a;
  };

  // Cells arrive grouped by slice; the reveal wants one day-ordered run.
  const all = [];
  for (let s = 0; s < slices.length; s++) {
    for (const c of slices[s].cells) all.push({ c, slot: s });
  }
  all.sort((a, b) => a.c.i - b.c.i);

  const sheetOpts = ({ c, slot }) => {
    const p = slotPos(slot);
    return {
      levels: c.hf,
      bands: hfBands,
      x: p.x - half + c.x + 0.5,
      z: p.z - half + c.y + 0.5,
      y: c.z - half,
      size: 1,
      relief,
      lift,
    };
  };

  // The merged scene: one printed-top builder per biome (one atlas each)
  // plus one ink builder for every skirt and underside, all appended in day
  // order — the accretion animation is just growing draw ranges, and a
  // raycast hit maps back to its day through recorded vertex ranges.
  // Buried cells ship no heightfield and render nothing — skip them.
  const tops = new Map(); // biome → { builder, marks, ranges }
  const skin = { builder: new InkBuilder(), marks: [], ranges: [] };
  const mark = (rec, ord, start, cell) => {
    rec.marks.push({ ord, end: rec.builder.vertexCount });
    rec.ranges.push({ start, end: rec.builder.vertexCount, cell });
  };
  let ord = 0;
  let todayEntry = null;
  for (const entry of all) {
    if (entry.c.today) {
      todayEntry = entry;
      continue;
    }
    if (!entry.c.hf || entry.c.hf.length === 0) continue; // fully buried
    const o = sheetOpts(entry);
    const bi = entry.c.biome;
    let top = tops.get(bi);
    if (!top) {
      top = { builder: new FabricBuilder(), marks: [], ranges: [] };
      tops.set(bi, top);
    }
    const tStart = top.builder.vertexCount;
    top.builder.sheet({ ...o, uvFor: atlasFor(bi).uvFor, age: entry.c.age });
    mark(top, ord, tStart, entry.c);
    const sStart = skin.builder.vertexCount;
    skin.builder.sheetSides({
      ...o,
      palette: palettes[bi] || [theme.ink, theme.ink, theme.ink],
      surface: theme.surface,
      fade: fade(entry.c.age),
    });
    mark(skin, ord, sStart, entry.c);
    ord++;
  }
  const meshes = [];
  for (const [bi, top] of atlasOrdered(tops)) {
    const mesh = new THREE.Mesh(top.builder.geometry(), fabricMaterial(atlasFor(bi).texture));
    mesh.userData = { marks: top.marks, ranges: top.ranges };
    scene.add(mesh);
    meshes.push(mesh);
  }
  {
    const mesh = new THREE.Mesh(skin.builder.geometry(), inkMaterial());
    mesh.userData = { marks: skin.marks, ranges: skin.ranges };
    scene.add(mesh);
    meshes.push(mesh);
  }

  // Today's sheet — its own near-plate-resolution drape, full ink, marked
  // by an accent hairline around the unit cell: an instrument pointer, not
  // a beacon. Built at a local origin so its slight pulse scales in place.
  let todayGroup = null;
  let todayRim = null;
  const todayCell = todayEntry ? todayEntry.c : null;
  if (todayEntry) {
    const o = sheetOpts(todayEntry);
    const bi = todayEntry.c.biome;
    const local = { ...o, x: 0, z: 0, y: 0 };
    const tb = new FabricBuilder();
    tb.sheet({ ...local, uvFor: atlasFor(bi).uvFor, age: 0 });
    const ib = new InkBuilder();
    ib.sheetSides({
      ...local,
      palette: palettes[bi] || [theme.ink, theme.ink, theme.ink],
      surface: theme.surface,
      fade: 1,
    });
    todayGroup = new THREE.Group();
    const topMesh = new THREE.Mesh(tb.geometry(), fabricMaterial(atlasFor(bi).texture));
    const sideMesh = new THREE.Mesh(ib.geometry(), inkMaterial());
    topMesh.userData.cell = todayCell;
    sideMesh.userData.cell = todayCell;
    todayGroup.add(topMesh, sideMesh);
    // A hair larger than the cell, so the hairline never lands inside the
    // sheet's own skirt planes. The rim ignores the scene fog: it sits at
    // the deep end of the procession now, and a fogged accent would wash
    // to the page color — a small pointer is fine, an invisible one is not.
    const rim = new THREE.LineSegments(
      new THREE.EdgesGeometry(new THREE.BoxGeometry(1.03, 1.03, 1.03)),
      new THREE.LineBasicMaterial({ color: theme.accent, fog: false }),
    );
    rim.position.y = 0.5;
    todayRim = rim;
    todayGroup.add(rim);
    todayGroup.position.set(o.x, o.y, o.z);
    scene.add(todayGroup);
  }

  // ── swap the SVG for the canvas ──────────────────────────────────────────
  rig.mount();

  // Frame the procession: the oldest massif is the subject — the camera
  // close and low so it looms at about half the frame height, its laminated
  // strata the first thing read — while the younger volumes shrink along
  // the diagonal to the sparse frontier and the ghost, the deepest hazed by
  // fog. The framing measures what is actually built in slot 0 (today an
  // 8×8×8 corner block; one day the full volume), not the volume's mostly
  // empty bounding cube. Enough downward angle survives that the receding
  // sheet roofs stay visible. The orbit pivot sits a little way down the
  // procession, not on the massif, so the gentle azimuth sway turns the
  // whole line without throwing either end out of frame.
  {
    const built = new THREE.Box3();
    for (const c of slices[0].cells) {
      built.expandByPoint(new THREE.Vector3(c.x - half + 0.5, c.z - half + 0.5, c.y - half + 0.5));
    }
    built.expandByScalar(0.5);
    const massif = built.getCenter(new THREE.Vector3());
    const vFov = (rig.camera.fov * Math.PI) / 180;
    // Stand back just far enough that the massif fills ~55% of the frame
    // height; ~29° off the procession axis and ~18° above it, so the
    // younger volumes separate and recede across the frame instead of
    // hiding behind the massif — and the sight line down to today's cell
    // clears the mid-line stacks across the whole sway.
    const subjectDist = (built.max.y - built.min.y) / (0.55 * 2 * Math.tan(vFov / 2));
    rig.camera.position.set(0.49, 0.33, 0.88).normalize()
      .multiplyScalar(subjectDist).add(massif);
    // The pivot leans a little left of the line and below the massif's
    // waist: the massif rides high in the frame, whole, while the line
    // recedes toward the upper right. It sits only a little way down the
    // procession — close to the massif — so the sway barely moves the
    // looming subject and spends its motion on the far, hazed end.
    rig.controls.target.set(1.2 * stepX - 6, massif.y - 3.5, -1.2 * stepZ);
    const dist = rig.camera.position.distanceTo(rig.controls.target);
    rig.controls.minDistance = dist * 0.35;
    rig.controls.maxDistance = dist * 2.5;
    rig.controls.update();
    // Distance haze, matched to the surface so the frontier and the ghost
    // sink toward the page rather than gray out — instrument-quiet
    // recession, scaled to the line's actual depth.
    const g = slotPos(slots - 1);
    const ghostDist = rig.camera.position.distanceTo(new THREE.Vector3(g.x, 0, g.z));
    scene.fog = new THREE.Fog(theme.surface, subjectDist * 2.2, ghostDist * 1.5);
    // Today's rim ignores the fog (it must stay findable at the deep end of
    // the line) and widens with distance — at range it reads as a locator
    // bracket around the sheet, never washing out to a lost pixel.
    if (todayRim) {
      const d = rig.camera.position.distanceTo(todayGroup.position);
      todayRim.scale.setScalar(THREE.MathUtils.clamp(d / 55, 1, 3.5));
    }
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
  const cellAtVertex = (ranges, v) => {
    let lo = 0;
    let hi = ranges.length - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (ranges[mid].end <= v) lo = mid + 1;
      else hi = mid;
    }
    return ranges[lo] && v >= ranges[lo].start ? ranges[lo].cell : null;
  };

  const hoverTargets = todayGroup
    ? [...meshes, ...todayGroup.children.filter((m) => m.isMesh)]
    : meshes;
  rig.renderer.domElement.addEventListener('pointermove', (ev) => {
    const r = rig.renderer.domElement.getBoundingClientRect();
    pointer.set(((ev.clientX - r.left) / r.width) * 2 - 1, -((ev.clientY - r.top) / r.height) * 2 + 1);
    raycaster.setFromCamera(pointer, rig.camera);
    const hit = raycaster.intersectObjects(hoverTargets, false)[0];
    let cell = null;
    if (hit) {
      cell = hit.object.userData.cell ||
        cellAtVertex(hit.object.userData.ranges || [], hit.faceIndex * 3);
    }
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
  // Accretion: the whole archive stacks in day order over ~3s. Every merged
  // buffer keeps day order, so the reveal is growing draw ranges sweeping
  // along the row in step (cells landed whole: prints, skirts, and backs
  // together); then today's sheet lands with its accent rim and a very
  // slight breathing pulse. Sheets never rebuild — geometry is static after
  // the reveal; only the draw ranges move. Instead of the plate's full
  // turntable — which would put a row this wide end-on — the camera sways
  // gently about its azimuth. Reduced motion renders the final state.
  const total = ord;
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
  const accretionMs = 3000;
  const t0 = performance.now();
  if (!still) {
    for (const m of meshes) {
      m.geometry.setDrawRange(0, 0);
      m.visible = false;
    }
    if (todayGroup) todayGroup.visible = false;
  }

  rig.start((now) => {
    if (still) return;
    // One-sided ~8° sway, phase-biased so the camera only swings toward
    // the spread side of the rest pose and back: swinging past it toward
    // the procession axis would pile the line up behind the massif and
    // tuck today's rim behind the mid-line stacks.
    rig.controls.autoRotateSpeed = -0.10 * Math.sin((now - t0) / 6500);
    const t = now - t0;
    const k = Math.min(1, t / accretionMs);
    const shown = Math.floor(k * total);
    for (const m of meshes) {
      const end = drawnEnd(m.userData.marks, shown);
      m.geometry.setDrawRange(0, end);
      m.visible = end > 0;
    }
    if (todayGroup) {
      todayGroup.visible = k >= 1;
      const s = 1 + 0.02 * Math.sin((now - t0) / 700); // subtle instrument pulse
      todayGroup.scale.setScalar(s);
    }
  });

  let tris = 0;
  for (const m of meshes) tris += m.geometry.attributes.position.count / 3;
  if (todayGroup) {
    for (const m of todayGroup.children) {
      if (m.isMesh) tris += m.geometry.attributes.position.count / 3;
    }
  }
  console.log('[structure] three r' + THREE.REVISION + ' · ' + slices.length +
    ' volumes · ' + all.length + ' cells · ' + Math.round(tris / 1000) +
    'k tris · ' + atlases.size + ' glyph atlases');
}

// atlasOrdered yields a Map's entries in ascending-key order — a stable
// biome→mesh order, so draw order never depends on first-touch order.
function atlasOrdered(map) {
  return [...map.entries()].sort((a, b) => a[0] - b[0]);
}
