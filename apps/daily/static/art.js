// art.js — progressive enhancement for /art: swaps the static plate SVG
// for a live terraced-terrain tile of the same day — the same heightfield
// the SVG quantizes into glyphs, rendered as stepped relief in the same
// biome inks, slowly orbiting. The SVG stays the canonical deterministic
// artifact (its bytes and CID never change) and the no-JS / no-WebGL
// fallback; if anything here fails, the SVG stays put.
//
// The geometry, lighting, and orbit all come from terrain.js — the same
// module the structure viewer draws with, so the plate and the structure
// read as one instrument at two zooms.

import * as THREE from 'three';
import { readTheme, reduceMotion, TileBuilder, terrainMaterial, tileContours, createRig } from 'terrain';

const plate = document.getElementById('plate-canvas');
if (plate && plate.dataset.api) {
  enhance(plate).catch((err) => {
    // Keep the SVG plate; just note why the canvas did not come up.
    console.warn('[art] keeping SVG plate:', err);
  });
}

async function enhance(plate) {
  const theme = readTheme();

  // Plate framing: the SVG's 500×310 viewBox proportions, one tile filling
  // the frame.
  const size = 24;
  const rig = createRig(plate, { theme, viewSize: size, aspect: 500 / 310, camDist: 1.95 });

  const res = await fetch(plate.dataset.api, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error('terrain API ' + res.status);
  const data = await res.json();

  const palette = (data.biome.colors || []).map((c) => new THREE.Color(c));
  const bands = data.bands || 6;

  // Terrace proportions — judged against the SVG plate: enough relief that
  // the bands read as strata, shallow enough to stay a survey plate, on a
  // thin plinth with a small apron (the framed-plate composition in 3-D).
  const relief = size * 0.24;
  const slabH = size * 0.045;
  const slabSize = size * 1.06;
  // Centered on the mass, lifted a touch — most terrain sits in the low
  // bands, so a pure geometric center leaves the frame top-heavy.
  const baseY = -(slabH + relief) / 2 + size * 0.04;

  const opts = {
    levels: data.levels,
    bands,
    palette,
    surface: theme.surface,
    fade: 1,
    x: 0,
    z: 0,
    y: baseY,
    size,
    relief,
    slabH,
    slabSize,
  };

  const build = (capH) => {
    const b = new TileBuilder();
    b.tile(capH === undefined ? opts : { ...opts, capH });
    return b.geometry();
  };

  const mesh = new THREE.Mesh(build(reduceMotion() ? undefined : 0), terrainMaterial());
  rig.scene.add(mesh);

  // Terrace contours — the plate's hairline language on the steps.
  const contours = tileContours({ ...opts, color: palette[palette.length - 1], opacity: 0.35 });
  if (contours) {
    contours.visible = reduceMotion();
    rig.scene.add(contours);
  }

  rig.mount();

  // Settle: terraces rise quickly in elevation order — the lowest bands
  // land first, the peaks last — then the contours ink in. Reduced motion
  // renders the final state immediately.
  const settleMs = 900;
  const t0 = performance.now();
  let settling = !reduceMotion();

  rig.start((now) => {
    if (!settling) return;
    const k = Math.min(1, (now - t0) / settleMs);
    const ease = 1 - Math.pow(1 - k, 3);
    mesh.geometry.dispose();
    mesh.geometry = build(k >= 1 ? undefined : ease * relief);
    if (k >= 1) {
      settling = false;
      if (contours) contours.visible = true;
    }
  });

  console.log('[art] three r' + THREE.REVISION + ' · ' + data.date +
    ' · ' + data.biome.name + ' · live terraces');
}
