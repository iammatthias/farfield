// Instantiates the vendored ternlight engine (static/ternlight/) without a
// bundler: fetch the wasm, hand the wasm-bindgen glue its module, start it.
// Returns the glue module — embed(text) → Float32Array(384), L2-normalized.
export async function loadTern() {
  const V = "?v=ternlight-0.1.0"; // bump when the vendored engine changes
  const bg = await import("/static/ternlight/tern_engine_bg.js" + V);
  const resp = await fetch("/static/ternlight/tern_engine_bg.wasm" + V);
  let result;
  try {
    result = await WebAssembly.instantiateStreaming(resp, {
      "./tern_engine_bg.js": bg,
    });
  } catch {
    // Streaming needs the application/wasm MIME — fall back to a buffer.
    const buf = await (await fetch("/static/ternlight/tern_engine_bg.wasm" + V)).arrayBuffer();
    result = await WebAssembly.instantiate(buf, { "./tern_engine_bg.js": bg });
  }
  bg.__wbg_set_wasm(result.instance.exports);
  result.instance.exports.__wbindgen_start();
  return bg;
}

// Base64 round-trip for caching Float32Array embeddings in localStorage.
export function vecToB64(v) {
  let bin = "";
  const bytes = new Uint8Array(v.buffer);
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}
export function vecFromB64(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new Float32Array(bytes.buffer);
}
export function dot(a, b) {
  let s = 0;
  for (let i = 0; i < a.length; i++) s += a[i] * b[i];
  return s;
}
