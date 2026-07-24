/* Farfield Sync — bidirectional vault ↔ content-service sync, in-app.
 *
 * This is the same three-way merge as `content sync-vault` (apps/content/
 * sync.go) and shares its state file (content/.farfield-sync.json), so the
 * CLI and the plugin are interchangeable: per-entry state records the remote
 * CID and a local content hash from the last sync. Local-only changes push,
 * remote-only changes pull, and both-changed conflicts resolve to whichever
 * side was edited more recently (a tie or unknowable timestamp writes a
 * `.remote.md` sibling instead — the fallback never guesses). Deletions
 * never propagate — a vanished side is reported.
 *
 * The hash MUST stay byte-compatible with entryHash in sync.go:
 * sha256(title \0 excerpt \0 tags-joined-by-comma \0 published \0 body-trimmed \0).
 *
 * Sync runs vault-wide (ribbon, command, auto-interval) or for a single
 * note (ribbon, command, file context menu) — both paths share syncEntry
 * and the state file, so they never disagree.
 *
 * Plain CommonJS, no build step, no Node APIs — works on mobile.
 */
"use strict";

const {
  Plugin, Notice, PluginSettingTab, Setting, requestUrl, stringifyYaml,
} = require("obsidian");

const DEFAULTS = {
  contentUrl: "https://content.farfield.systems",
  apiKey: "",
  contentRoot: "content",
  autoSyncMinutes: 0, // 0 = manual only
  blobsUrl: "https://blobs.farfield.systems",
  blobsKey: "", // blob uploads stay off until a key is set
};

const STATE_FILE = ".farfield-sync.json";
const IPFS_REF_RE = /ipfs:\/\/[A-Za-z0-9]{46,}/;

/* ── local media references (manual blob upload) ───────────────────────── */

const MEDIA_EXTS = new Set([
  "png", "jpg", "jpeg", "gif", "webp", "avif", "svg",
  "mp4", "mov", "webm", "m4v",
  "m4a", "mp3", "wav", "ogg", "aac", "flac",
  "pdf",
]);

function mediaExt(target) {
  return MEDIA_EXTS.has((target.split(".").pop() || "").toLowerCase());
}

// Finds media references in a body that are not yet blob:// refs, in
// document order:
//   kind "wiki"     — ![[file.png]] / ![[file.png|alt]] (vault-resolvable)
//   kind "path"     — ![alt](relative/path.png)          (vault-resolvable)
//   kind "file-url" — ![alt](file:///abs/path.png)       (outside the vault;
//                     reported, never uploadable from here)
// Web URLs, blob://, series://, and ipfs:// targets are not local media and
// are left alone.
function scanLocalRefs(body) {
  const out = [];
  let m;
  const wiki = /!\[\[([^\]|]+?)(?:\|([^\]]*))?\]\]/g;
  while ((m = wiki.exec(body))) {
    const target = m[1].trim();
    if (!mediaExt(target)) continue; // note transclusion, not media
    out.push({ match: m[0], target, alt: (m[2] || "").trim(), kind: "wiki" });
  }
  const md = /!\[([^\]]*)\]\(([^)]+)\)/g;
  while ((m = md.exec(body))) {
    let target = m[2].trim();
    if (/^file:\/\//i.test(target)) {
      if (mediaExt(target)) out.push({ match: m[0], target, alt: m[1].trim(), kind: "file-url" });
      continue;
    }
    if (/^[a-z][a-z0-9+.-]*:/i.test(target)) continue; // http(s), blob, series, ipfs…
    try { target = decodeURIComponent(target); } catch (e) { /* keep as-is */ }
    if (!mediaExt(target)) continue;
    out.push({ match: m[0], target, alt: m[1].trim(), kind: "path" });
  }
  return out;
}

/* ── canonical hashing (parity with sync.go entryHash) ─────────────────── */

