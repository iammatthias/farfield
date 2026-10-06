//! Farfield v5, natively: the tokens are lib/theme/theme.css's custom
//! properties (generated at build time), the faces are its vendored fonts.
//!
//! The rules carried over from the stylesheet: structure from space and
//! full-width horizon rules, never boxes; borders only for an input's edge and
//! focus; shadows only on things that float; Horizon (the signal) stays rare.
//! Inter for interface chrome, Newsreader only inside documents, IBM Plex Mono
//! for technical readouts.

use gpui::{px, App, BoxShadow, Global, Hsla, Pixels, Rgba, WindowAppearance};
use std::borrow::Cow;

pub mod tokens {
    include!(concat!(env!("OUT_DIR"), "/theme_tokens.rs"));
}

pub const FONT_UI: &str = "Inter";
pub const FONT_DOC: &str = "Newsreader 16pt";
pub const FONT_MONO: &str = "IBM Plex Mono";

/// Spacing scale (--s-1 … --s-9).
pub const S1: Pixels = px(4.);
pub const S2: Pixels = px(8.);
pub const S3: Pixels = px(12.);
pub const S4: Pixels = px(16.);
pub const S5: Pixels = px(24.);
pub const S6: Pixels = px(32.);
pub const R_S: Pixels = px(3.);
pub const TOP_H: Pixels = px(48.);
/// --measure, the reading width of a document (47rem).
pub const MEASURE: Pixels = px(752.);

#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum Mode {
    #[default]
    System,
    Light,
    Dark,
}

#[derive(Clone, Debug)]
pub struct Theme {
    pub dark: bool,
    pub paper: Hsla,
    pub paper_2: Hsla,
    pub ink: Hsla,
    pub ink_2: Hsla,
    pub ink_3: Hsla,
    pub rule: Hsla,
    pub rule_strong: Hsla,
    pub wash: Hsla,
    pub accent: Hsla,
    pub accent_ink: Hsla,
    pub accent_soft: Hsla,
    pub signal: Hsla,
    pub good: Hsla,
    pub warn: Hsla,
    pub bad: Hsla,
    pub bad_soft: Hsla,
    pub select: Hsla,
    pub float: Hsla,
    /// Raw 0xRRGGBBAA values for the editor's palette.
    pub raw_paper: u32,
    pub raw_ink: u32,
    pub raw_accent: u32,
    pub raw_bad: u32,
    pub reduced_motion: bool,
}

impl Global for Theme {}

/// Parse `#rrggbb`, `#rgb` or `rgba(r, g, b, a)` into 0xRRGGBBAA.
pub fn parse_color(v: &str) -> Option<u32> {
    let v = v.trim();
    if let Some(h) = v.strip_prefix('#') {
        let h = if h.len() == 3 { h.chars().flat_map(|c| [c, c]).collect::<String>() } else { h.to_string() };
        let n = u32::from_str_radix(&h, 16).ok()?;
        return Some(if h.len() == 8 { n } else { (n << 8) | 0xff });
    }
    let inner = v.strip_prefix("rgba(").or_else(|| v.strip_prefix("rgb("))?.strip_suffix(')')?;
    let parts: Vec<f32> = inner.split(',').filter_map(|p| p.trim().parse().ok()).collect();
    if parts.len() < 3 {
        return None;
    }
    let a = parts.get(3).copied().unwrap_or(1.0);
    Some(
        ((parts[0] as u32) << 24) | ((parts[1] as u32) << 16) | ((parts[2] as u32) << 8) | ((a * 255.0).round() as u32),
    )
}

fn hsla(c: u32) -> Hsla {
    Rgba {
        r: ((c >> 24) & 255) as f32 / 255.0,
        g: ((c >> 16) & 255) as f32 / 255.0,
        b: ((c >> 8) & 255) as f32 / 255.0,
        a: (c & 255) as f32 / 255.0,
    }
    .into()
}

fn token(set: &[(&str, &str)], name: &str) -> u32 {
    set.iter()
        .find(|(k, _)| *k == name)
        .and_then(|(_, v)| parse_color(v))
        .unwrap_or_else(|| panic!("theme.css token --{name} missing or not a colour"))
}

