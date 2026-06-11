# Vendored three.js

- `three.module.min.js` — three.js r170 (npm `three@0.170.0`), `build/three.module.min.js`
- `OrbitControls.js` — same release, `examples/jsm/controls/OrbitControls.js`

Source: https://cdn.jsdelivr.net/npm/three@0.170.0/ (upstream: https://github.com/mrdoob/three.js)

License: MIT — Copyright © 2010-2024 three.js authors.
Full text: https://github.com/mrdoob/three.js/blob/r170/LICENSE

Both files are served verbatim by the daily app (embedded via go:embed,
fingerprinted with the cid16 `?v=` pattern, cached immutable). The page's
importmap maps the bare specifier `three` to the vendored module so
OrbitControls' `import ... from 'three'` resolves locally — no CDN, no
build step.
