#!/usr/bin/env node
/* Test harness for the Farfield Sync plugin — plain node, zero deps.
 *
 * Shims the `obsidian` module, then exercises the pure helpers (hash,
 * decide table, LWW, time round-trip) and drives syncEntry/syncFile/runSync
 * against an in-memory vault and a scripted API. Run: node test.js
 */
"use strict";

const crypto = require("crypto");
const Module = require("module");

/* ── obsidian shim ─────────────────────────────────────────────────────── */

const notices = [];
const shim = {
  Plugin: class {},
  PluginSettingTab: class {},
  Setting: class {
    setName() { return this; }
    setDesc() { return this; }
    addText() { return this; }
  },
  Notice: class {
    constructor(msg) { notices.push(String(msg)); }
  },
  requestUrl: async () => { throw new Error("requestUrl must be stubbed in tests"); },
  // Enough of stringifyYaml for the fixtures below (plain scalars).
  stringifyYaml: (v) => {
    if (typeof v === "string" && /^[\w .,'’—-]*$/.test(v) && v === v.trim() && v !== "") return v + "\n";
    return JSON.stringify(v === undefined ? "" : v) + "\n";
  },
};
const origLoad = Module._load;
Module._load = (request, parent, isMain) =>
  request === "obsidian" ? shim : origLoad(request, parent, isMain);

const FarfieldSyncPlugin = require("./main.js");
const { entryHash, decide, newerSide, toRFC3339, vaultTime, renderEntryFile } =
  FarfieldSyncPlugin.__internals;

/* ── tiny check runner ─────────────────────────────────────────────────── */

let failures = 0;
function check(name, ok, detail) {
  if (ok) {
    console.log("  ok  " + name);
  } else {
    failures++;
    console.error("FAIL  " + name + (detail ? " — " + detail : ""));
  }
}

/* ── in-memory plugin ──────────────────────────────────────────────────── */

// Minimal frontmatter parser for files written by renderEntryFile.
function parseFM(raw) {
  const m = (raw || "").match(/^---\n([\s\S]*?)\n---/);
  if (!m) return {};
  const fm = {};
  let listKey = null;
  for (const line of m[1].split("\n")) {
    const li = line.match(/^ {2}- (.*)$/);
    if (li && listKey) { fm[listKey].push(unquote(li[1])); continue; }
    const kv = line.match(/^(\w+):\s*(.*)$/);
    if (!kv) continue;
    listKey = null;
    const k = kv[1], v = kv[2];
    if (v === "") { fm[k] = []; listKey = k; }
    else if (v === "[]") fm[k] = [];
    else if (v === "true") fm[k] = true;
    else if (v === "false") fm[k] = false;
    else fm[k] = unquote(v);
  }
  return fm;
}
function unquote(s) {
  s = s.trim();
  if (s.startsWith('"') && s.endsWith('"')) return JSON.parse(s);
  return s;
}

function makePlugin() {
  const p = Object.create(FarfieldSyncPlugin.prototype);
  p.settings = { contentUrl: "https://x", apiKey: "k", contentRoot: "content", autoSyncMinutes: 0 };
  p.files = new Map();   // path -> content
  p.mtimes = new Map();  // path -> ms
  p.remote = new Map();  // slug -> server entry
  p.apiCalls = [];
  p.cidSeq = 0;
  p.serverNow = "2026-07-23T12:00:00Z";
  p.syncing = false;

  const fileObj = (path) => ({
    path,
    basename: path.split("/").pop().replace(/\.md$/, ""),
    stat: { mtime: p.mtimes.get(path) || 0 },
  });
  p.fileObj = fileObj;

  p.app = {
    vault: {
      getMarkdownFiles: () =>
        [...p.files.keys()].filter((x) => x.endsWith(".md")).map(fileObj),
      read: async (f) => p.files.get(f.path),
      modify: async (f, c) => { p.files.set(f.path, c); },
      create: async (path, c) => { p.files.set(path, c); },
      getAbstractFileByPath: (path) => (p.files.has(path) ? fileObj(path) : null),
      adapter: {
        read: async (path) => {
          if (!p.files.has(path)) throw new Error("missing " + path);
          return p.files.get(path);
        },
        write: async (path, c) => { p.files.set(path, c); },
      },
    },
    metadataCache: {
      getFileCache: (f) => ({ frontmatter: parseFM(p.files.get(f.path)) }),
    },
  };

  p.api = async (method, path, body, allow404) => {
    p.apiCalls.push({ method, path, body });
    if (method === "GET" && path.startsWith("/api/entries/")) {
      const slug = decodeURIComponent(path.slice("/api/entries/".length));
      const e = p.remote.get(slug);
      if (!e) {
        if (allow404) return null;
        throw new Error("GET " + path + ": HTTP 404");
      }
      return e;
    }
    if (method === "GET") return { entries: [...p.remote.values()] };
    if (method === "POST" || method === "PUT") {
      const slug = method === "POST" ? body.slug : path.slice("/api/entries/".length);
      const e = Object.assign({}, body, {
        slug,
        cid: "cid-" + ++p.cidSeq,
        updatedAt: p.serverNow,
        createdAt: body.createdAt || p.serverNow,
      });
      p.remote.set(slug, e);
      return e;
    }
    throw new Error("unexpected " + method + " " + path);
  };

  return p;
}

function serverEntry(p, over) {
  return Object.assign({
    collection: "essays", slug: "one", title: "One", excerpt: "an essay",
    body: "hello world", tags: ["a", "b"], published: true,
    cid: "cid-" + ++p.cidSeq,
    createdAt: "2026-07-01T00:00:00Z", updatedAt: "2026-07-10T00:00:00Z",
  }, over);
}

async function main() {
  /* ── hash parity ── */
  // This constant is pinned in Go too (TestEntryHashVector in
  // apps/content/sync_test.go) — if either implementation drifts, one of
  // the two suites breaks. Belt: recompute the joining here as well.
  const VECTOR = "09cdd1dd8c4e3b16d8887f7f2a251db42991785adc559fa2e4b16b9190559471";
  const got = await entryHash({ title: "T", excerpt: "E", tags: ["a", "b"], published: true, body: "  B \n" });
  check("entryHash matches the cross-tool vector", got === VECTOR, got);
  check("entryHash joins with NUL and trims the body",
    got === crypto.createHash("sha256").update("T\0E\0a,b\0true\0B\0", "utf8").digest("hex"));

  /* ── decide table (parity with sync.go) ── */
  const table = [
    [[true, true, true, false, false], "none"],
    [[true, true, true, true, false], "push"],
    [[true, true, true, false, true], "pull"],
    [[true, true, true, true, true], "conflict"],
    [[true, false, false, true, false], "create"],
    [[true, false, true, false, false], "remote-gone"],
    [[false, true, false, false, true], "pull-new"],
    [[false, true, true, false, false], "local-gone"],
    [[false, false, true, false, false], "none"],
  ];
  for (const [args, wantAct] of table) {
    check("decide(" + args.join(",") + ") = " + wantAct, decide(...args) === wantAct);
  }

  /* ── last write wins ── */
  const t = Date.parse("2026-07-10T00:00:00Z");
  check("LWW: local newer pushes", newerSide(t + 1000, "2026-07-10T00:00:00Z") === "push");
  check("LWW: remote newer pulls", newerSide(t - 1000, "2026-07-10T00:00:00Z") === "pull");
  check("LWW: tie stays conflict", newerSide(t, "2026-07-10T00:00:00Z") === "conflict");
  check("LWW: bad remote stamp stays conflict", newerSide(t, "not-a-date") === "conflict");
  check("LWW: zero local stays conflict", newerSide(0, "2026-07-10T00:00:00Z") === "conflict");

  /* ── time round-trip ── */
  check("toRFC3339 vault stamp", toRFC3339("2026-07-16 09:30") === "2026-07-16T09:30:00Z");
  check("vaultTime round-trips", vaultTime("2026-07-16T09:30:00Z") === "2026-07-16 09:30");

  /* ── seed on equal ── */
  {
    const p = makePlugin();
    const re = serverEntry(p);
    p.remote.set("one", re);
    p.files.set("content/essays/one.md", renderEntryFile(re));
    const sum = await p.runSync();
    check("seed-on-equal counts unchanged", sum.line.startsWith("1 unchanged"), sum.line);
    const st = JSON.parse(p.files.get("content/.farfield-sync.json"));
    check("seed-on-equal records remote cid", st.entries.one && st.entries.one.remoteCid === re.cid);
  }

  /* ── push a local edit ── */
  {
    const p = makePlugin();
    const re = serverEntry(p);
    p.remote.set("one", re);
    p.files.set("content/essays/one.md", renderEntryFile(re));
    await p.runSync(); // seed
    p.files.set("content/essays/one.md",
      p.files.get("content/essays/one.md").replace("hello world", "hello edited world"));
    const sum = await p.runSync();
    check("local edit pushes", sum.line.includes("1 push"), sum.line);
    check("push PUT the entry", p.apiCalls.some((c) => c.method === "PUT" && c.path === "/api/entries/one"));
    check("push updated the server body", p.remote.get("one").body.includes("hello edited world"));
  }

  /* ── pull a remote edit ── */
  {
    const p = makePlugin();
    const re = serverEntry(p);
    p.remote.set("one", re);
    p.files.set("content/essays/one.md", renderEntryFile(re));
    await p.runSync(); // seed
    p.remote.set("one", serverEntry(p, { body: "server rewrote this", updatedAt: "2026-07-11T00:00:00Z" }));
    const sum = await p.runSync();
    check("remote edit pulls", sum.line.includes("1 pull ·"), sum.line);
    check("pull rewrote the file", p.files.get("content/essays/one.md").includes("server rewrote this"));
  }

  /* ── conflict: most recently edited side wins ── */
  {
    const p = makePlugin();
    const re = serverEntry(p);
    p.remote.set("one", re);
    p.files.set("content/essays/one.md", renderEntryFile(re));
    await p.runSync(); // seed
    p.remote.set("one", serverEntry(p, { body: "server side", updatedAt: "2026-07-11T00:00:00Z" }));
    p.files.set("content/essays/one.md",
      p.files.get("content/essays/one.md").replace("hello world", "local side"));
    p.mtimes.set("content/essays/one.md", Date.parse("2026-07-12T00:00:00Z"));
    const sum = await p.runSync();
    check("both-changed, local newer → push", sum.line.includes("1 push"), sum.line);
    check("winner reached the server", p.remote.get("one").body.includes("local side"));
  }

  /* ── conflict: unknowable local time → sibling ── */
  {
    const p = makePlugin();
    const re = serverEntry(p);
    p.remote.set("one", re);
    p.files.set("content/essays/one.md", renderEntryFile(re).replace("updated:", "xupdated:"));
    // seed manually so both sides read as changed
    p.files.set("content/.farfield-sync.json", JSON.stringify({
      version: 1, entries: { one: { remoteCid: "stale", localHash: "stale", syncedAt: "x" } },
    }));
    p.remote.set("one", serverEntry(p, { body: "server side", updatedAt: "2026-07-11T00:00:00Z" }));
    const sum = await p.runSync();
    check("tie/unknown writes a sibling", sum.conflicts === 1 &&
      p.files.has("content/essays/one.remote.md"), sum.line);
  }

  /* ── ipfs guard ── */
  {
    const p = makePlugin();
    const re = serverEntry(p);
    p.remote.set("one", re);
    p.files.set("content/essays/one.md", renderEntryFile(re));
    await p.runSync(); // seed
    p.files.set("content/essays/one.md", p.files.get("content/essays/one.md")
      .replace("hello world", "![](ipfs://" + "b".repeat(50) + ")"));
    const before = p.apiCalls.length;
    const sum = await p.runSync();
    check("ipfs body refuses to push", sum.conflicts === 1 &&
      !p.apiCalls.slice(before).some((c) => c.method === "PUT"), sum.line);
  }

  /* ── single-note sync ── */
  {
    // create: local-only note goes up via POST, scoped API traffic only
    const p = makePlugin();
    const le = serverEntry(p, { slug: "two", title: "Two", body: "fresh note" });
    p.files.set("content/essays/two.md", renderEntryFile(le));
    notices.length = 0;
    await p.syncFile(p.fileObj("content/essays/two.md"));
    check("syncFile creates a new entry", p.apiCalls.some((c) => c.method === "POST"));
    check("syncFile fetched only its slug",
      !p.apiCalls.some((c) => c.method === "GET" && !c.path.startsWith("/api/entries/")));
    check("syncFile notice says created", notices.some((n) => n.includes("created on Farfield")), notices.join(" | "));
    const st = JSON.parse(p.files.get("content/.farfield-sync.json"));
    check("syncFile recorded state", !!st.entries.two);

    // up to date: second run is a no-op
    notices.length = 0;
    await p.syncFile(p.fileObj("content/essays/two.md"));
    check("syncFile no-op says up to date", notices.some((n) => n.includes("up to date")), notices.join(" | "));

    // pull: server changed, note follows
    p.remote.set("two", serverEntry(p, { slug: "two", title: "Two", body: "server tweak", updatedAt: "2026-07-20T00:00:00Z" }));
    notices.length = 0;
    await p.syncFile(p.fileObj("content/essays/two.md"));
    check("syncFile pulls remote edits", p.files.get("content/essays/two.md").includes("server tweak"),
      notices.join(" | "));

    // guard rails
    notices.length = 0;
    p.files.set("scratch.md", "not an entry");
    await p.syncFile(p.fileObj("scratch.md"));
    check("syncFile rejects non-entries", notices.some((n) => n.includes("not an entry")), notices.join(" | "));
    notices.length = 0;
    p.files.set("content/essays/two.remote.md", "sibling");
    await p.syncFile(p.fileObj("content/essays/two.remote.md"));
    check("syncFile explains conflict copies", notices.some((n) => n.includes("conflict copy")), notices.join(" | "));
  }

  console.log(failures ? "\n" + failures + " failure(s)" : "\nall checks passed");
  process.exit(failures ? 1 : 0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
