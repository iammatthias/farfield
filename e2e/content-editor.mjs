// content editor end-to-end suite.
//
// Drives the real editor (lib/editor: WASM, canvas-drawn) in a real browser
// and asserts the invariant everything else depends on: opening a document
// and saving it back never corrupts the stored markdown (byte-identical round
// trip), and typed edits serialize to correct markdown.
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

  // ── round trip: open the editor, save with no edits ──
  // The editor draws to a canvas (lib/editor, WASM); typing goes through a
  // hidden textarea sink. It adds .ready to its host once the engine is up.
  await page.goto(`${BASE}/entries/${slug}/edit`);
  await page.waitForSelector(".ff-doc.ready");
  await page.waitForTimeout(300);

  const saveAndWait = async () => {
    await page.click("[data-doc-save]");
    await page.waitForFunction(() =>
      /^Saved/.test(document.querySelector(".save-note")?.textContent ?? ""));
  };
  await saveAndWait();
  const untouched = await (await api("/api/entries/" + slug)).json();
  check("round trip is byte-identical", untouched.body.trim() === BODY.trim(),
    JSON.stringify(untouched.body.slice(0, 120)));

  // ── typed edits serialize correctly ──
  const mod = process.platform === "darwin" ? "Meta" : "Control";
  await page.focus(".ff-editor-sink");
  await page.keyboard.press(mod + "+a");
  await page.keyboard.press("ArrowRight"); // collapse to the end
  await page.keyboard.press("Enter");
  await page.keyboard.press("Enter");
  await page.keyboard.type("## Appendix");
  await page.waitForFunction(() =>
    /Unsaved/.test(document.querySelector(".save-note")?.textContent ?? ""));
  await page.keyboard.press(mod + "+s");
  await page.waitForFunction(() =>
    /^Saved/.test(document.querySelector(".save-note")?.textContent ?? ""));
  const after = await (await api("/api/entries/" + slug)).json();
  check("typed heading saves as markdown, nothing else changed",
    after.body.trim() === BODY.trim() + "\n\n## Appendix", JSON.stringify(after.body.slice(-80)));

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