async function entryHash(e) {
  const parts = [
    e.title || "",
    e.excerpt || "",
    (e.tags || []).join(","),
    e.published ? "true" : "false",
    (e.body || "").trim(),
  ];
  const enc = new TextEncoder();
  const chunks = [];
  for (const p of parts) {
    chunks.push(enc.encode(p), Uint8Array.of(0));
  }
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const buf = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) { buf.set(c, off); off += c.length; }
  const digest = await crypto.subtle.digest("SHA-256", buf);
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/* ── time & yaml helpers (parity with sync.go) ─────────────────────────── */

// Vault dates ("YYYY-MM-DD HH:mm") are read as UTC, matching the importer.
function toRFC3339(s) {
  s = (s || "").toString().trim();
  if (!s) return "";
  if (/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}(:\d{2})?$/.test(s)) {
    return new Date(s.replace(" ", "T") + (s.length === 16 ? ":00" : "") + "Z").toISOString().replace(/\.\d{3}Z$/, "Z");
  }
  if (/^\d{4}-\d{2}-\d{2}$/.test(s)) {
    return s + "T00:00:00Z";
  }
  const d = new Date(s);
  return isNaN(d) ? "" : d.toISOString().replace(/\.\d{3}Z$/, "Z");
}

function nowStamp() {
  return new Date().toISOString().replace(/\.\d{3}Z$/, "Z");
}

