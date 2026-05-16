// Farfield Publisher — a thin Obsidian wrapper over the `farfield` CLI.
//
// Two jobs:
//   1. Publish the current note    — shells out to `farfield push`.
//   2. Upload pasted/dropped media — shells out to `farfield upload-media`,
//      then rewrites the embed to `blob://<cid>`.
//
// The CLI does the real work (frontmatter parsing, schema validation, the
// blob/media records); this plugin is just the Obsidian-side ergonomics. It
// is desktop-only — spawning the binary needs Node's child_process.

import {
  App,
  Editor,
  FileSystemAdapter,
  Notice,
  Plugin,
  PluginSettingTab,
  Setting,
  TFile,
} from "obsidian";
import { execFile } from "child_process";
import { promises as fs } from "fs";
import * as os from "os";
import * as path from "path";

interface FarfieldSettings {
  binaryPath: string;
  contentUrl: string;
  blobsUrl: string;
  schemasPath: string;
  token: string;
}

const DEFAULT_SETTINGS: FarfieldSettings = {
  binaryPath: "",
  contentUrl: "https://content.farfield.systems",
  blobsUrl: "https://blobs.farfield.systems",
  schemasPath: "",
  token: "",
};

interface RunResult {
  code: number;
  stdout: string;
  stderr: string;
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

    // Intercept pasted / dropped media and route it through the blob store.
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

  // ---- commands ------------------------------------------------------------

  private async publish(file: TFile): Promise<void> {
    const abs = this.absPath(file);
    if (!abs) {
      new Notice("Farfield: this vault is not on a local filesystem.");
      return;
    }
    const args = ["push", abs, "--service", this.settings.contentUrl];
    if (this.settings.schemasPath) {
      args.push("--schemas", this.settings.schemasPath);
    }
    new Notice(`Farfield: publishing ${file.basename}…`);
    try {
      const r = await this.run(args);
      if (r.code === 0) {
        new Notice(`Farfield: published ${file.basename} ✓`);
      } else {
        new Notice(`Farfield: publish failed — ${firstLine(r.stderr || r.stdout)}`, 8000);
      }
    } catch (e) {
      new Notice(`Farfield: ${errMessage(e)}`, 8000);
    }
  }

  private async checkStatus(): Promise<void> {
    try {
      const r = await this.run(["status", "--service", this.settings.contentUrl]);
      if (r.code === 0) {
        new Notice(`Farfield status:\n${r.stdout.trim()}`, 8000);
      } else {
        new Notice(`Farfield: ${firstLine(r.stderr || r.stdout)}`, 8000);
      }
    } catch (e) {
      new Notice(`Farfield: ${errMessage(e)}`, 8000);
    }
  }

  // ---- media upload --------------------------------------------------------

