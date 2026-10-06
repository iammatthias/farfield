//! Keys: no API by design. Keys are minted in the private keys console — a
//! browser page on the tailnet, behind the administrator password, which is
//! typed there and never here. This workspace shows which
//! services this profile holds a key for (a hint only, never the key), and
//! hands off: to the console to mint or revoke, to Connections to paste.

use crate::app::{self, state, Health};
use crate::shell::goto;
use crate::theme::{theme, FONT_MONO, S2, S3, S4, S5};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::Workspace;
use gpui::{div, prelude::*, px, App, Context, Hsla, Window};

/// How each service is keyed, from lib/keys.Attach and each app's web.Auth.
fn accepts(service: &str) -> &'static str {
    match service {
        "blobs" | "bookmarks" | "content" | "feed" | "library" | "qr" | "scrap" | "sideload" | "switchboard" => {
            "minted or env key"
        }
        "pulse" => "PULSE_READ_KEY only",
        "backup" => "BACKUP_API_KEY only",
        "keys" => "password (console)",
        "daily" | "apex" => "public — no key",
        _ => "",
    }
}

pub struct KeysWs;

impl KeysWs {
    pub fn new(_w: &mut Window, _cx: &mut Context<Self>) -> Self {
        KeysWs
    }
}

impl Workspace for KeysWs {
    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        vec![
            ("console", "Keys: open console (browser)".into(), ""),
            ("paste", "Keys: paste a key in Settings".into(), "⌘,"),
        ]
    }

    fn run_command(&mut self, id: &str, _w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "console" => crate::ws::connections::open_console(cx, "keys"),
            "paste" => goto(cx, "connections"),
            _ => {}
        }
    }
}

impl Render for KeysWs {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let session = app::session(cx);
        let health = state(cx).health.clone();

        let mut rows = div().flex().flex_col();
        rows = rows.child(
            div()
                .flex()
                .items_center()
                .px(px(14.))
                .pb(px(6.))
                .border_b_1()
                .border_color(t.rule_strong)
                .text_xs()
                .text_color(t.ink_2)
                .child(div().w(px(130.)).flex_none().child("SERVICE"))
                .child(div().w(px(190.)).flex_none().child("ACCEPTS"))
                .child(div().flex_1().child("STORED ON THIS MAC"))
                .child(div().w(px(110.)).flex_none().child("STATUS")),
        );
        for s in farfield_core::registry::services() {
            let name = s.name.clone();
            let hint = session.client(&name).ok().and_then(|c| c.credential().map(|k| k.hint()));
            let needs = !matches!(name.as_str(), "daily" | "apex" | "keys");
            let (word, color): (&str, Hsla) = match health.get(&name) {
                Some(Health::Up) => ("online", t.good),
                Some(Health::NoAuth) => ("needs key", t.warn),
                Some(Health::Down(_)) => ("offline", t.bad),
                _ => ("unknown", t.ink_3),
            };
            rows = rows.child(
                div()
                    .flex()
                    .items_center()
                    .px(px(14.))
                    .py(px(8.))
                    .border_b_1()
                    .border_color(t.rule)
                    .child(div().w(px(130.)).flex_none().text_sm().text_color(t.ink).child(name.clone()))
                    .child(div().w(px(190.)).flex_none().text_xs().text_color(t.ink_2).child(accepts(&name)))
                    .child(
                        div()
                            .flex_1()
                            .min_w_0()
                            .font_family(FONT_MONO)
                            .text_xs()
                            .text_color(if hint.is_some() { t.ink } else { t.ink_3 })
                            .truncate()
                            .child(match (&hint, needs) {
                                (Some(h), _) if h == "…" => "stored".to_string(),
                                (Some(h), _) => h.clone(),
                                (None, true) => "no key".to_string(),
                                (None, false) => "—".to_string(),
                            }),
                    )
                    .child(div().w(px(110.)).flex_none().child(ui::chip(word, color, cx))),
            );
        }

        div().id("keys").size_full().overflow_y_scroll().child(
            div()
                .flex()
                .flex_col()
                .gap(S4)
                .px(px(40.))
                .py(S5)
                .max_w(px(920.))
                .child(div().text_xl().text_color(t.ink).child("Keys"))
                .child(
                    div()
                        .flex()
                        .gap(S2)
                        .child(ui::button("console", "Open console", BtnKind::Primary, cx, |_, _, cx| {
                            crate::ws::connections::open_console(cx, "keys")
                        }))
                        .child(ui::button("paste", "Paste a key…", BtnKind::Quiet, cx, |_, _, cx| {
                            goto(cx, "connections")
                        })),
                )
                .child(div().pt(S3).child(ui::eyebrow(format!("Keys in “{}”", session.profile.name), cx)))
                .child(rows),
        )
    }
}
