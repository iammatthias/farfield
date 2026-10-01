// farfield ⌘K — one menu over the whole fleet.
//
// ⌘K (Ctrl K) anywhere opens it; on a phone, the ⌘K button in the top bar.
// It gathers every app's GET /palette — actions, pages and records — with the
// fleet session the browser already holds, ranks them as you type, and opens
// the one you pick. When typing is not enough ("the post where I wrote about
// diffusion", "make a QR code for the docs"), the last row asks a model, by
// way of content's POST /palette/ask, to pick from the same list.
//
// A page can add its own actions (the editor adds Save, Publish, Markdown):
//   (window.FarfieldPaletteQueue = window.FarfieldPaletteQueue || [])
//     .push(function () { return [{ title, sub, words, run() {} }]; });
(function () {
  "use strict";
  if (window.FarfieldPalette) return;

  var CACHE = "ff-palette-v1", TTL = 3 * 60 * 1000;
  var providers = [];
  var state = { apps: {}, fleet: [], here: "", loading: 0 };
  var ui = null, open = false, results = [], at = 0, asked = null, picked = null, fetchedAt = 0;

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }

  // ── data ──────────────────────────────────────────────────────────────
  function absolute(base, url) {
    try { return new URL(url, base + "/").href; } catch (e) { return url; }
  }
  function take(payload) {
    var app = payload.app, base = payload.base || location.origin;
    state.apps[app] = {
      app: app, base: base, signedIn: payload.signedIn,
      items: (payload.items || []).map(function (it) {
        return { app: app, kind: it.kind, title: it.title, sub: it.sub || "", words: it.words || "",
          url: absolute(base, it.url) };
      }),
    };
  }
  function saveCache() {
    try { sessionStorage.setItem(CACHE, JSON.stringify({ t: Date.now(), apps: state.apps, fleet: state.fleet, here: state.here })); } catch (e) {}
  }
  function loadCache() {
    try {
      var c = JSON.parse(sessionStorage.getItem(CACHE) || "null");
      if (c && Date.now() - c.t < TTL) { state.apps = c.apps; state.fleet = c.fleet; state.here = c.here; return true; }
    } catch (e) {}
    return false;
  }
  // a down app must not hold the list open: give each one a few seconds
  function get(url) {
    var ctl = window.AbortController ? new AbortController() : null;
    if (ctl) setTimeout(function () { ctl.abort(); }, 6000);
    return fetch(url, { credentials: "include", headers: { Accept: "application/json" }, signal: ctl ? ctl.signal : undefined })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); });
  }
  function refresh() {
    if (state.loading > 0) return;
    fetchedAt = Date.now();
    state.loading++;
    get("/palette").then(function (p) {
      state.here = p.app;
      state.fleet = p.fleet || [];
      take(p);
      render();
      var pending = state.fleet.filter(function (f) { return f.name !== p.app; });
      state.loading += pending.length;
      pending.forEach(function (f) {
        get(f.url.replace(/\/$/, "") + "/palette").then(function (q) { take(q); })
          .catch(function () { /* an app that is down or out of reach just isn't listed */ })
          .then(function () { state.loading--; saveCache(); render(); });
      });
    }).catch(function () {}).then(function () { state.loading--; render(); });
  }

  // ── ranking ───────────────────────────────────────────────────────────
  // A small fuzzy scorer: whole-word and prefix hits count most, then a
  // contiguous substring, then the letters in order. Titles outweigh the rest.
  function scoreIn(q, s) {
    if (!s) return 0;
    s = s.toLowerCase();
    var i = s.indexOf(q);
    if (i === 0) return 100;
    if (i > 0) return (/[\s\-_/.:]/.test(s[i - 1]) ? 80 : 50) - Math.min(i, 20) * 0.5;
    var j = 0, gap = 0, last = -1;
    for (var k = 0; k < s.length && j < q.length; k++) {
      if (s[k] === q[j]) { if (last >= 0) gap += k - last - 1; last = k; j++; }
    }
    // letters in order count only when they stay close: "nwpst" finds
    // "new post", but a short word scattered across a long title is noise
    return j === q.length && q.length >= 3 && gap <= q.length * 2 ? Math.max(1, 30 - gap) : 0;
  }
  // words a sentence carries that name nothing
  var STOP = { a: 1, an: 1, the: 1, for: 1, to: 1, of: 1, and: 1, in: 1, on: 1, my: 1, me: 1, i: 1,
    is: 1, it: 1, with: 1, about: 1, from: 1, that: 1, this: 1, where: 1, what: 1, show: 1, find: 1, open: 1 };
  function score(q, it) {
    var all = q.toLowerCase().split(/\s+/).filter(Boolean);
    var terms = all.filter(function (t) { return !STOP[t]; });
    if (!terms.length) terms = all;
    // a name is matched whole; a sentence ("new code for the docs") needs
    // most of its words to land, and is the Ask row's to answer in full
    var need = terms.length <= 2 ? terms.length : Math.ceil(terms.length * 0.6);
    var total = 0, hit = 0;
    for (var n = 0; n < terms.length; n++) {
      var t = terms[n];
      var best = Math.max(scoreIn(t, it.title) * 3, scoreIn(t, it.app) * 1.5,
        scoreIn(t, it.sub), scoreIn(t, it.words));
      if (best) { hit++; total += best; }
    }
    if (hit < need) return 0;
    total *= hit / terms.length;
    if (it.app === state.here) total *= 1.15;
    if (it.kind === "action") total *= 1.1;
    return total;
  }
  function everything() {
    var all = [];
    providers.forEach(function (fn) {
      try {
        (fn() || []).forEach(function (a) {
          all.push({ app: state.here || "here", kind: "action", title: a.title, sub: a.sub || "", words: a.words || "", run: a.run });
        });
      } catch (e) {}
    });
    Object.keys(state.apps).forEach(function (k) { all = all.concat(state.apps[k].items); });
    // every app is a destination even before its own list (and its own
    // home row) arrives — or when it is out of reach
    state.fleet.forEach(function (f) {
      if (state.apps[f.name]) return;
      all.push({ app: f.name, kind: "page", title: f.name, sub: "home", url: f.url, words: "go to " + f.name });
    });
    return all;
  }
  function rank(q) {
    var all = everything();
    if (!q.trim()) {
      // nothing typed: what you can do here, then where you can go
      var here = all.filter(function (it) { return it.app === state.here || it.run; });
      var apps = all.filter(function (it) { return it.sub === "home" && it.app !== state.here; });
      return here.slice(0, 12).concat(apps);
    }
    var seen = {}, out = [];
    all.forEach(function (it) {
      var s = score(q, it);
      var key = it.app + "|" + it.title + "|" + it.url;
      if (s > 0 && !seen[key]) { seen[key] = true; out.push({ it: it, s: s }); }
    });
    out.sort(function (a, b) { return b.s - a.s; });
    return out.slice(0, 40).map(function (x) { return x.it; });
  }

  // ── ui ────────────────────────────────────────────────────────────────
  function build() {
    ui = {};
    ui.root = el("div", "ff-palette");
    ui.root.setAttribute("role", "dialog");
    ui.root.setAttribute("aria-label", "Command menu");
    ui.root.hidden = true;
    ui.panel = el("div", "ff-palette-panel");
    ui.input = el("input", "ff-palette-input");
    ui.input.type = "text";
    ui.input.setAttribute("placeholder", "Go to, find or do anything…");
    ui.input.setAttribute("aria-label", "Command");
    ui.input.setAttribute("autocomplete", "off");
    ui.input.setAttribute("spellcheck", "false");
    ui.list = el("div", "ff-palette-list");
    ui.list.setAttribute("role", "listbox");
    ui.foot = el("div", "ff-palette-foot");
    ui.panel.appendChild(ui.input);
    ui.panel.appendChild(ui.list);
    ui.panel.appendChild(ui.foot);
    ui.root.appendChild(ui.panel);
    document.body.appendChild(ui.root);
    ui.root.addEventListener("mousedown", function (e) { if (e.target === ui.root) close(); });
    ui.input.addEventListener("input", function () { asked = null; picked = null; at = 0; render(); });
    ui.input.addEventListener("keydown", function (e) {
      if (e.key === "ArrowDown") { e.preventDefault(); move(1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); move(-1); }
      else if (e.key === "Enter") { e.preventDefault(); choose(at, e.metaKey || e.ctrlKey); }
      else if (e.key === "Escape") { e.preventDefault(); close(); }
    });
    ui.list.addEventListener("click", function (e) {
      var row = e.target.closest("[data-i]");
      if (row) choose(+row.getAttribute("data-i"), e.metaKey || e.ctrlKey);
    });
  }
  function move(d) {
    if (!results.length) return;
    at = (at + d + results.length) % results.length;
    paintSelection();
  }
  function paintSelection() {
    Array.prototype.forEach.call(ui.list.children, function (r, i) {
      r.setAttribute("aria-selected", i === at ? "true" : "false");
      if (i === at) r.scrollIntoView({ block: "nearest" });
    });
  }
  function label(it) {
    return it.kind === "action" ? "do" : it.kind === "record" ? "open" : "go";
  }
  function render() {
    if (!ui || !open) return;
    var q = ui.input.value;
    results = rank(q);
    if (q.trim().split(/\s+/).length >= 2 || (q.trim() && !results.length)) {
      results.push({ kind: "ask", title: "Ask farfield", sub: "“" + q.trim() + "”", app: "" });
    }
    // the words inside things — entries, posts, bookmarks — are content's
    // fleet search, which reads them; the menu only knows names
    var content = state.fleet.filter(function (f) { return f.name === "content"; })[0];
    if (q.trim() && content) {
      results.push({ kind: "page", app: "content", title: "Search everything", sub: "“" + q.trim() + "”",
        url: content.url.replace(/\/$/, "") + "/search?q=" + encodeURIComponent(q.trim()) });
    }
    if (picked) results.unshift(picked);
    if (at >= results.length) at = 0;
    ui.list.textContent = "";
    results.forEach(function (it, i) {
      var row = el("div", "ff-palette-row" + (it.kind === "ask" ? " ask" : "") + (it === picked ? " picked" : ""));
      row.setAttribute("role", "option");
      row.setAttribute("data-i", String(i));
      var main = el("span", "ff-palette-main");
      main.appendChild(el("span", "ff-palette-title", it.title));
      var sub = it === picked ? "picked for “" + q.trim() + "”" : it.sub;
      if (sub) main.appendChild(el("span", "ff-palette-sub", sub));
      row.appendChild(main);
      row.appendChild(el("span", "ff-palette-app", it.kind === "ask" ? "↵ ask" : (it.app || "") + " · " + label(it)));
      ui.list.appendChild(row);
    });
    if (asked) {
      var note = el("div", "ff-palette-answer", asked);
      ui.list.insertBefore(note, ui.list.firstChild);
    }
    if (!results.length && !asked) ui.list.appendChild(el("div", "ff-palette-empty", "Nothing by that name."));
    var n = Object.keys(state.apps).length;
    ui.foot.textContent = (state.loading > 0 ? "reaching the fleet… " : "") + n + " apps · ↑↓ to move · ↵ to open · esc to close";
    paintSelection();
  }
  function go(url, newTab) {
    close();
    if (newTab) window.open(url, "_blank", "noopener");
    else location.href = url;
  }
  function choose(i, newTab) {
    var it = results[i];
    if (!it) return;
    if (it.kind === "ask") return ask(ui.input.value.trim());
    if (it.run) { close(); it.run(); return; }
    if (it.url) go(it.url, newTab);
  }

  // ── ask: a sentence becomes one of the items ───────────────────────────
  function ask(q) {
    var content = state.fleet.filter(function (f) { return f.name === "content"; })[0];
    if (!content || !q) return;
    // the best fuzzy matches first, then the rest, capped — the model reads a list
    var seen = {}, cand = [];
    rank(q).concat(everything()).forEach(function (it) {
      var key = it.app + "|" + it.title + "|" + (it.url || "");
      if ((it.url || it.run) && !seen[key] && cand.length < 400) { seen[key] = true; cand.push(it); }
    });
    picked = null;
    asked = "Thinking…";
    render();
    fetch(content.url.replace(/\/$/, "") + "/palette/ask", {
      method: "POST", credentials: "include",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify({
        q: q, here: state.here,
        items: cand.map(function (it, i) { return { i: i, t: it.title, a: it.app, k: it.kind, s: it.sub }; }),
      }),
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (d) { return { ok: r.ok, d: d }; });
    }).then(function (res) {
      if (!res.ok) { asked = res.d.error || "Ask is unavailable right now."; render(); return; }
      var d = res.d;
      if (typeof d.pick === "number" && cand[d.pick]) {
        // the pick becomes the top row, selected, so Enter opens it
        asked = null;
        picked = cand[d.pick];
        at = 0;
        render();
        return;
      }
      asked = d.answer || "Nothing in the fleet fits that.";
      render();
    }).catch(function () { asked = "Ask is unavailable right now."; render(); });
  }
  // ── open / close ──────────────────────────────────────────────────────
  var returnFocus = null;
  function show() {
    if (!ui) build();
    if (open) { ui.input.select(); return; }
    returnFocus = document.activeElement;
    open = true;
    ui.root.hidden = false;
    ui.input.value = "";
    asked = null;
    picked = null;
    at = 0;
    // the cached list paints at once; the fleet is asked again only when stale
    var cached = loadCache() && state.apps[state.here];
    render();
    if (!cached || Date.now() - fetchedAt > 30000) refresh();
    ui.input.focus();
  }
  function close() {
    if (!open) return;
    open = false;
    ui.root.hidden = true;
    if (returnFocus && returnFocus.focus) { try { returnFocus.focus({ preventScroll: true }); } catch (e) {} }
  }

  document.addEventListener("keydown", function (e) {
    if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey && e.key.toLowerCase() === "k") {
      e.preventDefault();
      if (open) close(); else show();
    }
  });
  // a button in the top bar, for touch screens without ⌘K
  function addButton() {
    var bar = document.querySelector(".bar");
    if (!bar || bar.querySelector(".ff-palette-open")) return;
    var touch = window.matchMedia && window.matchMedia("(pointer: coarse)").matches;
    var b = el("button", "ff-palette-open", touch ? "Go to…" : (/Mac|iP/.test(navigator.platform) ? "⌘K" : "Ctrl K"));
    b.type = "button";
    b.setAttribute("aria-label", "Open the command menu");
    b.addEventListener("click", show);
    var fleet = bar.querySelector(".fleet");
    bar.insertBefore(b, fleet || null);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", addButton);
  else addButton();

  window.FarfieldPalette = {
    open: show,
    close: close,
    register: function (fn) { providers.push(fn); },
  };
  (window.FarfieldPaletteQueue || []).forEach(function (fn) { providers.push(fn); });
  window.FarfieldPaletteQueue = { push: function (fn) { providers.push(fn); } };
})();
