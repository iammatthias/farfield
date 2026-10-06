// The editor's bytes come from the Go workspace, never from a second
// assembler: lib/editor/cmd/export writes the exact editor.wasm, fonts and
// dictionary the browser and desk load, and this crate embeds them.
//
// FARFIELD_EDITOR_ASSETS points at a pre-exported directory instead, for a
// build machine without Go (the .app build exports once, then builds).
use std::{env, path::PathBuf, process::Command};

fn main() {
    let manifest = PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap());
    let repo = manifest.join("../..").canonicalize().unwrap();
    let out = PathBuf::from(env::var("OUT_DIR").unwrap()).join("editor-assets");

    println!("cargo:rerun-if-env-changed=FARFIELD_EDITOR_ASSETS");
    let dir = if let Ok(pre) = env::var("FARFIELD_EDITOR_ASSETS") {
        PathBuf::from(pre)
    } else {
        for p in ["lib/editor/wat", "lib/editor/fonts", "lib/editor/dict", "lib/editor/cmd/export", "lib/wat"] {
            println!("cargo:rerun-if-changed={}", repo.join(p).display());
        }
        let status = Command::new("go")
            .current_dir(&repo)
            .args(["run", "./lib/editor/cmd/export", "-out"])
            .arg(&out)
            .status()
            .expect("go is needed to export the editor (or set FARFIELD_EDITOR_ASSETS)");
        assert!(status.success(), "lib/editor/cmd/export failed");
        out
    };
    println!("cargo:rustc-env=FARFIELD_EDITOR_DIR={}", dir.display());
}