function vaultTime(rfc) {
  const d = new Date(rfc);
  if (isNaN(d)) return rfc || "";
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())} ${p(d.getUTCHours())}:${p(d.getUTCMinutes())}`;
}

function yamlScalar(v) {
  return stringifyYaml(v === undefined || v === null ? "" : v).trimEnd();
}

// Renders an entry in the vault's frontmatter shape (same field order as
// the CLI's writeVaultFile, so the two tools don't churn each other's files).
function renderEntryFile(e) {
  const lines = ["---"];
  lines.push("title: " + yamlScalar(e.title || ""));
  lines.push("slug: " + yamlScalar(e.slug));
  lines.push("published: " + (e.published ? "true" : "false"));
  lines.push("created: " + yamlScalar(vaultTime(e.createdAt)));
  lines.push("updated: " + yamlScalar(vaultTime(e.updatedAt)));
  const tags = e.tags || [];
  if (tags.length === 0) {
    lines.push("tags: []");
  } else {
    lines.push("tags:");
    for (const t of tags) lines.push("  - " + yamlScalar(t));
  }
  lines.push("excerpt: " + yamlScalar(e.excerpt || ""));
  lines.push("---", "");
  return lines.join("\n") + "\n" + (e.body || "").trim() + "\n";
}

/* ── displaying farfield-hosted media ──────────────────────────────────── */

// mediaKindFor buckets a MIME type into the element family that displays it
// (parity with lib/markdown renderBlob). An empty mime — the probe failed,
// likely offline — optimistically tries an image, the common case.
function mediaKindFor(mime) {
  if (/^image\//.test(mime || "")) return "image";
  if (/^video\//.test(mime || "")) return "video";
  if (/^audio\//.test(mime || "")) return "audio";
  return mime ? "file" : "image";
}

// blobRefsIn extracts every blob CID referenced in a markdown body — used
// to turn a series' gallery markdown into an image grid.
function blobRefsIn(text) {
  const out = [];
  const re = /blob:\/\/([A-Za-z0-9]+)/g;
  let m;
  while ((m = re.exec(text || ""))) out.push(m[1]);
  return out;
}

/* ── conflict resolution: last write wins (parity with sync.go) ────────── */

// The more recently edited side wins a both-changed conflict. A tie or an
// unknowable timestamp stays a conflict — the sibling fallback never
// guesses. Losing versions are recoverable either way: remote losers live
// in the entry's revision history, local losers in the vault's git.
function newerSide(localMs, remoteUpdated) {
  const remoteMs = Date.parse(remoteUpdated || "");
  if (!localMs || isNaN(remoteMs)) return "conflict";
  if (localMs > remoteMs) return "push";
  if (remoteMs > localMs) return "pull";
  return "conflict";
}

/* ── merge table (parity with sync.go decide) ──────────────────────────── */

function decide(hasLocal, hasRemote, known, localChanged, remoteChanged) {
  if (hasLocal && hasRemote) {
    if (localChanged && remoteChanged) return "conflict";
    if (localChanged) return "push";
    if (remoteChanged) return "pull";
    return "none";
  }
  if (hasLocal && !hasRemote) return known ? "remote-gone" : "create";
  if (!hasLocal && hasRemote) return known ? "local-gone" : "pull-new";
  return "none";
}

/* ── the plugin ────────────────────────────────────────────────────────── */

class FarfieldSyncPlugin extends Plugin {
  async onload() {
    this.settings = Object.assign({}, DEFAULTS, await this.loadData());
    this.syncing = false;

    this.addRibbonIcon("refresh-cw", "Farfield: sync vault", () => this.sync());
    this.addRibbonIcon("file-check", "Farfield: sync current note", () => {
      const f = this.app.workspace.getActiveFile();
      if (!f) {
        new Notice("Farfield sync: no active note");
        return;
      }
      this.syncFile(f);
    });
    this.addRibbonIcon("image-up", "Farfield: upload current note's media", () => {
      const f = this.app.workspace.getActiveFile();
      if (!f) {
        new Notice("Farfield media: no active note");
        return;
      }
      this.uploadMedia(f);
    });

    this.addCommand({
      id: "sync",
      name: "Sync vault with Farfield",
      callback: () => this.sync(),
    });
    this.addCommand({
      id: "sync-note",
      name: "Sync this note with Farfield",
      checkCallback: (checking) => {
        const f = this.app.workspace.getActiveFile();
        const ok = !!(f && this.entryForFile(f));
        if (!checking && ok) this.syncFile(f);
        return ok;
      },
    });
    // Deliberately manual and never part of sync/auto-sync: an uploaded
    // blob only stops being an orphan once its entry pushes, so the author
    // decides when media goes up.
    this.addCommand({
      id: "upload-media",
      name: "Upload this note's media to Farfield",
      checkCallback: (checking) => {
        const f = this.app.workspace.getActiveFile();
        const ok = !!(f && this.entryForFile(f));
        if (!checking && ok) this.uploadMedia(f);
        return ok;
      },
    });

    // Right-click a note in the explorer (or its tab) to act on just it.
    this.registerEvent(this.app.workspace.on("file-menu", (menu, file) => {
      if (!this.entryForFile(file)) return;
      menu.addItem((item) => item
        .setTitle("Sync with Farfield")
        .setIcon("refresh-cw")
        .onClick(() => this.syncFile(file)));
      menu.addItem((item) => item
        .setTitle("Upload media to Farfield")
        .setIcon("image-up")
        .onClick(() => this.uploadMedia(file)));
    }));

    this.addSettingTab(new FarfieldSettingTab(this.app, this));
    this.statusEl = this.addStatusBarItem();
    this.applyAutoSync();

    // Render farfield-hosted media inside notes. The post-processor covers
    // reading view; the observer catches embeds Obsidian builds outside it
    // (Live Preview widgets, popovers). Hydration is idempotent, so overlap
    // is harmless.
    this.mimeCache = new Map();
    this.registerMarkdownPostProcessor((el) => this.hydrateMedia(el));
    const observer = new MutationObserver((muts) => {
      for (const m of muts) {
        for (const n of m.addedNodes) this.hydrateMedia(n);
      }
    });
    observer.observe(document.body, { childList: true, subtree: true });
    this.register(() => observer.disconnect());
  }

  /* ── displaying blob:// and series:// refs ── */

  blobPublicURL(cid) {
    return (this.settings.blobsUrl || "").replace(/\/$/, "") + "/blobs/" + cid;
  }

  // MIME for a cid, probed once per session via HEAD on the public bytes
  // route — no key needed, and the tag choice (img/video/audio) needs it.
  async blobMime(cid) {
    if (this.mimeCache.has(cid)) return this.mimeCache.get(cid);
    let mime = "";
    try {
      const res = await requestUrl({ url: this.blobPublicURL(cid), method: "HEAD", throw: false });
      if (res.status >= 200 && res.status < 300) {
        const h = res.headers || {};
        mime = h["content-type"] || h["Content-Type"] || "";
      }
    } catch (e) { /* offline — render optimistically */ }
    this.mimeCache.set(cid, mime);
    return mime;
  }

  // Rewrites farfield refs inside a rendered fragment: <img src="blob://…">
  // becomes the real media element served by blobs, a series embed becomes
  // an image grid, and [text](blob://…) links get a clickable public URL.
  hydrateMedia(root) {
    if (!root || root.nodeType !== 1) return;
    const embedSel = 'img[src^="blob://"], img[src^="series://"]';
    const embeds = Array.from(root.querySelectorAll(embedSel));
    if (root.matches && root.matches(embedSel)) embeds.push(root);
    for (const el of embeds) {
      if (el.dataset.ffHydrated) continue;
      el.dataset.ffHydrated = "1";
      const src = el.getAttribute("src") || "";
      if (src.startsWith("series://")) this.hydrateSeries(el, src.slice("series://".length));
      else this.hydrateBlob(el, src.slice("blob://".length));
    }
    const linkSel = 'a[href^="blob://"]';
    const links = Array.from(root.querySelectorAll(linkSel));
    if (root.matches && root.matches(linkSel)) links.push(root);
    for (const a of links) {
      a.setAttribute("href", this.blobPublicURL((a.getAttribute("href") || "").slice("blob://".length)));
    }
  }

  async hydrateBlob(el, cid) {
    const url = this.blobPublicURL(cid);
    const kind = mediaKindFor(await this.blobMime(cid));
    if (kind === "video" || kind === "audio") {
      const m = document.createElement(kind);
      m.controls = true;
      m.preload = "metadata";
      m.src = url;
      m.className = "farfield-blob-media";
      el.replaceWith(m);
    } else if (kind === "file") {
      const a = document.createElement("a");
      a.href = url;
      a.textContent = el.getAttribute("alt") || "blob://" + cid;
      el.replaceWith(a);
    } else {
      el.classList.add("farfield-blob-media");
      el.src = url;
    }
  }

  async hydrateSeries(el, slug) {
    const wrap = document.createElement("div");
    wrap.className = "farfield-series";
    wrap.textContent = "Loading series " + slug + "…";
    el.replaceWith(wrap);
    try {
      const se = await this.api("GET", "/api/series/" + encodeURIComponent(slug));
      const cids = blobRefsIn(se && se.body);
      wrap.textContent = "";
      if (!cids.length) {
        wrap.textContent = "series://" + slug + " (empty)";
        return;
      }
      for (const cid of cids) {
        const img = document.createElement("img");
        img.loading = "lazy";
        img.alt = "";
        img.src = this.blobPublicURL(cid);
        wrap.appendChild(img);
      }
    } catch (err) {
      wrap.textContent = "series://" + slug + " (could not load: " + err.message + ")";
    }
  }

  applyAutoSync() {
    if (this.autoTimer) {
      window.clearInterval(this.autoTimer);
      this.autoTimer = null;
    }
    const mins = Number(this.settings.autoSyncMinutes) || 0;
    if (mins > 0) {
      this.autoTimer = window.setInterval(() => this.sync(true), mins * 60 * 1000);
      this.registerInterval(this.autoTimer);
    }
  }

  status(msg) {
    if (this.statusEl) this.statusEl.setText(msg ? "farfield: " + msg : "");
  }

  /* ── vault I/O ── */

  statePath() {
    return this.settings.contentRoot + "/" + STATE_FILE;
  }

  async loadState() {
    try {
      const raw = await this.app.vault.adapter.read(this.statePath());
      const st = JSON.parse(raw);
      if (!st.entries) st.entries = {};
      return st;
    } catch (e) {
      return { version: 1, entries: {} };
    }
  }

  async saveState(st) {
    await this.app.vault.adapter.write(this.statePath(), JSON.stringify(st, null, 2) + "\n");
  }

  // Entries are <contentRoot>/<collection>/<slug>.md; deeper nesting, root
  // notes, and conflict siblings are not entries. Returns {slug, lf} or null.
  entryForFile(f) {
    if (!f || !f.path || !f.path.endsWith(".md")) return null;
    if (f.path.endsWith(".remote.md")) return null;
    const root = this.settings.contentRoot + "/";
    if (!f.path.startsWith(root)) return null;
    const rel = f.path.slice(root.length).split("/");
    if (rel.length !== 2) return null;
    const fm = this.app.metadataCache.getFileCache(f)?.frontmatter || {};
    const slug = String(fm.slug || f.basename);
    return { slug, lf: { file: f, collection: rel[0], fm } };
  }

  localEntries() {
    const out = new Map();
    for (const f of this.app.vault.getMarkdownFiles()) {
      const ent = this.entryForFile(f);
      if (ent) out.set(ent.slug, ent.lf);
    }
    return out;
  }

  async readEntry(lf, slug) {
    const raw = await this.app.vault.read(lf.file);
    const m = raw.match(/^---\n[\s\S]*?\n---\n?/);
    const body = (m ? raw.slice(m[0].length) : raw).trim();
    const fm = lf.fm;
    let tags = fm.tags ?? [];
    if (typeof tags === "string") tags = [tags];
    tags = tags.filter(Boolean).map(String);
    return {
      collection: lf.collection,
      slug,
      title: String(fm.title ?? slug),
      excerpt: String(fm.excerpt ?? "").trim(),
      body,
      tags,
      published: fm.published === true,
      createdAt: toRFC3339(fm.created),
      updatedAt: toRFC3339(fm.updated) || toRFC3339(fm.created),
    };
  }

  async writeEntryFile(path, e) {
    const content = renderEntryFile(e);
    const existing = this.app.vault.getAbstractFileByPath(path);
    if (existing) {
      await this.app.vault.modify(existing, content);
    } else {
      await this.app.vault.create(path, content);
    }
  }

  /* ── remote I/O ── */

  async api(method, path, body, allow404) {
    const res = await requestUrl({
      url: this.settings.contentUrl.replace(/\/$/, "") + path,
      method,
      headers: Object.assign(
        { "X-API-Key": this.settings.apiKey },
        body ? { "Content-Type": "application/json" } : {},
      ),
      body: body ? JSON.stringify(body) : undefined,
      throw: false,
    });
    if (allow404 && res.status === 404) return null;
    if (res.status < 200 || res.status >= 300) {
      throw new Error(method + " " + path + ": HTTP " + res.status);
    }
    return res.json;
  }

  async fetchRemote() {
    const data = await this.api("GET", "/api/entries?status=all");
    const map = new Map();
    for (const e of data.entries || []) map.set(e.slug, e);
    return map;
  }

  // One entry by slug; null when it does not exist remotely.
  fetchRemoteOne(slug) {
    return this.api("GET", "/api/entries/" + encodeURIComponent(slug), null, true);
  }

  pushPayload(e) {
    return {
      collection: e.collection, slug: e.slug, title: e.title,
      excerpt: e.excerpt, body: e.body, tags: e.tags,
      published: e.published, createdAt: e.createdAt,
    };
  }

  /* ── the sync ── */

  async sync(quiet) {
    if (this.syncing) return;
    if (!this.settings.apiKey) {
      new Notice("Farfield sync: set the API key in settings first");
      return;
    }
    this.syncing = true;
    this.status("syncing…");
    try {
      const summary = await this.runSync();
      this.status(summary.line);
      if (!quiet || summary.changed || summary.conflicts) {
        new Notice("Farfield sync — " + summary.line, 8000);
      }
      if (summary.notes.length) {
        console.log("[farfield-sync]\n" + summary.notes.join("\n"));
      }
    } catch (err) {
      this.status("failed");
      new Notice("Farfield sync failed: " + err.message, 10000);
      console.error("[farfield-sync]", err);
    } finally {
      this.syncing = false;
    }
  }

  // Sync exactly one note: same decide/apply logic, scoped to the file's
  // slug — the state file still records the result, so a later vault-wide
  // sync agrees with what happened here.
  async syncFile(file) {
    if (this.syncing) return;
    if (!this.settings.apiKey) {
      new Notice("Farfield sync: set the API key in settings first");
      return;
    }
    const ent = this.entryForFile(file);
    if (!ent) {
      new Notice(file && file.path && file.path.endsWith(".remote.md")
        ? "Farfield sync: this is a conflict copy — merge it into the main note, delete it, then sync that note"
        : "Farfield sync: not an entry (expects " + this.settings.contentRoot + "/<collection>/<note>.md)");
      return;
    }
    this.syncing = true;
    this.status("syncing " + file.basename + "…");
    try {
      const st = await this.loadState();
      const re = await this.fetchRemoteOne(ent.slug);
      const counts = { unchanged: 0, push: 0, create: 0, pull: 0, "pull-new": 0, conflict: 0 };
      const notes = [];
      const act = await this.syncEntry(ent.slug, ent.lf, re || undefined, st, counts, notes);
      await this.saveState(st);
      const verb = {
        "none": "up to date",
        "push": "pushed local edits",
        "create": "created on Farfield",
        "pull": "pulled remote edits",
        "pull-new": "pulled from Farfield",
        "conflict": "conflict — review the .remote.md copy",
        "skip-ipfs": "not pushed — body still has legacy ipfs:// refs",
        "remote-gone": "remote entry is gone (trashed?); local kept",
        "local-gone": "local file is gone; remote kept",
      }[act] || act;
      this.status(file.basename + ": " + verb);
      new Notice("Farfield sync — " + file.basename + ": " + verb, 6000);
      if (notes.length) console.log("[farfield-sync]\n" + notes.join("\n"));
    } catch (err) {
      this.status("failed");
      new Notice("Farfield sync failed: " + err.message, 10000);
      console.error("[farfield-sync]", err);
    } finally {
      this.syncing = false;
    }
  }

  /* ── manual media upload ── */

  // Uploads a note's vault-local media to the blobs service and rewrites
  // the references to blob://cid. Manual by design — it never runs from
  // sync or auto-sync, and it never pushes the entry: media without a
  // pushed entry is an orphan-in-waiting, so the author syncs when ready.
  // Blobs are content-addressed, so re-running is a no-op per file.
  async uploadMedia(file) {
    if (this.syncing) return;
    const ent = this.entryForFile(file);
    if (!ent) {
      new Notice("Farfield media: not an entry — uploads are per-entry");
      return;
    }
    if (!this.settings.blobsKey) {
      new Notice("Farfield media: set the blobs URL + API key in settings first");
      return;
    }
    this.syncing = true;
    this.status("uploading media…");
    try {
      const raw = await this.app.vault.read(file);
      const refs = scanLocalRefs(raw);
      const outside = refs.filter((r) => r.kind === "file-url");
      const uploadable = refs.filter((r) => r.kind !== "file-url");
      if (!refs.length) {
        this.status("");
        new Notice("Farfield media — " + file.basename + ": no local media references");
        return;
      }

      const cids = new Map(); // target -> cid
      const failed = [];
      for (const r of uploadable) {
        if (cids.has(r.target)) continue;
        const tf = this.app.metadataCache.getFirstLinkpathDest(r.target, file.path);
        if (!tf) {
          failed.push(r.target + " — not found in the vault");
          continue;
        }
        try {
          const bytes = await this.app.vault.readBinary(tf);
          const meta = await this.blobsUpload(bytes);
          cids.set(r.target, meta.cid);
        } catch (err) {
          failed.push(r.target + " — " + err.message);
        }
      }

      let rewrote = 0;
      let body = raw;
      for (const r of uploadable) {
        const cid = cids.get(r.target);
        if (!cid || !body.includes(r.match)) continue;
        rewrote += body.split(r.match).length - 1;
        body = body.split(r.match).join("![" + r.alt + "](blob://" + cid + ")");
      }
      if (body !== raw) await this.app.vault.modify(file, body);

      const bits = [cids.size + " uploaded", rewrote + " refs rewritten"];
      if (failed.length) bits.push(failed.length + " failed");
      if (outside.length) {
        bits.push(outside.length + " file:// refs skipped (outside the vault)");
      }
      const line = bits.join(" · ") +
        (cids.size ? " — sync this note when you're ready to publish" : "");
      this.status(file.basename + ": media " + (cids.size ? "uploaded" : "unchanged"));
      new Notice("Farfield media — " + file.basename + ": " + line, 10000);
      const details = failed.concat(outside.map((r) => r.target + " — file:// ref; move it into the vault or upload it another way"));
      if (details.length) console.log("[farfield-sync] media:\n" + details.join("\n"));
    } catch (err) {
      this.status("failed");
      new Notice("Farfield media failed: " + err.message, 10000);
      console.error("[farfield-sync]", err);
    } finally {
      this.syncing = false;
    }
  }

  // Raw-body upload to the blobs service; the server sniffs the MIME type
  // from the bytes and dedupes by CID.
  async blobsUpload(bytes) {
    const res = await requestUrl({
      url: (this.settings.blobsUrl || "").replace(/\/$/, "") + "/blobs",
      method: "POST",
      headers: {
        "X-API-Key": this.settings.blobsKey,
        "Content-Type": "application/octet-stream",
      },
      body: bytes,
      throw: false,
    });
    if (res.status < 200 || res.status >= 300) {
      throw new Error("HTTP " + res.status);
    }
    return res.json;
  }

  async runSync() {
    const [remote, st] = await Promise.all([this.fetchRemote(), this.loadState()]);
    const local = this.localEntries();

    const slugs = new Set([...local.keys(), ...remote.keys(), ...Object.keys(st.entries)]);
    const counts = { unchanged: 0, push: 0, create: 0, pull: 0, "pull-new": 0, conflict: 0 };
    const notes = [];

    for (const slug of [...slugs].sort()) {
      await this.syncEntry(slug, local.get(slug), remote.get(slug), st, counts, notes);
    }

    await this.saveState(st);
    const line = `${counts.unchanged} unchanged · ${counts.push} push · ${counts.create} create · ` +
      `${counts.pull} pull · ${counts["pull-new"]} pull-new · ${counts.conflict} conflicts`;
    const changed = counts.push + counts.create + counts.pull + counts["pull-new"];
    return { line, notes, changed, conflicts: counts.conflict };
  }

  // Syncs one slug: the full decide/apply pass for a (local, remote, state)
  // triple. Mutates counts, notes, and st; returns the action taken. Both
  // the vault-wide sync and the single-note sync run through here.
  async syncEntry(slug, lf, re, st, counts, notes) {
    const rec = st.entries[slug];

    const le = lf ? await this.readEntry(lf, slug) : null;
    const lHash = le ? await entryHash(le) : "";
    const localChanged = !!le && (!rec || lHash !== rec.localHash);
    const remoteChanged = !!re && (!rec || re.cid !== rec.remoteCid);

    // First encounter, equal content: seed quietly (see sync.go).
    if (le && re && !rec && lHash === (await entryHash(re))) {
      st.entries[slug] = { remoteCid: re.cid, localHash: lHash, syncedAt: nowStamp() };
      counts.unchanged++;
      return "none";
    }

    let act = decide(!!le, !!re, !!rec, localChanged, remoteChanged);
    if (act === "conflict") {
      // local edit time: the later of file mtime and frontmatter updated
      const fmMs = Date.parse(toRFC3339(lf.fm.updated) || "");
      const localMs = Math.max(lf.file.stat?.mtime || 0, isNaN(fmMs) ? 0 : fmMs);
      act = newerSide(localMs, re.updatedAt);
      if (act !== "conflict") {
        notes.push("conflict " + slug + " → took " + (act === "push" ? "local" : "remote") + " (newer)");
      }
    }

    switch (act) {
      case "none":
        counts.unchanged++;
        return act;
      case "push":
      case "create": {
        if (IPFS_REF_RE.test(le.body)) {
          notes.push("SKIP " + slug + " — body has legacy ipfs:// refs; fix them first");
          counts.conflict++;
          return "skip-ipfs";
        }
        const saved = act === "create"
          ? await this.api("POST", "/api/entries", this.pushPayload(le))
          : await this.api("PUT", "/api/entries/" + slug, this.pushPayload(le));
        le.updatedAt = saved.updatedAt || le.updatedAt;
        le.createdAt = saved.createdAt || le.createdAt;
        await this.writeEntryFile(lf.file.path, le);
        st.entries[slug] = {
          remoteCid: saved.cid, localHash: await entryHash(le), syncedAt: nowStamp(),
        };
        counts[act === "create" ? "create" : "push"]++;
        return act;
      }
      case "pull":
      case "pull-new": {
        const path = lf
          ? lf.file.path
          : this.settings.contentRoot + "/" + re.collection + "/" + slug + ".md";
        await this.writeEntryFile(path, re);
        st.entries[slug] = {
          remoteCid: re.cid, localHash: await entryHash(re), syncedAt: nowStamp(),
        };
        counts[act === "pull-new" ? "pull-new" : "pull"]++;
        return act;
      }
      case "conflict": {
        counts.conflict++;
        const sib = lf.file.path.replace(/\.md$/, ".remote.md");
        await this.writeEntryFile(sib, re);
        notes.push("CONFLICT " + slug + " — review " + sib);
        return act;
      }
      case "remote-gone":
        notes.push("NOTE " + slug + " — remote entry gone (trashed?); local kept");
        return act;
      case "local-gone":
        notes.push("NOTE " + slug + " — local file gone; remote kept");
        return act;
    }
    return act;
  }

  async saveSettings() {
    await this.saveData(this.settings);
    this.applyAutoSync();
  }
}

/* ── settings ──────────────────────────────────────────────────────────── */

class FarfieldSettingTab extends PluginSettingTab {
  constructor(app, plugin) {
    super(app, plugin);
    this.plugin = plugin;
  }

  display() {
    const { containerEl } = this;
    containerEl.empty();

    new Setting(containerEl)
      .setName("Content service URL")
      .addText((t) => t
        .setValue(this.plugin.settings.contentUrl)
        .onChange(async (v) => { this.plugin.settings.contentUrl = v.trim(); await this.plugin.saveSettings(); }));

    new Setting(containerEl)
      .setName("API key")
      .setDesc("The content write key. Stored in this plugin's data.json — keep it out of any vault git remote.")
      .addText((t) => {
        t.inputEl.type = "password";
        t.setValue(this.plugin.settings.apiKey)
          .onChange(async (v) => { this.plugin.settings.apiKey = v.trim(); await this.plugin.saveSettings(); });
      });

    new Setting(containerEl)
      .setName("Content folder")
      .setDesc("Vault folder whose subfolders are collections.")
      .addText((t) => t
        .setValue(this.plugin.settings.contentRoot)
        .onChange(async (v) => { this.plugin.settings.contentRoot = v.trim().replace(/\/$/, ""); await this.plugin.saveSettings(); }));

    new Setting(containerEl)
      .setName("Auto-sync every (minutes)")
      .setDesc("0 disables; sync always available from the ribbon or command palette.")
      .addText((t) => t
        .setValue(String(this.plugin.settings.autoSyncMinutes))
        .onChange(async (v) => { this.plugin.settings.autoSyncMinutes = Number(v) || 0; await this.plugin.saveSettings(); }));

    new Setting(containerEl)
      .setName("Blobs service URL")
      .setDesc("For the manual per-note media upload.")
      .addText((t) => t
        .setValue(this.plugin.settings.blobsUrl)
        .onChange(async (v) => { this.plugin.settings.blobsUrl = v.trim(); await this.plugin.saveSettings(); }));

    new Setting(containerEl)
      .setName("Blobs API key")
      .setDesc("A blobs write key — a scoped key from the keys app works. Empty disables media upload.")
      .addText((t) => {
        t.inputEl.type = "password";
        t.setValue(this.plugin.settings.blobsKey)
          .onChange(async (v) => { this.plugin.settings.blobsKey = v.trim(); await this.plugin.saveSettings(); });
      });
  }
}

module.exports = FarfieldSyncPlugin;
// Exposed for the node test harness only.
module.exports.__internals = { entryHash, decide, newerSide, toRFC3339, vaultTime, renderEntryFile, scanLocalRefs, mediaKindFor, blobRefsIn };
