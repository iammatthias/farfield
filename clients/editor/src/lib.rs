//! The farfield editor, hosted natively.
//!
//! `editor.wasm` is hand-written WebAssembly (lib/editor/wat) that owns the
//! document, Markdown styling, font rasterization, layout, selection, undo and
//! every pixel. This crate is only its host, the Rust twin of
//! lib/editor/engine.go and host.js: it moves input in and the framebuffer
//! out. The module bytes, fonts and dictionary are the ones the Go workspace
//! exports, so this runs exactly what the browser runs; the parity suite in
//! lib/editor/testdata/parity proves it.

use anyhow::{anyhow, bail, Result};
use std::collections::HashMap;
use wasmtime::{Engine as Runtime, Instance, Memory, Module, Store, Val};

/// The assets the build embedded, from lib/editor/cmd/export.
pub mod assets {
    pub const WASM: &[u8] = include_bytes!(concat!(env!("FARFIELD_EDITOR_DIR"), "/editor.wasm"));
    pub const DICT: &[u8] = include_bytes!(concat!(env!("FARFIELD_EDITOR_DIR"), "/en_US.txt"));
    pub const MANIFEST: &str = include_str!(concat!(env!("FARFIELD_EDITOR_DIR"), "/manifest.json"));
    /// Font slots 0–6 in the order the editor expects (see lib/editor/fonts.go).
    pub const FONTS: [&[u8]; 7] = [
        include_bytes!(concat!(env!("FARFIELD_EDITOR_DIR"), "/fonts/Newsreader16pt-Regular.ttf")),
        include_bytes!(concat!(env!("FARFIELD_EDITOR_DIR"), "/fonts/Newsreader16pt-SemiBold.ttf")),
        include_bytes!(concat!(env!("FARFIELD_EDITOR_DIR"), "/fonts/Newsreader16pt-Italic.ttf")),
        include_bytes!(concat!(env!("FARFIELD_EDITOR_DIR"), "/fonts/Newsreader16pt-SemiBoldItalic.ttf")),
        include_bytes!(concat!(env!("FARFIELD_EDITOR_DIR"), "/fonts/IBMPlexMono-Regular.ttf")),
        include_bytes!(concat!(env!("FARFIELD_EDITOR_DIR"), "/fonts/IBMPlexMono-SemiBold.ttf")),
        include_bytes!(concat!(env!("FARFIELD_EDITOR_DIR"), "/fonts/Newsreader16pt-Medium.ttf")),
    ];
}

/// Key codes, as edit.wat defines them.
#[repr(i32)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Key {
    Left = 1,
    Right,
    Up,
    Down,
    Home,
    End,
    PageUp,
    PageDown,
    Backspace,
    Delete,
    Enter,
    Tab,
    Escape,
}

/// Modifier bits, as edit.wat defines them.
pub mod mods {
    pub const SHIFT: i32 = 1;
    /// ⌥ on a Mac, Ctrl elsewhere.
    pub const WORD: i32 = 2;
    /// ⌘ on a Mac, Ctrl elsewhere.
    pub const CMD: i32 = 4;
}

/// Command ids, as edit.wat defines them.
#[repr(i32)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Command {
    Bold = 1,
    Italic,
    Code,
    Link,
    Strike,
    H1,
    H2,
    H3,
    Quote,
    Bullets,
    Numbers,
    CodeBlock,
    Undo,
    Redo,
    SelectAll,
    Rule,
    SelectWord,
    SelectLine,
}

/// Pointer event kinds for [`Editor::pointer`].
pub mod pointer {
    pub const DOWN: i32 = 1;
    pub const MOVE: i32 = 2;
    pub const UP: i32 = 3;
}

/// Colour slots for [`Editor::set_palette`], in host.js's order.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Palette {
    pub paper: u32,
    pub ink: u32,
    pub accent: u32,
    pub bad: u32,
}

impl Palette {
    /// The eleven colours host.js derives from the page's tokens, derived the
    /// same way so the native editor is indistinguishable from the browser's.
    pub fn slots(&self) -> [u32; 11] {
        let (bg, ink, accent) = (self.paper, self.ink, self.accent);
        [
            bg,
            ink,
            blend(bg, ink, 0.42),
            accent,
            blend(bg, ink, 0.08),
            blend(bg, accent, 0.22),
            accent,
            blend(bg, ink, 0.38),
            blend(bg, ink, 0.25),
            blend(bg, ink, 0.35),
            self.bad,
        ]
    }
}

/// Mix colour `b` over `a` by `t`, as host.js's blend() does (0xRRGGBBAA).
pub fn blend(a: u32, b: u32, t: f64) -> u32 {
    let ch = |c: u32, s: u32| ((c >> s) & 255) as f64;
    let mix = |s: u32| ((ch(a, s) * (1.0 - t) + ch(b, s) * t).round() as u32) & 255;
    (mix(24) << 24) | (mix(16) << 16) | (mix(8) << 8) | 255
}

