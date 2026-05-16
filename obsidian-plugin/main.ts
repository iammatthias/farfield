// Farfield Publisher — an Obsidian plugin that publishes notes and media to a
// Farfield backend over its authenticated HTTP API.
//
// Every request goes through Obsidian's `requestUrl`, so the plugin works on
// desktop and mobile alike — no CLI, no child process, no CORS.
//
//   - Publish the current note   — PUT /records/{collection}/{rkey}
//   - Upload pasted/dropped media — POST /blobs, then a media record
//
// The collection is the note's parent folder. A note under a `feed` folder
// publishes to the feed service; every other folder to the content service.

import {
  App,
  Editor,
  Notice,
  Plugin,
  PluginSettingTab,
  Setting,
  TFile,
  requestUrl,
  RequestUrlResponse,
} from "obsidian";

interface FarfieldSettings {
  contentUrl: string;
  feedUrl: string;
  blobsUrl: string;
  token: string;
}

const DEFAULT_SETTINGS: FarfieldSettings = {
  contentUrl: "https://content.farfield.systems",
  feedUrl: "https://feed.farfield.systems",
  blobsUrl: "https://blobs.farfield.systems",
  token: "",
};

// A lexicon-lite schema, as served by GET /schemas/{collection}.
interface SchemaField {
  type: string;
  items?: SchemaField;
}
interface Schema {
  required?: string[];
  properties: Record<string, SchemaField>;
}

export default class FarfieldPlugin extends Plugin {
  settings: FarfieldSettings = DEFAULT_SETTINGS;
  private uploadCounter = 0;

  async onload(): Promise<void> {
    await this.loadSettings();
    this.addSettingTab(new FarfieldSettingTab(this.app, this));

    this.addCommand({
      id: "publish-current-note",
      name: "Publish current note",
      checkCallback: (checking: boolean) => {
        const file = this.app.workspace.getActiveFile();
        if (!file || file.extension !== "md") return false;
        if (!checking) void this.publish(file);
        return true;
      },
    });

    this.addCommand({
      id: "check-status",
      name: "Check service status",
      callback: () => void this.checkStatus(),
    });

    // Route pasted / dropped media through the blob store.
    this.registerEvent(
      this.app.workspace.on("editor-paste", (evt, editor) => {
        const files = mediaFiles(evt.clipboardData);
        if (files.length === 0) return;
        evt.preventDefault();
        void this.handleFiles(files, editor);
      }),
    );
    this.registerEvent(
      this.app.workspace.on("editor-drop", (evt, editor) => {
        const files = mediaFiles(evt.dataTransfer);
        if (files.length === 0) return;
        evt.preventDefault();
        void this.handleFiles(files, editor);
      }),
    );
  }

  // ---- publish -------------------------------------------------------------

  private async publish(file: TFile): Promise<void> {
    const collection = file.parent?.name;
    if (!collection || file.parent?.isRoot()) {
      new Notice("Farfield: a note must live inside a collection folder.");
      return;
    }
    const service = this.serviceFor(collection);

    new Notice(`Farfield: publishing ${file.basename}…`);
    try {
      const schema = await this.fetchSchema(service, collection);
      const { frontmatter, body } = await this.readNote(file);
      const record = buildRecord(schema, frontmatter, body);
      const rkey = pickRkey(frontmatter, file);
      if (!validRkey(rkey)) {
        new Notice(`Farfield: "${rkey}" is not a valid rkey ([a-z0-9-], 1–128).`, 8000);
        return;
      }
      const resp = await this.api("PUT", `${service}/records/${collection}/${rkey}`, record);
      if (resp.status < 300) {
        new Notice(`Farfield: published ${collection}/${rkey} ✓`);
      } else {
        new Notice(`Farfield: ${collection}/${rkey} — ${apiError(resp)}`, 8000);
      }
    } catch (e) {
      new Notice(`Farfield: ${errMessage(e)}`, 8000);
    }
  }

  // serviceFor routes a collection to its service: `feed` to the feed service,
  // everything else to the content service.
  private serviceFor(collection: string): string {
    return collection === "feed" ? this.settings.feedUrl : this.settings.contentUrl;
  }

  private async fetchSchema(service: string, collection: string): Promise<Schema> {
    const resp = await this.api("GET", `${service}/schemas/${collection}`);
    if (resp.status === 404) throw new Error(`unknown collection "${collection}"`);
    if (resp.status >= 300) throw new Error(`fetching schema: ${apiError(resp)}`);
    return resp.json as Schema;
  }

