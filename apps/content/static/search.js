// entries search — instant substring filtering, upgraded to on-device
// semantic search once the vendored ternlight engine (static/ternlight/,
// @ternlight/base) is instantiated. Everything runs locally: the model is
// ~7 MB (cached immutably), embeddings are ~5 ms each and cached in
// localStorage keyed by entry CID — content-addressed, so a cache entry can
// never go stale.
const input = document.getElementById("entry-search");
const status = document.getElementById("search-status");
const tbody = document.querySelector("table.rows tbody");

if (input && tbody) {
  const originalRows = [...tbody.querySelectorAll("tr[data-slug]")];
  const rowBySlug = new Map(originalRows.map((tr) => [tr.dataset.slug, tr]));

  let tern = null;   // loaded engine (embed fn)
  let vecs = null;   // [{slug, v, hay}] — embedding + lowercase haystack
  let loadState = "idle";
  let timer = null;

  input.addEventListener("focus", ensureReady, { once: true });
  input.addEventListener("input", () => {
    substringFilter(); // instant, always
    clearTimeout(timer);
    timer = setTimeout(semanticRank, 120);
  });

  function say(msg) { if (status) status.textContent = msg; }

  function substringFilter() {
    const q = input.value.trim().toLowerCase();
    for (const tr of originalRows) {
      tr.hidden = !!q && !tr.textContent.toLowerCase().includes(q);
    }
    if (!q) restoreOrder();
  }

  function restoreOrder() {
    for (const tr of originalRows) tbody.appendChild(tr);
  }

  async function ensureReady() {
    if (loadState !== "idle") return;
    loadState = "loading";
    say("loading semantic model…");
    try {
      const [bg, data] = await Promise.all([loadTern(), loadDocs()]);
      tern = bg;
      vecs = data.entries.map((d) => ({
        slug: d.slug,
        hay: [d.title, d.excerpt, (d.tags || []).join(" ")].join(" ").toLowerCase(),
        v: cachedVec(d),
      }));
      loadState = "ready";
      say("semantic search on");
      if (input.value.trim()) semanticRank();
    } catch (err) {
      loadState = "failed";
      say("substring search only");
    }
  }

  async function loadTern() {
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

  function loadDocs() {
    return fetch("/search-data").then((r) => {
      if (!r.ok) throw new Error("search-data failed");
      return r.json();
    });
  }

  function embedText(d) {
    return [d.title, d.excerpt, (d.tags || []).join(" "), d.snippet]
      .filter(Boolean).join(". ");
  }

  function cachedVec(d) {
    const key = "ffemb1:" + d.cid;
    try {
      const hit = localStorage.getItem(key);
      if (hit) {
        const bin = atob(hit);
        const bytes = new Uint8Array(bin.length);
        for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
        return new Float32Array(bytes.buffer);
      }
    } catch { /* storage unavailable — recompute */ }
    const v = tern.embed(embedText(d));
    try {
      let bin = "";
      const bytes = new Uint8Array(v.buffer);
      for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
      localStorage.setItem(key, btoa(bin));
    } catch { /* full or unavailable — fine */ }
    return v;
  }

  function dot(a, b) {
    let s = 0;
    for (let i = 0; i < a.length; i++) s += a[i] * b[i];
    return s;
  }

  function semanticRank() {
    const q = input.value.trim();
    if (!q || loadState !== "ready") return;
    const qv = tern.embed(q);
    const words = q.toLowerCase().split(/\s+/).filter((w) => w.length >= 3);
    const scored = vecs
      .map(({ slug, hay, v }) => ({
        slug,
        substr: words.some((w) => hay.includes(w)),
        sim: dot(qv, v),
      }))
      // A word-level hit always shows; otherwise require semantic signal
      // so unrelated entries drop out.
      .filter((s) => s.substr || s.sim > 0.3)
      .sort((a, b) => (b.substr - a.substr) || (b.sim - a.sim));

    for (const tr of originalRows) tr.hidden = true;
    for (const s of scored) {
      const tr = rowBySlug.get(s.slug);
      if (!tr) continue;
      tr.hidden = false;
      tbody.appendChild(tr); // re-append = ranked order
    }
    say(scored.length + " match" + (scored.length === 1 ? "" : "es") + " · semantic");
  }
}
