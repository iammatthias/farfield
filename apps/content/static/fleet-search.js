// fleet search — one box over the whole constellation. The corpus comes from
// /fleet-search-data (entries, series, feed posts, public bookmarks);
// ranking is hybrid word-match + on-device semantic similarity, all local.
import { loadTern, vecToB64, vecFromB64, dot } from "/static/ternlight-loader.js";

const input = document.getElementById("fleet-search");
const status = document.getElementById("fleet-status");
const out = document.getElementById("fleet-results");

if (input && out) {
  let tern = null;
  let docs = [];
  let vecs = [];
  let state = "idle";
  let timer = null;

  boot();
  input.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(run, 120);
  });

  function say(msg) { if (status) status.textContent = msg; }

  async function boot() {
    state = "loading";
    say("loading the fleet…");
    try {
      const [bg, data] = await Promise.all([
        loadTern(),
        fetch("/fleet-search-data").then((r) => {
          if (!r.ok) throw new Error("fleet-search-data " + r.status);
          return r.json();
        }),
      ]);
      tern = bg;
      docs = data.docs || [];
      vecs = docs.map((d) => ({ d, v: cachedVec(d), hay: hayOf(d) }));
      state = "ready";
      const warn = (data.warnings || []).length
        ? " · " + data.warnings.join(", ")
        : "";
      say(docs.length + " things searchable" + warn);
      if (input.value.trim()) run();
    } catch (err) {
      state = "failed";
      say("fleet search unavailable");
    }
  }

  function hayOf(d) {
    return [d.title, d.snippet, (d.tags || []).join(" "), d.meta]
      .filter(Boolean).join(" ").toLowerCase();
  }
  function embedText(d) {
    return [d.title, d.snippet, (d.tags || []).join(" ")]
      .filter(Boolean).join(". ");
  }
  function cachedVec(d) {
    const key = "ffemb1:" + d.key;
    if (d.key) {
      try {
        const hit = localStorage.getItem(key);
        if (hit) return vecFromB64(hit);
      } catch { /* recompute */ }
    }
    const v = tern.embed(embedText(d));
    if (d.key) {
      try { localStorage.setItem(key, vecToB64(v)); } catch { /* fine */ }
    }
    return v;
  }

  function run() {
    const q = input.value.trim();
    out.innerHTML = "";
    if (!q || state !== "ready") return;
    const qv = tern.embed(q);
    const words = q.toLowerCase().split(/\s+/).filter((w) => w.length >= 3);
    const scored = vecs
      .map(({ d, v, hay }) => ({
        d,
        substr: words.some((w) => hay.includes(w)),
        sim: dot(qv, v),
      }))
      .filter((s) => s.substr || s.sim > 0.3)
      .sort((a, b) => (b.substr - a.substr) || (b.sim - a.sim))
      .slice(0, 30);

    say(scored.length + " match" + (scored.length === 1 ? "" : "es"));
    for (const { d, sim } of scored) {
      const a = document.createElement("a");
      a.className = "fleet-hit";
      a.href = d.url;
      a.innerHTML =
        '<span class="badge">' + d.kind + "</span>" +
        '<span class="fleet-hit-main"><strong></strong><small class="muted"></small></span>' +
        '<span class="fleet-hit-sim mono muted">' + sim.toFixed(2) + "</span>";
      a.querySelector("strong").textContent = d.title || "(untitled)";
      a.querySelector("small").textContent =
        (d.meta ? d.meta + " — " : "") + (d.snippet || "").slice(0, 140);
      out.appendChild(a);
    }
  }
}