  private async readNote(
    file: TFile,
  ): Promise<{ frontmatter: Record<string, unknown>; body: string }> {
    const content = await this.app.vault.read(file);
    const cache = this.app.metadataCache.getFileCache(file);
    const frontmatter = (cache?.frontmatter ?? {}) as Record<string, unknown>;
    // Strip a leading YAML frontmatter block; what remains is the body.
    const body = content
      .replace(/^---\r?\n[\s\S]*?\r?\n---\r?\n?/, "")
      .replace(/^\s+/, "");
    return { frontmatter, body };
  }

  // ---- media ---------------------------------------------------------------

  private async handleFiles(files: File[], editor: Editor): Promise<void> {
    for (const file of files) {
      const token = `![uploading ${file.name} #${++this.uploadCounter}…]()`;
      editor.replaceSelection(token + "\n");
      try {
        const cid = await this.uploadMedia(await file.arrayBuffer());
        const embed =
          file.type.startsWith("image/") || file.type.startsWith("video/")
            ? `![](blob://${cid})`
            : `[${file.name}](blob://${cid})`;
        replaceToken(editor, token, embed);
        new Notice(`Farfield: uploaded ${file.name} ✓`);
      } catch (e) {
        replaceToken(editor, token, "");
        new Notice(`Farfield: upload failed — ${errMessage(e)}`, 8000);
      }
    }
  }

  // uploadMedia POSTs bytes to the blob service, then records the media entry
  // on the content service (rkey = the blob CID).
  private async uploadMedia(data: ArrayBuffer): Promise<string> {
    const up = await this.api("POST", `${this.settings.blobsUrl}/blobs`, data);
    if (up.status >= 300) throw new Error(apiError(up));
    const meta = up.json as Record<string, unknown>;
    const cid = typeof meta.cid === "string" ? meta.cid : "";
    if (!cid) throw new Error("blob service returned no CID");
    const rec = { ...meta, created: nowRFC3339() };
    const put = await this.api("PUT", `${this.settings.contentUrl}/records/media/${cid}`, rec);
    if (put.status >= 300) throw new Error(`media record: ${apiError(put)}`);
    return cid;
  }

  // ---- status --------------------------------------------------------------

  private async checkStatus(): Promise<void> {
    const services: ReadonlyArray<readonly [string, string]> = [
      ["content", this.settings.contentUrl],
      ["feed", this.settings.feedUrl],
      ["blobs", this.settings.blobsUrl],
    ];
    const lines: string[] = [];
    for (const [name, url] of services) {
      try {
        const r = await this.api("GET", `${url}/status`);
        lines.push(`${name}: ${r.status < 300 ? "ok" : "HTTP " + r.status}`);
      } catch {
        lines.push(`${name}: unreachable`);
      }
    }
    new Notice("Farfield —\n" + lines.join("\n"), 8000);
  }

  // ---- http ----------------------------------------------------------------

  // api makes an authenticated request through Obsidian's requestUrl, which
  // works on mobile and is not subject to CORS. A plain object body is sent as
  // JSON; an ArrayBuffer as raw bytes. Never throws on HTTP status.
  private async api(
    method: string,
    url: string,
    body?: unknown,
  ): Promise<RequestUrlResponse> {
    const headers: Record<string, string> = {};
    if (this.settings.token) headers["Authorization"] = `Bearer ${this.settings.token}`;

    let payload: string | ArrayBuffer | undefined;
    if (body instanceof ArrayBuffer) {
      payload = body;
      headers["Content-Type"] = "application/octet-stream";
    } else if (body !== undefined) {
      payload = JSON.stringify(body);
      headers["Content-Type"] = "application/json";
    }
    return requestUrl({ url, method, headers, body: payload, throw: false });
  }

  async loadSettings(): Promise<void> {
    this.settings = Object.assign({}, DEFAULT_SETTINGS, await this.loadData());
  }

  async saveSettings(): Promise<void> {
    await this.saveData(this.settings);
  }
}

// ---- record building -------------------------------------------------------

// buildRecord projects a note's frontmatter onto a collection's schema: every
// declared field, coerced to its type; `body` from the markdown body; unknown
// frontmatter keys dropped (the server rejects them). A required datetime that
// is absent — common for quick feed posts — is stamped with the current time.
function buildRecord(
  schema: Schema,
  frontmatter: Record<string, unknown>,
  body: string,
): Record<string, unknown> {
  const required = new Set(schema.required ?? []);
  const out: Record<string, unknown> = {};
  for (const [name, field] of Object.entries(schema.properties ?? {})) {
    if (name === "body") {
      out.body = body;
    } else if (name in frontmatter && frontmatter[name] != null) {
      out[name] = coerce(field, frontmatter[name]);
    } else if (required.has(name) && field.type === "datetime") {
      out[name] = nowRFC3339();
    }
  }
  return out;
}

