// structure.js — progressive enhancement for /art/structure: swaps the
// server-rendered SVG plate for an interactive three.js scene of the same
// w-slice. The SVG remains the canonical deterministic artifact and the
// no-JS / no-WebGL fallback; if anything here fails, the SVG stays put.
//
// Aesthetic contract with the page: muted survey-ink palette on the warm
// farfield surface, hairline volume edges, flat lighting — an instrument,
// not an arcade. Motion is slow and pausable, and prefers-reduced-motion
// renders the final state with no auto-orbit and no accretion animation.

import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';

const plate = document.getElementById('structure-plate');
if (plate && plate.dataset.api) {
  enhance(plate).catch((err) => {
    // Keep the SVG fallback; just note why the canvas did not come up.
    console.warn('[structure] keeping SVG fallback:', err);
  });
}

async function enhance(plate) {
  const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;

  // Theme inks come from the page, so the scene always matches the surface.
  const css = getComputedStyle(document.documentElement);
  const surface = new THREE.Color(css.getPropertyValue('--surface').trim() || '#fafaf7');
  const ink = new THREE.Color(css.getPropertyValue('--ink').trim() || '#0a0a0a');
  const accent = new THREE.Color(css.getPropertyValue('--accent').trim() || '#d93a00');

  // WebGL first — no point fetching data if the context cannot exist.
  let renderer;
  try {
    renderer = new THREE.WebGLRenderer({ antialias: true });
  } catch (err) {
    throw new Error('WebGL unavailable: ' + err.message);
  }
  if (!renderer.getContext()) throw new Error('WebGL context is null');

  const res = await fetch(plate.dataset.api, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error('structure API ' + res.status);
  const data = await res.json();
  const side = data.side || 16;
  const half = side / 2;

  // ── scene ────────────────────────────────────────────────────────────────
  const scene = new THREE.Scene();
  scene.background = surface;

  const camera = new THREE.PerspectiveCamera(35, 400 / 244, 1, 500);
  camera.position.set(side * 1.45, side * 1.05, side * 1.45);
  camera.lookAt(0, 0, 0);

  // Flat survey light: mostly ambient, one soft key so faces read apart —
  // kept near unit total so the instance tints stay true to the biome inks.
  scene.add(new THREE.AmbientLight(0xffffff, 0.85));
  const key = new THREE.DirectionalLight(0xffffff, 0.5);
  key.position.set(side, side * 1.6, side * 0.5);
  scene.add(key);

  // Hairline bounding box of the full 16³ volume.
  const boxEdges = new THREE.LineSegments(
    new THREE.EdgesGeometry(new THREE.BoxGeometry(side, side, side)),
    new THREE.LineBasicMaterial({ color: ink, transparent: true, opacity: 0.3 }),
  );
  scene.add(boxEdges);

  // Faint floor grid under the volume, restyled to the page's hairlines.
  const grid = new THREE.GridHelper(side, side, ink, ink);
  grid.position.y = -half;
  grid.material.transparent = true;
  grid.material.opacity = 0.12;
  scene.add(grid);

  // ── cells ────────────────────────────────────────────────────────────────
  // Lattice (x, y, z) with z up maps to scene (x, z, y) with Y up, centered.
  const at = (c) => [c.x - half + 0.5, c.z - half + 0.5, c.y - half + 0.5];

  // Age fade matches the SVG: newest = full ink, oldest fades toward the
  // surface tone across the band range (1 → 0.4, like the glyph opacities).
  const bands = data.bands || 5;
  const fade = (band) => 1 - (band / Math.max(bands - 1, 1)) * 0.6;

  const cells = data.cells || [];
  const past = cells.filter((c) => !c.today);
  const todayCell = cells.find((c) => c.today);

  const unit = new THREE.BoxGeometry(0.92, 0.92, 0.92);
  const inst = new THREE.InstancedMesh(
    unit,
    new THREE.MeshLambertMaterial({ color: 0xffffff }),
    Math.max(past.length, 1),
  );
  const m = new THREE.Matrix4();
  const tint = new THREE.Color();
  past.forEach((c, i) => {
    m.setPosition(...at(c));
    inst.setMatrixAt(i, m);
    const biome = data.biomes[c.biome] || { colors: ['#888', '#666', '#444'] };
    tint.copy(surface).lerp(new THREE.Color(biome.colors[2]), fade(c.age));
    inst.setColorAt(i, tint);
  });
  inst.count = past.length;
  inst.visible = past.length > 0;
  scene.add(inst);

  // Today's cell — the accent instrument light of the scene, gently pulsing.
  let todayMesh = null;
  if (todayCell) {
    todayMesh = new THREE.Mesh(
      unit,
      new THREE.MeshLambertMaterial({
        color: accent, emissive: accent, emissiveIntensity: 0.45,
      }),
    );
    todayMesh.position.set(...at(todayCell));
    todayMesh.userData.cell = todayCell;
    scene.add(todayMesh);
  }

  // ── swap the SVG for the canvas ──────────────────────────────────────────
  renderer.setPixelRatio(Math.min(devicePixelRatio || 1, 2));
  plate.classList.add('live');
  const svg = plate.querySelector('svg');
  if (svg) svg.remove();
  plate.appendChild(renderer.domElement);

  const tip = document.createElement('div');
  tip.className = 'structure-tip';
  tip.hidden = true;
  plate.appendChild(tip);

  const resize = () => {
    const w = plate.clientWidth || 400;
    const h = plate.clientHeight || Math.round((w * 244) / 400);
    renderer.setSize(w, h, false);
    camera.aspect = w / h;
    camera.updateProjectionMatrix();
  };
  new ResizeObserver(resize).observe(plate);
  resize();

  // ── controls + slow auto-orbit ───────────────────────────────────────────
  const controls = new OrbitControls(camera, renderer.domElement);
  controls.enableDamping = true;
  controls.dampingFactor = 0.08;
  controls.enablePan = false;
  controls.minDistance = side * 1.1;
  controls.maxDistance = side * 4;
  controls.autoRotate = !reduceMotion;
  controls.autoRotateSpeed = 0.5; // ≈3°/s — a survey turntable, not a spinner

  let lastInteract = -Infinity;
  const noteInteraction = () => { lastInteract = performance.now(); };
  renderer.domElement.addEventListener('pointerdown', noteInteraction);
  renderer.domElement.addEventListener('wheel', noteInteraction, { passive: true });

  // ── hover tooltip ────────────────────────────────────────────────────────
  const raycaster = new THREE.Raycaster();
  const pointer = new THREE.Vector2();
  const epoch = Date.UTC(...(data.epoch || '2020-01-01').split('-').map(Number).map((v, i) => (i === 1 ? v - 1 : v)));
  const dateOf = (i) => new Date(epoch + i * 86400000).toISOString().slice(0, 10);
  const describe = (c) => {
    const biome = data.biomes[c.biome];
    return 'DAY ' + c.i + ' · ' + dateOf(c.i) + ' · ' + (biome ? biome.name.toUpperCase() : '');
  };

  renderer.domElement.addEventListener('pointermove', (ev) => {
    const r = renderer.domElement.getBoundingClientRect();
    pointer.set(((ev.clientX - r.left) / r.width) * 2 - 1, -((ev.clientY - r.top) / r.height) * 2 + 1);
    raycaster.setFromCamera(pointer, camera);
    const targets = todayMesh ? [inst, todayMesh] : [inst];
    const hit = raycaster.intersectObjects(targets, false)[0];
    let cell = null;
    if (hit) cell = hit.object === todayMesh ? todayCell : past[hit.instanceId];
    if (cell) {
      tip.textContent = describe(cell);
      tip.style.left = ev.clientX - r.left + 12 + 'px';
      tip.style.top = ev.clientY - r.top + 12 + 'px';
      tip.hidden = false;
    } else {
      tip.hidden = true;
    }
  });
  renderer.domElement.addEventListener('pointerleave', () => { tip.hidden = true; });

  // ── animation ────────────────────────────────────────────────────────────
  // Accretion: cells arrive in day order over ~2.5s (the API pre-sorts them),
  // then today's cell lands and starts its slow pulse. Reduced motion skips
  // straight to the final state.
  const accretionMs = 2500;
  const t0 = performance.now();
  if (!reduceMotion) {
    inst.count = 0;
    if (todayMesh) todayMesh.visible = false;
  }

  renderer.setAnimationLoop((now) => {
    if (!reduceMotion) {
      const t = now - t0;
      const k = Math.min(1, t / accretionMs);
      inst.count = Math.floor(k * past.length);
      inst.visible = inst.count > 0;
      if (todayMesh) {
        todayMesh.visible = k >= 1;
        const s = 1 + 0.05 * Math.sin((now - t0) / 700); // subtle instrument pulse
        todayMesh.scale.setScalar(s);
      }
      // Auto-orbit pauses while the visitor is (recently) driving the view.
      controls.autoRotate = now - lastInteract > 5000;
    }
    controls.update();
    renderer.render(scene, camera);
  });

  console.log('[structure] three r' + THREE.REVISION + ' · w=' + data.w +
    ' · ' + cells.length + ' cells live');
}
