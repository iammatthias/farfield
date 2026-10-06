//! Renders the app icon (a Horizon signal on the Deep Space field, the
//! mark's own palette from docs/BRAND.md) at every size an .icns needs.
//!
//!   cargo run -p farfield-desktop --bin mkicon -- <out.iconset>
use image::{Rgba, RgbaImage};

fn render(n: u32) -> RgbaImage {
    let mut img = RgbaImage::new(n, n);
    let f = n as f32;
    let (space, blue, horizon, paper) = ([14u8, 34, 45], [13u8, 53, 96], [229u8, 159, 103], [243u8, 229, 209]);
    let r = f * 0.225; // corner radius of the macOS squircle-ish tile
    let inset = f * 0.1;
    for y in 0..n {
        for x in 0..n {
            let (px, py) = (x as f32 + 0.5, y as f32 + 0.5);
            // rounded tile
            let (lo, hi) = (inset, f - inset);
            let cx = px.clamp(lo + r, hi - r);
            let cy = py.clamp(lo + r, hi - r);
            let d = ((px - cx).powi(2) + (py - cy).powi(2)).sqrt() - r;
            let a = (0.5 - d).clamp(0.0, 1.0);
            if a <= 0.0 {
                continue;
            }
            // field: Deep Space above, Farfield Blue toward the horizon
            let t = ((py - lo) / (hi - lo)).clamp(0.0, 1.0);
            let mut c = [0f32; 3];
            for i in 0..3 {
                c[i] = space[i] as f32 * (1.0 - t * 0.8) + blue[i] as f32 * (t * 0.8);
            }
            // the horizon: a thin Paper line two-thirds down
            let hy = lo + (hi - lo) * 0.66;
            let line = (1.0 - ((py - hy).abs() / (f * 0.006).max(0.6))).clamp(0.0, 1.0) * 0.55;
            for i in 0..3 {
                c[i] = c[i] * (1.0 - line) + paper[i] as f32 * line;
            }
            // the signal: a Horizon disc resting on the line
            let (sx, sy, sr) = (f * 0.5, hy - f * 0.13, f * 0.11);
            let sd = ((px - sx).powi(2) + (py - sy).powi(2)).sqrt() - sr;
            let sa = (0.5 - sd).clamp(0.0, 1.0);
            for i in 0..3 {
                c[i] = c[i] * (1.0 - sa) + horizon[i] as f32 * sa;
            }
            img.put_pixel(x, y, Rgba([c[0] as u8, c[1] as u8, c[2] as u8, (a * 255.0) as u8]));
        }
    }
    img
}

fn main() {
    let out = std::env::args().nth(1).expect("usage: mkicon <dir.iconset>");
    std::fs::create_dir_all(&out).unwrap();
    for (name, n) in [
        ("icon_16x16.png", 16),
        ("icon_16x16@2x.png", 32),
        ("icon_32x32.png", 32),
        ("icon_32x32@2x.png", 64),
        ("icon_128x128.png", 128),
        ("icon_128x128@2x.png", 256),
        ("icon_256x256.png", 256),
        ("icon_256x256@2x.png", 512),
        ("icon_512x512.png", 512),
        ("icon_512x512@2x.png", 1024),
    ] {
        render(n).save(format!("{out}/{name}")).unwrap();
    }
}
