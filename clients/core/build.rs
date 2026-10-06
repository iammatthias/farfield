// The fleet (names, ports, public hosts) and the quick-action table come from
// lib/fleet and lib/capability via lib/capability/cmd/manifest — the tables
// every Go surface reads — never from a copy kept here.
//
// FARFIELD_MANIFEST points at a pre-exported file for a build without Go.
use std::{env, path::PathBuf, process::Command};

fn main() {
    let manifest = PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap());
    let repo = manifest.join("../..").canonicalize().unwrap();
    let out = PathBuf::from(env::var("OUT_DIR").unwrap()).join("fleet.json");
    println!("cargo:rerun-if-env-changed=FARFIELD_MANIFEST");
    if let Ok(pre) = env::var("FARFIELD_MANIFEST") {
        std::fs::copy(pre, &out).expect("copy FARFIELD_MANIFEST");
        return;
    }
    for p in ["lib/fleet/fleet.go", "lib/capability/commands.go", "lib/capability/cmd/manifest"] {
        println!("cargo:rerun-if-changed={}", repo.join(p).display());
    }
    let o = Command::new("go")
        .current_dir(&repo)
        .args(["run", "./lib/capability/cmd/manifest"])
        .output()
        .expect("go is needed to export the fleet manifest (or set FARFIELD_MANIFEST)");
    assert!(o.status.success(), "manifest: {}", String::from_utf8_lossy(&o.stderr));
    std::fs::write(&out, o.stdout).unwrap();
}