function coerce(field: SchemaField, value: unknown): unknown {
  switch (field.type) {
    case "string":
      return String(value);
    case "datetime":
      return toRFC3339(value);
    case "boolean":
      if (typeof value === "boolean") return value;
      return ["true", "yes", "on", "1"].includes(String(value).toLowerCase());
    case "integer":
    case "float":
      return typeof value === "number" ? value : Number(value);
    case "array":
      if (Array.isArray(value) && field.items) {
        return value.map((v) => coerce(field.items as SchemaField, v));
      }
      return value;
    default:
      return value;
  }
}

// toRFC3339 normalizes a frontmatter datetime to an RFC3339 UTC string. An
// unparseable value is passed through so the server can report it.
function toRFC3339(value: unknown): string {
  if (value instanceof Date) return stripMillis(value);
  const s = String(value).trim();
  const d = new Date(s);
  return isNaN(d.getTime()) ? s : stripMillis(d);
}

function nowRFC3339(): string {
  return stripMillis(new Date());
}

function stripMillis(d: Date): string {
  return d.toISOString().replace(/\.\d{3}Z$/, "Z");
}

function pickRkey(frontmatter: Record<string, unknown>, file: TFile): string {
  const slug = frontmatter.slug;
  if (typeof slug === "string" && slug.trim()) return slug.trim();
  return file.basename;
}

function validRkey(rkey: string): boolean {
  return /^[a-z0-9-]{1,128}$/.test(rkey);
}

// ---- media helpers ---------------------------------------------------------

// mediaFiles returns the image / video / audio / PDF files in a clipboard or
// drag payload — the ones worth routing to the blob store.
function mediaFiles(dt: DataTransfer | null): File[] {
  if (!dt || dt.files.length === 0) return [];
  return Array.from(dt.files).filter(
    (f) =>
      f.type.startsWith("image/") ||
      f.type.startsWith("video/") ||
      f.type.startsWith("audio/") ||
      f.type === "application/pdf",
  );
}

// replaceToken swaps the first occurrence of a placeholder for the final text.
// The upload is async, so the token may have moved; a fresh search keeps it
// correct. If the user deleted the token, this is a no-op.
function replaceToken(editor: Editor, token: string, replacement: string): void {
  const content = editor.getValue();
  const idx = content.indexOf(token);
  if (idx === -1) return;
  const end = content.startsWith("\n", idx + token.length)
    ? idx + token.length + 1
    : idx + token.length;
  editor.replaceRange(
    replacement ? replacement + "\n" : "",
    editor.offsetToPos(idx),
    editor.offsetToPos(end),
  );
}

// ---- misc ------------------------------------------------------------------

function apiError(resp: RequestUrlResponse): string {
  try {
    const j = resp.json as { message?: string } | undefined;
    if (j && j.message) return j.message;
  } catch {
    /* response body was not JSON */
  }
  return `HTTP ${resp.status}`;
}

function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

class FarfieldSettingTab extends PluginSettingTab {
  constructor(
    app: App,
    private plugin: FarfieldPlugin,
  ) {
    super(app, plugin);
  }

  display(): void {
    const { containerEl } = this;
    containerEl.empty();

    const s = this.plugin.settings;
    const field = (
      name: string,
      desc: string,
      get: () => string,
      set: (v: string) => void,
      password = false,
    ): void => {
      new Setting(containerEl)
        .setName(name)
        .setDesc(desc)
        .addText((text) => {
          text.setValue(get()).onChange(async (v) => {
            set(v.trim());
            await this.plugin.saveSettings();
          });
          if (password) text.inputEl.type = "password";
          text.inputEl.style.width = "320px";
        });
    };

    field(
      "Content service URL",
      "Where content collections (posts, art, …) publish.",
      () => s.contentUrl,
      (v) => (s.contentUrl = v),
    );
    field(
      "Feed service URL",
      "Where notes in a `feed` folder publish.",
      () => s.feedUrl,
      (v) => (s.feedUrl = v),
    );
    field(
      "Blob service URL",
      "Where pasted media uploads.",
      () => s.blobsUrl,
      (v) => (s.blobsUrl = v),
    );
    field(
      "Write token",
      "FARFIELD_TOKEN — the bearer token for authenticated writes.",
      () => s.token,
      (v) => (s.token = v),
      true,
    );
  }
}
