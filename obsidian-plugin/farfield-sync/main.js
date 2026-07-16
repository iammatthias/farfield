/* Farfield Sync — bidirectional vault ↔ content-service sync, in-app.
 *
 * This is the same three-way merge as `content sync-vault` (apps/content/
 * sync.go) and shares its state file (content/.farfield-sync.json), so the
 * CLI and the plugin are interchangeable: per-entry state records the remote
 * CID and a local content hash from the last sync. Local-only changes push,
 * remote-only changes pull, both-changed conflicts write a `.remote.md`
 * sibling for review (or auto-resolve via the preference setting).
 * Deletions never propagate — a vanished side is reported.
 *
 * The hash MUST stay byte-compatible with entryHash in sync.go:
 * sha256(title \0 excerpt \0 tags-joined-by-comma \0 published \0 body-trimmed \0).
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
  prefer: "manual", // manual | local | remote
  autoSyncMinutes: 0, // 0 = manual only
};

const STATE_FILE = ".farfield-sync.json";
const IPFS_REF_RE = /ipfs:\/\/[A-Za-z0-9]{46,}/;

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

    this.addRibbonIcon("refresh-cw", "Sync with Farfield", () => this.sync());
    this.addCommand({
      id: "sync",
      name: "Sync with Farfield",
      callback: () => this.sync(),
    });
    this.addSettingTab(new FarfieldSettingTab(this.app, this));
    this.statusEl = this.addStatusBarItem();
    this.applyAutoSync();
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
  // notes, and conflict siblings are not entries.
  localEntries() {
    const root = this.settings.contentRoot + "/";
    const out = new Map();
    for (const f of this.app.vault.getMarkdownFiles()) {
      if (!f.path.startsWith(root)) continue;
      const rel = f.path.slice(root.length).split("/");
      if (rel.length !== 2) continue;
      if (f.path.endsWith(".remote.md")) continue;
      const fm = this.app.metadataCache.getFileCache(f)?.frontmatter || {};
      const slug = String(fm.slug || f.basename);
      out.set(slug, { file: f, collection: rel[0], fm });
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

  async api(method, path, body) {
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

  async runSync() {
    const prefer = this.settings.prefer;
    const [remote, st] = await Promise.all([this.fetchRemote(), this.loadState()]);
    const local = this.localEntries();

    const slugs = new Set([...local.keys(), ...remote.keys(), ...Object.keys(st.entries)]);
    const counts = { unchanged: 0, push: 0, create: 0, pull: 0, "pull-new": 0, conflict: 0 };
    const notes = [];
    const now = () => new Date().toISOString().replace(/\.\d{3}Z$/, "Z");

    for (const slug of [...slugs].sort()) {
      const lf = local.get(slug);
      const re = remote.get(slug);
      const rec = st.entries[slug];

      const le = lf ? await this.readEntry(lf, slug) : null;
      const lHash = le ? await entryHash(le) : "";
      const localChanged = !!le && (!rec || lHash !== rec.localHash);
      const remoteChanged = !!re && (!rec || re.cid !== rec.remoteCid);

      // First encounter, equal content: seed quietly (see sync.go).
      if (le && re && !rec && lHash === (await entryHash(re))) {
        st.entries[slug] = { remoteCid: re.cid, localHash: lHash, syncedAt: now() };
        counts.unchanged++;
        continue;
      }

      let act = decide(!!le, !!re, !!rec, localChanged, remoteChanged);
      if (act === "conflict" && prefer === "local") act = "push";
      if (act === "conflict" && prefer === "remote") act = "pull";

      switch (act) {
        case "none":
          counts.unchanged++;
          break;
        case "push":
        case "create": {
          if (IPFS_REF_RE.test(le.body)) {
            notes.push("SKIP " + slug + " — body has legacy ipfs:// refs; fix them first");
            counts.conflict++;
            break;
          }
          const saved = act === "create"
            ? await this.api("POST", "/api/entries", this.pushPayload(le))
            : await this.api("PUT", "/api/entries/" + slug, this.pushPayload(le));
          le.updatedAt = saved.updatedAt || le.updatedAt;
          le.createdAt = saved.createdAt || le.createdAt;
          await this.writeEntryFile(lf.file.path, le);
          st.entries[slug] = {
            remoteCid: saved.cid, localHash: await entryHash(le), syncedAt: now(),
          };
          counts[act === "create" ? "create" : "push"]++;
          break;
        }
        case "pull":
        case "pull-new": {
          const path = lf
            ? lf.file.path
            : this.settings.contentRoot + "/" + re.collection + "/" + slug + ".md";
          await this.writeEntryFile(path, re);
          st.entries[slug] = {
            remoteCid: re.cid, localHash: await entryHash(re), syncedAt: now(),
          };
          counts[act === "pull-new" ? "pull-new" : "pull"]++;
          break;
        }
        case "conflict": {
          counts.conflict++;
          const sib = lf.file.path.replace(/\.md$/, ".remote.md");
          await this.writeEntryFile(sib, re);
          notes.push("CONFLICT " + slug + " — review " + sib);
          break;
        }
        case "remote-gone":
          notes.push("NOTE " + slug + " — remote entry gone (trashed?); local kept");
          break;
        case "local-gone":
          notes.push("NOTE " + slug + " — local file gone; remote kept");
          break;
      }
    }

    await this.saveState(st);
    const line = `${counts.unchanged} unchanged · ${counts.push} push · ${counts.create} create · ` +
      `${counts.pull} pull · ${counts["pull-new"]} pull-new · ${counts.conflict} conflicts`;
    const changed = counts.push + counts.create + counts.pull + counts["pull-new"];
    return { line, notes, changed, conflicts: counts.conflict };
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
      .setName("When both sides changed")
      .addDropdown((d) => d
        .addOptions({ manual: "Keep both (write .remote.md)", local: "Prefer this vault", remote: "Prefer Farfield" })
        .setValue(this.plugin.settings.prefer)
        .onChange(async (v) => { this.plugin.settings.prefer = v; await this.plugin.saveSettings(); }));

    new Setting(containerEl)
      .setName("Auto-sync every (minutes)")
      .setDesc("0 disables; sync always available from the ribbon or command palette.")
      .addText((t) => t
        .setValue(String(this.plugin.settings.autoSyncMinutes))
        .onChange(async (v) => { this.plugin.settings.autoSyncMinutes = Number(v) || 0; await this.plugin.saveSettings(); }));
  }
}

module.exports = FarfieldSyncPlugin;
// Exposed for the node test harness only.
module.exports.__internals = { entryHash, decide, toRFC3339, vaultTime, renderEntryFile };
