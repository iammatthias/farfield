# @ternlight/base 0.1.0 — vendored

On-device semantic sentence embeddings (384-dim, L2-normalized) via WASM.
MIT-licensed; see LICENSE. Source: https://www.npmjs.com/package/@ternlight/base
(wasm-bindgen bundler-target output; instantiated without a bundler by
static/search.js, which fetches the .wasm and calls __wbg_set_wasm).

To upgrade: `npm pack @ternlight/base`, copy pkg-bundler/tern_engine_bg.{js,wasm}
and LICENSE here, and bump the ?v= query in static/search.js.