impl Theme {
    pub fn new(dark: bool) -> Self {
        // dark tokens override the light ones they name
        let get = |name: &str| {
            if dark && tokens::DARK.iter().any(|(k, _)| *k == name) {
                token(tokens::DARK, name)
            } else {
                token(tokens::LIGHT, name)
            }
        };
        Theme {
            dark,
            paper: hsla(get("paper")),
            paper_2: hsla(get("paper-2")),
            ink: hsla(get("ink")),
            ink_2: hsla(get("ink-2")),
            ink_3: hsla(get("ink-3")),
            rule: hsla(get("rule")),
            rule_strong: hsla(get("rule-strong")),
            wash: hsla(get("wash")),
            accent: hsla(get("accent")),
            accent_ink: hsla(get("accent-ink")),
            accent_soft: hsla(get("accent-soft")),
            signal: hsla(get("signal")),
            good: hsla(get("good")),
            warn: hsla(get("warn")),
            bad: hsla(get("bad")),
            bad_soft: hsla(get("bad-soft")),
            select: hsla(get("select")),
            float: hsla(get("float")),
            raw_paper: get("paper"),
            raw_ink: get("ink"),
            raw_accent: get("accent"),
            raw_bad: get("bad"),
            reduced_motion: false,
        }
    }

    pub fn for_appearance(mode: Mode, appearance: WindowAppearance) -> Self {
        let dark = match mode {
            Mode::Light => false,
            Mode::Dark => true,
            Mode::System => matches!(appearance, WindowAppearance::Dark | WindowAppearance::VibrantDark),
        };
        Theme::new(dark)
    }

    /// The shadow for things that float (menus, the palette, toasts) — and
    /// only those.
    pub fn float_shadow(&self) -> Vec<BoxShadow> {
        let a = if self.dark { 0.45 } else { 0.14 };
        vec![
            BoxShadow {
                color: Hsla { h: 0., s: 0., l: 0., a: if self.dark { 0.3 } else { 0.08 } },
                offset: gpui::point(px(0.), px(1.)),
                blur_radius: px(2.),
                spread_radius: px(0.),
            },
            BoxShadow {
                color: Hsla { h: 0.55, s: 0.5, l: 0.1, a },
                offset: gpui::point(px(0.), px(12.)),
                blur_radius: px(32.),
                spread_radius: px(0.),
            },
        ]
    }

    pub fn editor_palette(&self) -> farfield_editor::Palette {
        farfield_editor::Palette {
            paper: self.raw_paper,
            ink: self.raw_ink,
            accent: self.raw_accent,
            bad: self.raw_bad,
        }
    }
}

pub fn theme(cx: &App) -> &Theme {
    cx.global::<Theme>()
}

/// Register the brand faces: the theme's vendored Inter and Plex Mono, and the
/// editor's static Newsreader and Plex Mono (the same files the editor
/// rasterizes, so document chrome and document text match).
pub fn load_fonts(cx: &mut App) {
    // the theme's Newsreader is a latin-subset variable face that registers
    // under the same name as the editor's static cuts; two faces behind one
    // name shape with one and rasterize with the other, so only the editor's
    // complete static Newsreader is registered
    let mut fonts: Vec<Cow<'static, [u8]>> =
        tokens::FONTS.iter().filter(|(family, _)| *family != "Newsreader").map(|(_, b)| Cow::Borrowed(*b)).collect();
    for f in farfield_editor::assets::FONTS {
        fonts.push(Cow::Borrowed(f));
    }
    cx.text_system().add_fonts(fonts).expect("register brand fonts");
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn colours_parse() {
        assert_eq!(parse_color("#f3e5d1"), Some(0xf3e5d1ff));
        assert_eq!(parse_color("rgba(14, 34, 45, 0.12)"), Some(0x0e222d1f));
        assert_eq!(parse_color("var(--x)"), None);
    }

    #[test]
    fn both_palettes_resolve_from_theme_css() {
        let l = Theme::new(false);
        let d = Theme::new(true);
        assert_eq!(l.raw_paper, 0xf3e5d1ff);
        assert_eq!(d.raw_paper, 0x0e222dff);
        assert_eq!(d.raw_accent, 0xe59f67ff); // Horizon carries action by night
        assert_eq!(l.raw_accent, 0x0d3560ff); // Farfield Blue by day
    }
}
