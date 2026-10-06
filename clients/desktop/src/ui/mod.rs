//! Shared pieces of the v5 interface: buttons, horizon rules, readouts,
//! chips, notices. No boxes — structure is space and full-width rules.

pub mod doc_editor;
pub mod draft_doc;
pub mod input;

use crate::theme::{theme, Theme, FONT_DOC, FONT_MONO, R_S};
use gpui::{div, prelude::*, px, AnyElement, App, ClickEvent, Div, ElementId, Hsla, SharedString, Stateful, Window};

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    /// The one primary action in view: accent fill.
    Primary,
    /// Everything else: text with an underline on hover.
    Quiet,
    /// Destructive: the alarm colour, still quiet until hovered.
    Danger,
}

/// A button. Keyboard-focusable; Enter/Space activate via GPUI's click.
pub fn button(
    id: impl Into<ElementId>,
    label: impl Into<SharedString>,
    kind: Kind,
    cx: &App,
    on_click: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
) -> Stateful<Div> {
    let t = theme(cx).clone();
    let label: SharedString = label.into();
    let d = div()
        .id(id.into())
        .px(px(10.))
        .py(px(5.))
        .rounded(R_S)
        .text_sm()
        .cursor_pointer()
        .child(label)
        .on_click(on_click);
    match kind {
        Kind::Primary => d
            .bg(t.accent)
            .text_color(t.accent_ink)
            .font_weight(gpui::FontWeight::MEDIUM)
            .hover(|s| s.opacity(0.9)),
        Kind::Quiet => {
            let ink = t.ink;
            d.text_color(t.ink_2).hover(move |s| s.text_color(ink).bg(t.wash))
        }
        Kind::Danger => {
            let bad = t.bad;
            let soft = t.bad_soft;
            d.text_color(bad).hover(move |s| s.bg(soft))
        }
    }
}

/// A disabled-looking button that does nothing (keeps layout stable).
pub fn button_disabled(label: impl Into<SharedString>, cx: &App) -> Div {
    let t = theme(cx);
    div().px(px(10.)).py(px(5.)).text_sm().text_color(t.ink_3).child(label.into())
}

/// A full-width horizon rule.
pub fn rule(cx: &App) -> Div {
    div().w_full().h(px(1.)).bg(theme(cx).rule)
}

/// A small uppercase-tracked section label (the `.tech` style).
pub fn eyebrow(s: impl Into<SharedString>, cx: &App) -> Div {
    let t = theme(cx);
    div().text_xs().text_color(t.ink_2).font_weight(gpui::FontWeight::MEDIUM).child(s.into().to_uppercase())
}

/// A technical readout in Plex Mono (CIDs, slugs, sizes, timestamps).
pub fn mono(s: impl Into<SharedString>, cx: &App) -> Div {
    let t = theme(cx);
    div().font_family(FONT_MONO).text_xs().text_color(t.ink_2).child(s.into())
}

/// Document-voice text (titles of documents) in Newsreader.
pub fn doc_title(s: impl Into<SharedString>, cx: &App) -> Div {
    let t = theme(cx);
    div().font_family(FONT_DOC).text_size(px(17.)).text_color(t.ink).child(s.into())
}

/// A key–value row for the inspector.
pub fn field_row(label: impl Into<SharedString>, value: impl IntoElement, cx: &App) -> Div {
    let t = theme(cx);
    div()
        .flex()
        .flex_col()
        .gap(px(2.))
        .py(px(6.))
        .child(div().text_xs().text_color(t.ink_2).child(label.into()))
        .child(value)
}

/// A status chip: a dot and a word.
pub fn chip(label: impl Into<SharedString>, color: Hsla, cx: &App) -> Div {
    let t = theme(cx);
    div()
        .flex()
        .items_center()
        .gap(px(5.))
        .text_xs()
        .text_color(t.ink_2)
        .child(div().w(px(6.)).h(px(6.)).rounded_full().bg(color))
        .child(label.into())
}

/// A notice line (not a box): coloured text with a left rule.
pub fn notice(s: impl Into<SharedString>, color: Hsla, cx: &App) -> Div {
    let t = theme(cx);
    div()
        .border_l_2()
        .border_color(color)
        .pl(px(10.))
        .py(px(4.))
        .text_sm()
        .text_color(t.ink)
        .child(s.into())
}

/// An empty or loading state.
pub fn quiet_state(s: impl Into<SharedString>, cx: &App) -> Div {
    let t = theme(cx);
    div().p(px(24.)).text_sm().text_color(t.ink_2).child(s.into())
}

/// A row in a list: selected rows carry the accent wash and a left mark.
pub fn list_row(id: impl Into<ElementId>, selected: bool, t: &Theme) -> Stateful<Div> {
    let wash = t.wash;
    div()
        .id(id.into())
        .w_full()
        .px(px(14.))
        .py(px(8.))
        .border_b_1()
        .border_color(t.rule)
        .cursor_pointer()
        .when(selected, |d| d.bg(t.accent_soft).border_l_2().border_color(t.accent))
        .when(!selected, move |d| d.hover(move |s| s.bg(wash)))
}

/// Human-friendly byte size.
pub fn bytes(n: i64) -> String {
    let n = n as f64;
    if n < 1024.0 {
        format!("{n} B")
    } else if n < 1024.0 * 1024.0 {
        format!("{:.1} KB", n / 1024.0)
    } else if n < 1024.0 * 1024.0 * 1024.0 {
        format!("{:.1} MB", n / 1024.0 / 1024.0)
    } else {
        format!("{:.2} GB", n / 1024.0 / 1024.0 / 1024.0)
    }
}

/// A date-time string trimmed to minutes for display.
pub fn when(s: &str) -> String {
    if s.len() >= 16 {
        s[..16].replace('T', " ")
    } else {
        s.to_string()
    }
}

/// A floating surface (palette, menus, sheets, toasts) — the only things
/// that carry a shadow.
pub fn floating(cx: &App) -> Div {
    let t = theme(cx);
    div().bg(t.float).rounded(px(8.)).shadow(t.float_shadow()).border_1().border_color(t.rule)
}

pub fn any<E: IntoElement>(e: E) -> AnyElement {
    e.into_any_element()
}