/// A rectangle in device pixels, as the editor reports one.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Rect {
    pub x: i32,
    pub y: i32,
    pub w: i32,
    pub h: i32,
}

/// One running editor.
pub struct Editor {
    store: Store<()>,
    instance: Instance,
    memory: Memory,
    fns: HashMap<&'static str, wasmtime::Func>,
    w: u32,
    h: u32,
}

fn runtime() -> &'static Runtime {
    use std::sync::OnceLock;
    static RT: OnceLock<Runtime> = OnceLock::new();
    RT.get_or_init(Runtime::default)
}

fn module() -> Result<&'static Module> {
    use std::sync::OnceLock;
    static M: OnceLock<std::result::Result<Module, String>> = OnceLock::new();
    M.get_or_init(|| Module::new(runtime(), assets::WASM).map_err(|e| e.to_string()))
        .as_ref()
        .map_err(|e| anyhow!("editor: compile: {e}"))
}

impl Editor {
    /// Instantiate the editor and load the brand fonts and the dictionary,
    /// in the order every host follows (init, fonts, dictionary).
    pub fn new() -> Result<Self> {
        let mut store = Store::new(runtime(), ());
        let instance = Instance::new(&mut store, module()?, &[]).map_err(|e| anyhow!("editor: instantiate: {e}"))?;
        let memory = instance
            .get_memory(&mut store, "memory")
            .ok_or_else(|| anyhow!("editor: no memory export"))?;
        let mut e = Editor { store, instance, memory, fns: HashMap::new(), w: 0, h: 0 };
        e.call("init", &[])?;
        for (slot, font) in assets::FONTS.iter().enumerate() {
            let n = e.put(font)?;
            if e.call("font_load", &[slot as i32, n])? != 1 {
                bail!("editor: font slot {slot} did not load");
            }
        }
        Ok(e)
    }

    /// Load the spelling dictionary and turn checking on.
    pub fn load_dictionary(&mut self) -> Result<()> {
        let n = self.put(assets::DICT)?;
        self.call("dict_load", &[n])?;
        Ok(())
    }

    fn func(&mut self, name: &'static str) -> Result<wasmtime::Func> {
        if let Some(f) = self.fns.get(name) {
            return Ok(*f);
        }
        let f = self
            .instance
            .get_func(&mut self.store, name)
            .ok_or_else(|| anyhow!("editor: no export {name}"))?;
        self.fns.insert(name, f);
        Ok(f)
    }

    /// Call an export with i32 arguments, returning its i32 result (or 0).
    pub fn call(&mut self, name: &'static str, args: &[i32]) -> Result<i32> {
        let f = self.func(name)?;
        let params: Vec<Val> = args.iter().map(|a| Val::I32(*a)).collect();
        let n = f.ty(&self.store).results().len();
        let mut out = vec![Val::I32(0); n];
        f.call(&mut self.store, &params, &mut out)
            .map_err(|e| anyhow!("editor: {name}: {e}"))?;
        Ok(out.first().and_then(|v| v.i32()).unwrap_or(0))
    }

    fn io(&mut self) -> Result<usize> {
        Ok(self.call("io_ptr", &[])? as u32 as usize)
    }

    fn put(&mut self, b: &[u8]) -> Result<i32> {
        let cap = self.call("io_cap", &[])? as u32 as usize;
        if b.len() > cap {
            bail!("editor: {} bytes exceeds the io buffer ({cap})", b.len());
        }
        let p = self.io()?;
        self.memory.write(&mut self.store, p, b).map_err(|e| anyhow!("{e}"))?;
        Ok(b.len() as i32)
    }

    fn take(&mut self, n: i32) -> Result<String> {
        let p = self.io()?;
        let mut b = vec![0u8; n.max(0) as usize];
        self.memory.read(&self.store, p, &mut b).map_err(|e| anyhow!("{e}"))?;
        Ok(String::from_utf8_lossy(&b).into_owned())
    }

    fn rect(&mut self, name: &'static str) -> Result<Rect> {
        let p = self.call(name, &[])? as u32 as usize;
        let mut b = [0u8; 16];
        self.memory.read(&self.store, p, &mut b).map_err(|e| anyhow!("{e}"))?;
        let v = |i: usize| i32::from_le_bytes(b[i * 4..i * 4 + 4].try_into().unwrap());
        Ok(Rect { x: v(0), y: v(1), w: v(2), h: v(3) })
    }

    /// Set the surface in device pixels and the scale (1.0 = 100%).
    pub fn resize(&mut self, w: u32, h: u32, scale: f64) -> Result<()> {
        if self.call("resize", &[w as i32, h as i32, (scale * 100.0 + 0.5) as i32])? == 0 {
            bail!("editor: could not grow memory for the framebuffer");
        }
        self.w = w;
        self.h = h;
        Ok(())
    }

