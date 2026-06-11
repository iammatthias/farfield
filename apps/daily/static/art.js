// art.js — progressive enhancement for /art: swaps the static plate SVG
// for a live fabric sheet of the same day — the day's actual glyph field
// printed on cloth, the cloth flexing and folding into the day's terrain.
// The heightfield the SVG quantizes into glyphs displaces a high-segment
// plane smoothly (smoothstep-bilinear — drape, not stairs); the print is
// the same chars in the same biome inks. The SVG stays the canonical
// deterministic artifact (its bytes and CID never change) and the no-JS /
// no-WebGL fallback; if anything here fails, the SVG stays put.
//
// The print, the drape, the lighting, and the orbit all come from
// terrain.js — the same module the structure viewer draws with, so the
// plate is one sheet up close and the structure is the archive of all of
// them.

import * as THREE from 'three';
import {
  readTheme, reduceMotion, createRig,
  RAMPS, levelSampler, plateCanvas, fabricTexture,
} from 'terrain';

const plate = document.getElementById('plate-canvas');
if (plate && plate.dataset.api) {
  enhance(plate).catch((err) => {
    // Keep the SVG plate; just note why the canvas did not come up.
    console.warn('[art] keeping SVG plate:', err);
  });
}

async function enhance(plate) {
  const theme = readTheme();

  // Plate framing: the SVG's 500×310 viewBox proportions, one sheet filling
  // the frame.
  const size = 24;
  const rig = createRig(plate, { theme, viewSize: size, aspect: 500 / 310, camDist: 1.95 });

  const res = await fetch(plate.dataset.api, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error('terrain API ' + res.status);
  const data = await res.json();

  const palette = (data.biome.colors || []).map((c) => new THREE.Color(c));
  const bands = data.bands || 6;
  const ramp = RAMPS[data.biome.name] || RAMPS.basin;

  // Drape proportions — judged against the SVG plate: enough relief that
  // the folds read as terrain, shallow enough that the cloth stays a survey
  // sheet; centered a touch high since most terrain sits in the low bands.
  const relief = size * 0.24;
  const baseY = -relief / 2 + size * 0.02;

  // The print: the day's full 24×24 glyph field on a 1536² canvas.
  const map = fabricTexture(rig.renderer, plateCanvas({
    levels: data.levels, bands, ramp, palette, surface: theme.surface,
  }));

  // The cloth: a 96×96-segment plane, vertices displaced by the smoothly
  // interpolated heightfield. UVs map the whole print across the sheet
  // (texture rows run with +z, toward the viewer).
  const seg = 96;
  const geo = new THREE.PlaneGeometry(size, size, seg, seg);
  geo.rotateX(-Math.PI / 2);
  const posAttr = geo.attributes.position;
  const uvAttr = geo.attributes.uv;
  const sample = levelSampler(data.levels);
  const count = posAttr.count;
  const base = new Float32Array(count); // the day's drape, before any motion
  const us = new Float32Array(count);
  const vs = new Float32Array(count);
  for (let i = 0; i < count; i++) {
    const u = posAttr.getX(i) / size + 0.5;
    const v = posAttr.getZ(i) / size + 0.5;
    us[i] = u;
    vs[i] = v;
    base[i] = ((sample(u, v) + 1) / bands) * relief;
    uvAttr.setXY(i, u, 1 - v);
  }

  const still = reduceMotion();

  // Living motion: a very subtle traveling undulation under the drape —
  // the sheet never quite settles, like cloth in still air. Reduced motion
  // keeps a static drape.
  const amp = still ? 0 : relief * 0.04;
  const TAU = Math.PI * 2;

  // shape lays the sheet at settle progress k (0 = flat and lifted, 1 =
  // fully draped), with the undulation blended in by k.
  const shape = (k, now) => {
    const lift = (1 - k) * relief * 0.8;
    const t = now / 1000;
    for (let i = 0; i < count; i++) {
      let y = base[i] * k + lift;
      if (amp > 0) {
        y += amp * k * (
          0.6 * Math.sin(TAU * (1.1 * us[i] + 0.5 * vs[i]) + t * 0.7) +
          0.4 * Math.sin(TAU * (0.7 * vs[i] - 0.6 * us[i]) + t * 0.53)
        );
      }
      posAttr.setY(i, y);
    }
    posAttr.needsUpdate = true;
    geo.computeVertexNormals();
  };

  // The sheet: printed face up, the underside the same print in darkened
  // ink (a second draw of the same geometry, back faces only), so folds
  // and the drape's rim read as cloth with a back.
  const front = new THREE.MeshLambertMaterial({ map });
  const back = new THREE.MeshLambertMaterial({
    map, side: THREE.BackSide, color: new THREE.Color(0.42, 0.42, 0.4),
  });
  const sheet = new THREE.Group();
  sheet.add(new THREE.Mesh(geo, front), new THREE.Mesh(geo, back));
  sheet.position.y = baseY;
  rig.scene.add(sheet);

  shape(still ? 1 : 0, 0);
  rig.mount();

  // Settle: the sheet floats down and drapes into the day's terrain over
  // ~1s, then keeps its quiet undulation. Reduced motion renders the final
  // drape immediately and never mutates it again.
  const settleMs = 1000;
  const t0 = performance.now();

  rig.start((now) => {
    if (still) return;
    const k = Math.min(1, (now - t0) / settleMs);
    shape(1 - Math.pow(1 - k, 3), now);
  });

  console.log('[art] three r' + THREE.REVISION + ' · ' + data.date +
    ' · ' + data.biome.name + ' · glyph fabric · ' +
    seg * seg * 2 + ' tris ×2 sides · ' + map.image.width + 'px print');
}
