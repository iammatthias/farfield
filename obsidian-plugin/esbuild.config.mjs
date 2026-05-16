// Bundles main.ts -> main.js. `node esbuild.config.mjs` for a dev build with
// an inline sourcemap and watch; `node esbuild.config.mjs production` for a
// minified one-shot build.
import esbuild from "esbuild";
import process from "process";

const production = process.argv[2] === "production";

const options = {
  entryPoints: ["main.ts"],
  bundle: true,
  // Obsidian, Electron, and Node builtins are provided by the host at runtime.
  external: ["obsidian", "electron", "child_process", "fs", "os", "path"],
  format: "cjs",
  target: "es2020",
  platform: "node",
  logLevel: "info",
  sourcemap: production ? false : "inline",
  treeShaking: true,
  minify: production,
  outfile: "main.js",
};

if (production) {
  await esbuild.build(options);
} else {
  const ctx = await esbuild.context(options);
  await ctx.watch();
}
