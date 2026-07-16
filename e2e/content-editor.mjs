// content editor end-to-end suite.
//
// Drives the real rich editor in a real browser and asserts the invariant
// everything else depends on: opening a document and saving it back never
// corrupts the stored markdown (byte-identical round trip), and edits made
// through the rich surface serialize to correct markdown.
//
// Usage:
//   make dev                      # fleet on localhost with demo credentials
//   npm i --no-save playwright    # once (also: npx playwright install chromium)
//   make e2e
//
// Env: E2E_BASE (default http://127.0.0.1:8787), E2E_PASSWORD (demo),
//      E2E_API_KEY (dev-content-key) — the write key used to seed/clean up.
import { chromium } from "playwright";

const BASE = process.env.E2E_BASE || "http://127.0.0.1:8787";
const PASSWORD = process.env.E2E_PASSWORD || "demo";
const API_KEY = process.env.E2E_API_KEY || "dev-content-key";
const SLUG = "e2e-editor-check";

const BODY = `Opening paragraph with **bold**, *italic*, and a [link](https://example.org).

## Structure survives

- one
- two

> a quote

| a | b |
|---|---|
| 1 | 2 |

\`\`\`bash
echo "fences survive"
\`\`\`

Closing line.`;

const fails = [];
function check(name, ok, detail = "") {
  if (ok) console.log("  ok  " + name);
  else { console.error("  FAIL " + name + (detail ? " — " + detail : "")); fails.push(name); }
}

async function api(path, opts = {}) {
  const r = await fetch(BASE + path, {
    ...opts,
    headers: { "X-API-Key": API_KEY, "Content-Type": "application/json", ...(opts.headers || {}) },
  });
  if (!r.ok && opts.okStatus !== r.status) throw new Error(path + " -> " + r.status);
  return r;
}

const browser = await chromium.launch();
let slug = null;
try {
  const page = await (await browser.newContext()).newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));

  await page.goto(BASE + "/login");
  await page.fill("input[name=password]", PASSWORD);
  await page.click("button[type=submit]");
  await page.waitForURL(BASE + "/");

  // ── seed: own collection (idempotent) + a fresh entry ──
  console.log("seeding " + SLUG);
  await page.evaluate(() => fetch("/collections", {
    method: "POST",
    body: new URLSearchParams({ name: "E2E", slug: "e2e" }),
  }));
  await api("/api/entries/" + SLUG, { method: "DELETE", okStatus: 404 }).catch(() => {});
  const created = await (await api("/api/entries", {
    method: "POST",
    body: JSON.stringify({
      collection: "e2e",
      slug: SLUG, title: "E2E editor check", body: BODY, published: false, tags: ["e2e"],
    }),
  })).json();
  slug = created.slug; // the server stamps slugs

  // ── round trip: open rich, flip to markdown with no edits ──
  await page.goto(`${BASE}/entries/${slug}/edit`);
  await page.click(".doc-card");
  await page.waitForSelector("dialog.doc-editor[open] .doc-rich");
  await page.waitForTimeout(400);
  check("verbatim table block", await page.locator(".doc-rich pre.md-verbatim").count() > 0);

  await page.click('dialog.doc-editor .seg button:has-text("Markdown")');
  const roundtripped = await page.locator("dialog.doc-editor textarea#body").inputValue();
  check("round trip is byte-identical", roundtripped.trim() === BODY.trim(),
    JSON.stringify(roundtripped.slice(0, 120)));

  // ── rich edits serialize correctly ──
  await page.click('dialog.doc-editor .seg button:has-text("Edit")');
  await page.waitForSelector("dialog.doc-editor .doc-rich");
  await page.waitForTimeout(300);

  await page.evaluate(() => {
    const rich = document.querySelector(".doc-rich");
    const p = document.createElement("p");
    p.innerHTML = "<br>";
    rich.appendChild(p);
    const r = document.createRange();
    r.selectNodeContents(p); r.collapse(true);
    const s = getSelection(); s.removeAllRanges(); s.addRange(r);
  });
  await page.keyboard.type("## Appendix");
  check("typing ## makes a heading", await page.evaluate(() =>
    [...document.querySelectorAll(".doc-rich h2")]
      .some((h) => h.textContent.replace(/ /g, " ") === "Appendix")));

  await page.keyboard.press("Meta+s");
  await page.waitForFunction(() =>
    /^Saved/.test(document.querySelector("dialog.doc-editor .doc-state").textContent));
  const after = await (await api("/api/entries/" + slug)).json();
  check("saved markdown has the heading", after.body.includes("## Appendix"));
  check("saved markdown keeps the table", after.body.includes("| a | b |"));
  check("saved markdown keeps the fence", after.body.includes('echo "fences survive"'));

  // ── autosave: type in the surface, wait, expect a save with no ⌘S ──
  await page.evaluate(() => {
    const h = [...document.querySelectorAll(".doc-rich h2")].at(-1);
    const r = document.createRange();
    r.selectNodeContents(h); r.collapse(false);
    const sel = getSelection(); sel.removeAllRanges(); sel.addRange(r);
    document.querySelector(".doc-rich").focus();
  });
  await page.keyboard.type(" autosaved-token");
  await page.waitForFunction(() =>
    /Unsaved/.test(document.querySelector("dialog.doc-editor .doc-state").textContent));
  await page.waitForFunction(() =>
    /^Saved/.test(document.querySelector("dialog.doc-editor .doc-state").textContent),
    null, { timeout: 9000 });
  const after2 = await (await api("/api/entries/" + slug)).json();
  check("autosave persisted", after2.body.includes("autosaved-token"));

  check("no page errors", errors.length === 0, errors.join("; "));
} finally {
  await browser.close();
  if (slug) {
    await api("/api/entries/" + slug, { method: "DELETE" }).catch(() => {});
    console.log("cleaned up " + slug);
  }
}

if (fails.length) { console.error("\n" + fails.length + " failure(s)"); process.exit(1); }
console.log("\nall checks passed");