  private async handleFiles(files: File[], editor: Editor): Promise<void> {
    for (const file of files) {
      const token = `![uploading ${file.name} #${++this.uploadCounter}…]()`;
      editor.replaceSelection(token + "\n");
      try {
        const data = await file.arrayBuffer();
        const cid = await this.uploadMedia(data, file.name);
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

  // uploadMedia writes the bytes to a temp file, runs `farfield upload-media`,
  // and returns the blob CID it prints.
  private async uploadMedia(data: ArrayBuffer, name: string): Promise<string> {
    const tmp = path.join(os.tmpdir(), `farfield-${Date.now()}-${safeName(name)}`);
    await fs.writeFile(tmp, Buffer.from(data));
    try {
      const r = await this.run([
        "upload-media",
        tmp,
        "--blobs",
        this.settings.blobsUrl,
        "--content",
        this.settings.contentUrl,
      ]);
      if (r.code !== 0) {
        throw new Error(firstLine(r.stderr || r.stdout) || "upload failed");
      }
      const cid = r.stdout.trim().split("\n")[0].trim();
      if (!cid) throw new Error("the CLI returned no CID");
      return cid;
    } finally {
      void fs.unlink(tmp).catch(() => {});
    }
  }

  // ---- helpers -------------------------------------------------------------

  // run spawns the configured farfield binary, passing the write token in the
  // environment, and resolves with its exit code and output.
  private run(args: string[]): Promise<RunResult> {
    return new Promise((resolve, reject) => {
      if (!this.settings.binaryPath) {
        reject(new Error("set the farfield binary path in plugin settings"));
        return;
      }
      execFile(
        this.settings.binaryPath,
        args,
        {
          env: { ...process.env, FARFIELD_TOKEN: this.settings.token },
          maxBuffer: 16 * 1024 * 1024,
        },
        (err, stdout, stderr) => {
          const code =
            err && typeof err.code === "number" ? err.code : err ? 1 : 0;
          resolve({ code, stdout: stdout ?? "", stderr: stderr ?? "" });
        },
      );
    });
  }

  // absPath resolves a vault file to an absolute filesystem path, or null when
  // the vault is not on a local filesystem.
  private absPath(file: TFile): string | null {
    const adapter = this.app.vault.adapter;
    if (!(adapter instanceof FileSystemAdapter)) return null;
    return path.join(adapter.getBasePath(), file.path);
  }

  async loadSettings(): Promise<void> {
    this.settings = Object.assign({}, DEFAULT_SETTINGS, await this.loadData());
  }

  async saveSettings(): Promise<void> {
    await this.saveData(this.settings);
  }
}

// mediaFiles returns the image / video / audio / PDF files in a clipboard or
// drag payload — the ones worth routing to the blob store. Anything else
// (plain text, markdown files) is left for Obsidian to handle.
function mediaFiles(dt: DataTransfer | null): File[] {
  if (!dt || dt.files.length === 0) return [];
  return Array.from(dt.files).filter(isMedia);
}

function isMedia(file: File): boolean {
  return (
    file.type.startsWith("image/") ||
    file.type.startsWith("video/") ||
    file.type.startsWith("audio/") ||
    file.type === "application/pdf"
  );
}

// replaceToken swaps the first occurrence of a placeholder token for the final
// text. The upload is async, so the token may have moved; a fresh search keeps
// it correct. If the user deleted the token, this is a no-op.
function replaceToken(editor: Editor, token: string, replacement: string): void {
  const content = editor.getValue();
  const idx = content.indexOf(token);
  if (idx === -1) return;
  // Also consume the trailing newline that was inserted with the token.
  const end = content.startsWith("\n", idx + token.length)
    ? idx + token.length + 1
    : idx + token.length;
  editor.replaceRange(
    replacement ? replacement + "\n" : "",
    editor.offsetToPos(idx),
    editor.offsetToPos(end),
  );
}

function safeName(name: string): string {
  return name.replace(/[^a-zA-Z0-9._-]/g, "_");
}

function firstLine(s: string): string {
  return s.trim().split("\n")[0].trim();
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

    const field = (
      name: string,
      desc: string,
      placeholder: string,
      get: () => string,
      set: (v: string) => void,
      password = false,
    ): void => {
      new Setting(containerEl)
        .setName(name)
        .setDesc(desc)
        .addText((text) => {
          text
            .setPlaceholder(placeholder)
            .setValue(get())
            .onChange(async (v) => {
              set(v.trim());
              await this.plugin.saveSettings();
            });
          if (password) text.inputEl.type = "password";
          text.inputEl.style.width = "320px";
        });
    };

    const s = this.plugin.settings;

    field(
      "Farfield binary",
      "Absolute path to the compiled `farfield` CLI binary.",
      "/Users/you/Developer/farfield/farfield",
      () => s.binaryPath,
      (v) => (s.binaryPath = v),
    );
    field(
      "Content service URL",
      "The content/records service the notes publish to.",
      DEFAULT_SETTINGS.contentUrl,
      () => s.contentUrl,
      (v) => (s.contentUrl = v),
    );
    field(
      "Blob service URL",
      "The blob service pasted media uploads to.",
      DEFAULT_SETTINGS.blobsUrl,
      () => s.blobsUrl,
      (v) => (s.blobsUrl = v),
    );
    field(
      "Schema directory",
      "Path to the farfield repo's `schemas/content` — used to validate notes before publishing.",
      "/Users/you/Developer/farfield/schemas/content",
      () => s.schemasPath,
      (v) => (s.schemasPath = v),
    );
    field(
      "Write token",
      "FARFIELD_TOKEN — passed to the CLI in the environment for authenticated writes.",
      "",
      () => s.token,
      (v) => (s.token = v),
      true,
    );
  }
}