    pub fn size(&self) -> (u32, u32) {
        (self.w, self.h)
    }

    /// Draw if anything changed; returns whether it drew.
    pub fn render(&mut self) -> Result<bool> {
        Ok(self.call("render", &[])? == 1)
    }

    /// The RGBA framebuffer (w × h × 4), copied out of the module.
    pub fn framebuffer(&mut self) -> Result<Vec<u8>> {
        let p = self.call("fb_ptr", &[])? as u32 as usize;
        let mut b = vec![0u8; (self.w * self.h * 4) as usize];
        self.memory.read(&self.store, p, &mut b).map_err(|e| anyhow!("{e}"))?;
        Ok(b)
    }

    /// Copy the framebuffer into `dst`, converting RGBA to BGRA on the way,
    /// which is the order GPU image uploads on macOS expect.
    pub fn framebuffer_bgra(&mut self, dst: &mut Vec<u8>) -> Result<()> {
        let p = self.call("fb_ptr", &[])? as u32 as usize;
        let n = (self.w * self.h * 4) as usize;
        let data = self.memory.data(&self.store);
        let src = data.get(p..p + n).ok_or_else(|| anyhow!("editor: framebuffer out of bounds"))?;
        dst.clear();
        dst.extend_from_slice(src);
        for px in dst.chunks_exact_mut(4) {
            px.swap(0, 2);
        }
        Ok(())
    }

    pub fn set_text(&mut self, s: &str) -> Result<()> {
        let n = self.put(s.as_bytes())?;
        self.call("set_text", &[n])?;
        Ok(())
    }
    pub fn text(&mut self) -> Result<String> {
        let n = self.call("get_text", &[])?;
        self.take(n)
    }
    pub fn selection(&mut self) -> Result<String> {
        let n = self.call("get_selection", &[])?;
        self.take(n)
    }
    /// Selection as UTF-8 byte offsets (start ≤ end).
    pub fn selection_range(&mut self) -> Result<(u32, u32)> {
        Ok((self.call("sel_start", &[])? as u32, self.call("sel_end", &[])? as u32))
    }
    /// Set the selection by UTF-8 byte offsets (anchor, head).
    pub fn set_selection(&mut self, anchor: u32, head: u32) -> Result<()> {
        self.call("set_selection", &[anchor as i32, head as i32])?;
        Ok(())
    }
    pub fn insert(&mut self, s: &str) -> Result<()> {
        if s.is_empty() {
            return Ok(());
        }
        // a paste larger than the io buffer goes in pieces on char boundaries
        let cap = self.call("io_cap", &[])? as usize;
        let mut rest = s;
        while !rest.is_empty() {
            let mut cut = rest.len().min(cap);
            while !rest.is_char_boundary(cut) {
                cut -= 1;
            }
            let n = self.put(rest[..cut].as_bytes())?;
            self.call("insert_text", &[n])?;
            rest = &rest[cut..];
        }
        Ok(())
    }
    pub fn set_placeholder(&mut self, s: &str) -> Result<()> {
        let n = self.put(s.as_bytes())?;
        self.call("set_placeholder", &[n])?;
        Ok(())
    }
    /// Returns whether the editor handled the key.
    pub fn key(&mut self, k: Key, mods: i32) -> Result<bool> {
        Ok(self.call("key", &[k as i32, mods])? == 1)
    }
    pub fn command(&mut self, c: Command) -> Result<()> {
        self.call("command", &[c as i32])?;
        Ok(())
    }
    pub fn pointer(&mut self, kind: i32, x: i32, y: i32, mods: i32, clicks: i32) -> Result<()> {
        self.call("pointer", &[kind, x, y, mods, clicks])?;
        Ok(())
    }
    pub fn wheel(&mut self, dy: i32) -> Result<()> {
        self.call("wheel", &[dy])?;
        Ok(())
    }
    pub fn focus(&mut self, on: bool) -> Result<()> {
        self.call("focus", &[on as i32])?;
        Ok(())
    }
    pub fn tick(&mut self, ms: u32) -> Result<()> {
        self.call("tick", &[ms as i32])?;
        Ok(())
    }
    pub fn revision(&mut self) -> Result<u32> {
        Ok(self.call("revision", &[])? as u32)
    }
    pub fn words(&mut self) -> Result<u32> {
        Ok(self.call("word_count", &[])? as u32)
    }
    pub fn scroll_top(&mut self) -> Result<i32> {
        self.call("scroll_top", &[])
    }
    pub fn doc_height(&mut self) -> Result<i32> {
        self.call("doc_height", &[])
    }
    pub fn set_scroll(&mut self, y: i32) -> Result<()> {
        self.call("set_scroll", &[y])?;
        Ok(())
    }
    pub fn caret_rect(&mut self) -> Result<Rect> {
        self.rect("caret_rect")
    }
    pub fn sel_rect(&mut self) -> Result<Rect> {
        self.rect("sel_rect")
    }
    /// Plain-text mode turns the Markdown styler off (code, pastes).
    pub fn set_plain(&mut self, on: bool) -> Result<()> {
        self.call("set_mode", &[on as i32])?;
        Ok(())
    }
    pub fn set_spell(&mut self, on: bool) -> Result<()> {
        self.call("set_spell", &[on as i32])?;
        Ok(())
    }
    pub fn set_color(&mut self, slot: i32, rgba: u32) -> Result<()> {
        self.call("set_color", &[slot, rgba as i32])?;
        Ok(())
    }
    pub fn set_palette(&mut self, p: &Palette) -> Result<()> {
        for (i, c) in p.slots().iter().enumerate() {
            self.set_color(i as i32, *c)?;
        }
        Ok(())
    }
    /// The destination of a link under a surface point, or "".
    pub fn link_at(&mut self, x: i32, y: i32) -> Result<String> {
        let n = self.call("link_at", &[x, y])?;
        self.take(n)
    }

    /// Hand the editor a decoded image for `url` (the literal reference in
    /// the Markdown, e.g. `blob://cid`), already scaled to the column, as
    /// RGBA. The editor draws it beneath the line that references it.
    pub fn put_image(&mut self, url: &str, w: u32, h: u32, rgba: &[u8]) -> Result<bool> {
        let ptr = self.call("image_alloc", &[rgba.len() as i32])?;
        if ptr == 0 {
            return Ok(false);
        }
        self.memory.write(&mut self.store, ptr as u32 as usize, rgba).map_err(|e| anyhow!("{e}"))?;
        let n = self.put(url.as_bytes())?;
        self.call("image_put", &[n, w as i32, h as i32, ptr])?;
        Ok(true)
    }
}

/// The image references in a document that the editor would draw: a line
/// that is only `![alt](url)`, as host.js's IMAGE_LINE matches it.
pub fn image_refs(text: &str) -> Vec<String> {
    let mut out = Vec::new();
    for line in text.lines() {
        let t = line.trim_matches(|c| c == ' ' || c == '\t');
        let Some(rest) = t.strip_prefix("![") else { continue };
        let Some(close) = rest.find("](") else { continue };
        if rest[..close].contains(']') {
            continue;
        }
        let tail = &rest[close + 2..];
        let Some(url) = tail.strip_suffix(')') else { continue };
        if url.is_empty() || url.contains(char::is_whitespace) || url.contains(')') {
            continue;
        }
        if !out.iter().any(|u| u == url) {
            out.push(url.to_string());
        }
    }
    out
}

/// Convert a UTF-8 byte offset in `s` to a UTF-16 code-unit offset, the unit
/// platform text-input systems (IME) speak.
pub fn utf8_to_utf16(s: &str, byte: usize) -> usize {
    let byte = byte.min(s.len());
    s[..floor_boundary(s, byte)].encode_utf16().count()
}

/// Convert a UTF-16 offset in `s` to a UTF-8 byte offset.
pub fn utf16_to_utf8(s: &str, u16off: usize) -> usize {
    let mut n = 0;
    for (i, c) in s.char_indices() {
        if n >= u16off {
            return i;
        }
        n += c.len_utf16();
    }
    s.len()
}

fn floor_boundary(s: &str, mut i: usize) -> usize {
    while i > 0 && !s.is_char_boundary(i) {
        i -= 1;
    }
    i
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn image_refs_match_host_js() {
        let t = "a\n![x](blob://abc)\n  ![y](https://e.com/a.png)  \ntext ![z](blob://no)\n![bad](a b)\n";
        assert_eq!(image_refs(t), vec!["blob://abc", "https://e.com/a.png"]);
    }

    #[test]
    fn utf16_round_trip() {
        let s = "a👋🏽é";
        assert_eq!(utf8_to_utf16(s, s.len()), 1 + 2 + 2 + 1);
        assert_eq!(utf16_to_utf8(s, 3), 5);
        for (i, _) in s.char_indices() {
            assert_eq!(utf16_to_utf8(s, utf8_to_utf16(s, i)), i);
        }
    }

    #[test]
    fn blend_matches_host_js() {
        // host.js: blend(0xf3e5d1ff, 0x0e222dff, 0.42)
        let got = blend(0xf3e5d1ff, 0x0e222dff, 0.42);
        let want = {
            let m = |a: f64, b: f64| ((a * 0.58 + b * 0.42).round() as u32) & 255;
            (m(243., 14.) << 24) | (m(229., 34.) << 16) | (m(209., 45.) << 8) | 255
        };
        assert_eq!(got, want);
    }
}
