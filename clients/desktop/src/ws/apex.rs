//! Apex: the fleet at a glance — every service's health (the shell's own
//! checks, AppState.health), its private endpoint and public address — and
//! the public profile document apex renders for the GitHub README, read in
//! document type. ⌘R re-checks every service now.

use crate::app::{self, describe, state, Health};
use crate::shell::set_health;
use crate::theme::{theme, FONT_DOC, FONT_MONO, MEASURE, S1, S2, S3, S4, S5};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::Workspace;
use farfield_core::api::ext_observe::apex::{self, Profile};
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{div, prelude::*, px, AnyElement, App, Context, FontWeight, Hsla, SharedString, Window};
use std::sync::Arc;

pub struct ApexWs {
    profile: Option<Profile>,
    freshness: Option<Freshness>,
    error: Option<ApiError>,
    latest: Arc<Latest>,
    checking: bool,
}

/// Lightly render GitHub-flavoured markdown as text: headings, bullets,
/// links reduced to their words, emphasis markers dropped.
fn md_line(s: &str) -> String {
    let mut out = String::new();
    let mut rest = s;
    while let Some(i) = rest.find('[') {
        let tail = &rest[i..];
        let image = rest[..i].ends_with('!') && tail.contains("](");
        out.push_str(if image { &rest[..i - 1] } else { &rest[..i] });
        match (tail.find("]("), tail.find(')')) {
            (Some(a), Some(b)) if a < b => {
                // ![alt](src) images keep their alt text
                let text = &tail[1..a];
                out.push_str(text);
                rest = &tail[b + 1..];
            }
            _ => {
                out.push('[');
                rest = &tail[1..];
            }
        }
    }
    out.push_str(rest);
    out.replace("**", "").replace("__", "").replace('`', "")
}

impl ApexWs {
    pub fn new(_w: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut this = ApexWs {
            profile: None,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            checking: false,
        };
        this.load(cx);
        this
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        let task = farfield_core::spawn(async move { apex::profile(&s).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                match r {
                    Ok(Ok(p)) => {
                        this.profile = Some(p.value);
                        this.freshness = Some(p.freshness);
                        this.error = None;
                    }
                    Ok(Err(e)) => this.error = Some(e),
                    Err(e) => this.error = Some(ApiError::Decode(e.to_string())),
                }
                cx.notify();
            });
        })
        .detach();
    }

    /// Probe every service now rather than waiting for the shell's next pass.
    fn check_all(&mut self, cx: &mut Context<Self>) {
        self.checking = true;
        cx.notify();
        let session = app::session(cx);
        let names: Vec<String> = farfield_core::registry::services().iter().map(|s| s.name.clone()).collect();
        let task = farfield_core::spawn(async move {
            let mut out = Vec::new();
            for n in names {
                let r = farfield_core::api::status(&session, &n).await;
                out.push((n.clone(), r.map(|_| session.has_credential(&n))));
            }
            out
        });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                this.checking = false;
                if let Ok(list) = r {
                    for (n, res) in list {
                        let h = match res {
                            Ok(has) if has || matches!(n.as_str(), "daily" | "apex") => Health::Up,
                            // keep a refusal a workspace already saw; status can't see keys
                            Ok(_) => Health::NoAuth,
                            Err(e) => Health::Down(e.to_string()),
                        };
                        let keep = matches!((state(cx).health.get(&n), &h), (Some(Health::NoAuth), Health::Up));
                        if !keep {
                            set_health(cx, &n, h);
                        }
                    }
                }
                app::log("apex-check", &[]);
                cx.notify();
            });
        })
        .detach();
    }

    fn render_profile(&self, cx: &App) -> AnyElement {
        let t = theme(cx).clone();
        let Some(p) = &self.profile else {
            return match &self.error {
                Some(e) => {
                    ui::quiet_state(format!("The profile document didn't load: {}", describe(e)), cx).into_any_element()
                }
                None => ui::quiet_state("Loading the profile document…", cx).into_any_element(),
            };
        };
        if p.sections.is_empty() {
            return ui::quiet_state(
                "No sections right now. Apex leaves out any section whose source (feed, content, daily) didn't answer, so a README keeps its last good copy.",
                cx,
            )
            .into_any_element();
        }
        let mut col = div().flex().flex_col().gap(S5).max_w(MEASURE);
        for key in ["feed", "writing", "daily"]
            .into_iter()
            .chain(p.sections.keys().map(|k| k.as_str()).filter(|k| !matches!(*k, "feed" | "writing" | "daily")))
        {
            let Some(body) = p.sections.get(key) else { continue };
            let mut sec = div().flex().flex_col().gap(S2).child(ui::eyebrow(key.to_string(), cx));
            for line in body.lines() {
                let l = line.trim_end();
                if l.trim().is_empty() {
                    continue;
                }
                let el = if let Some(h) = l.trim_start().strip_prefix('#') {
                    div()
                        .font_family(FONT_DOC)
                        .text_size(px(20.))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(t.ink)
                        .child(md_line(h.trim_start_matches('#').trim()))
                } else if let Some(b) = l.trim_start().strip_prefix("- ").or_else(|| l.trim_start().strip_prefix("* "))
                {
                    div()
                        .flex()
                        .gap(S2)
                        .font_family(FONT_DOC)
                        .text_size(px(17.))
                        .line_height(px(26.))
                        .text_color(t.ink)
                        .child(div().text_color(t.ink_3).child("—"))
                        .child(div().flex_1().child(md_line(b)))
                } else if let Some(q) = l.trim_start().strip_prefix('>') {
                    div()
                        .border_l_2()
                        .border_color(t.rule_strong)
                        .pl(S3)
                        .font_family(FONT_DOC)
                        .text_size(px(17.))
                        .line_height(px(26.))
                        .text_color(t.ink_2)
                        .child(md_line(q.trim()))
                } else {
                    div()
                        .font_family(FONT_DOC)
                        .text_size(px(17.))
                        .line_height(px(26.))
                        .text_color(t.ink)
                        .child(md_line(l))
                };
                sec = sec.child(el);
            }
            col = col.child(sec);
        }
        col.into_any_element()
    }
}

impl Workspace for ApexWs {
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.check_all(cx);
        self.load(cx);
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        vec![
            ("check", "Apex: check every service now".into(), "⌘R"),
            ("site", "Apex: open farfield.systems".into(), ""),
        ]
    }

    fn run_command(&mut self, id: &str, _w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "check" => self.check_all(cx),
            "site" => {
                if let Some(u) = app::session(cx)
                    .public_base("apex")
                    .or_else(|| farfield_core::registry::lookup("apex").and_then(|s| s.public_url()))
                {
                    cx.open_url(&u)
                }
            }
            _ => {}
        }
    }
}

impl Render for ApexWs {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let st = state(cx);
        let health = st.health.clone();
        let profile = st.session.profile.clone();
        let services = farfield_core::registry::services();
        let up = services.iter().filter(|s| matches!(health.get(&s.name), Some(Health::Up))).count();
        let down: Vec<&str> = services
            .iter()
            .filter(|s| matches!(health.get(&s.name), Some(Health::Down(_))))
            .map(|s| s.name.as_str())
            .collect();
        let nokey = services.iter().filter(|s| matches!(health.get(&s.name), Some(Health::NoAuth))).count();

        let cell = |name: &str,
                    h: Option<&Health>,
                    api: String,
                    public: Option<String>,
                    host: bool,
                    cx: &mut Context<Self>| {
            let (word, color): (String, Hsla) = match h {
                Some(Health::Up) => ("online".into(), t.good),
                Some(Health::NoAuth) => ("online · needs key".into(), t.warn),
                Some(Health::Down(_)) => ("offline".into(), t.bad),
                _ => ("unknown".into(), t.ink_3),
            };
            let err = match h {
                Some(Health::Down(e)) => Some(e.clone()),
                _ => None,
            };
            let ws = name.to_string();
            let wash = t.wash;
            div()
                .id(SharedString::from(format!("svc-{name}")))
                .w(px(272.))
                .flex()
                .flex_col()
                .gap(px(3.))
                .pt(S3)
                .pb(S3)
                .px(S2)
                .border_t_1()
                .border_color(t.rule)
                .cursor_pointer()
                .hover(move |s| s.bg(wash))
                .on_click(cx.listener(move |_, _, _, cx| crate::shell::goto(cx, &ws)))
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap(S2)
                        .child(div().w(px(8.)).h(px(8.)).rounded_full().bg(color))
                        .child(
                            div().text_base().text_color(t.ink).font_weight(FontWeight::MEDIUM).child(name.to_string()),
                        )
                        .when(host, |d| d.child(div().text_xs().text_color(t.ink_3).child("host unit"))),
                )
                .child(div().pl(px(16.)).text_xs().text_color(t.ink_2).child(word))
                .child(div().pl(px(16.)).font_family(FONT_MONO).text_xs().text_color(t.ink_2).truncate().child(api))
                .child(
                    div()
                        .pl(px(16.))
                        .font_family(FONT_MONO)
                        .text_xs()
                        .text_color(t.ink_3)
                        .truncate()
                        .child(public.unwrap_or_else(|| "tailnet only".into())),
                )
                .when_some(err, |d, e| {
                    d.child(div().pl(px(16.)).font_family(FONT_MONO).text_xs().text_color(t.bad).truncate().child(e))
                })
        };
        let cells: Vec<AnyElement> = services
            .iter()
            .map(|s| {
                let ep = profile.endpoint(&s.name);
                let api = ep.map(|e| e.api.clone()).unwrap_or_else(|| "no endpoint in this profile".into());
                let public = ep.and_then(|e| e.public.clone()).or_else(|| s.public_url());
                cell(&s.name, health.get(&s.name), api, public, s.host, cx).into_any_element()
            })
            .collect();

        let headline = if down.is_empty() {
            format!("{up} of {} services online.", services.len())
        } else {
            format!("{} offline: {}.", down.len(), down.join(", "))
        };
        let e = cx.entity();
        div().id("apex").size_full().overflow_y_scroll().child(
            div()
                .flex()
                .flex_col()
                .gap(S4)
                .px(px(40.))
                .py(S5)
                .child(
                    div()
                        .flex()
                        .items_end()
                        .gap(S4)
                        .child(
                            div()
                                .flex()
                                .flex_col()
                                .gap(S1)
                                .child(div().text_xl().text_color(t.ink).child(headline))
                                .child(ui::mono(
                                    format!("{} · {}{}", profile.name, if nokey > 0 { format!("{nokey} need a key") } else { "every key accepted".into() }, if self.checking { " · checking…" } else { "" }),
                                    cx,
                                )),
                        )
                        .child(div().flex_1())
                        .child(ui::button("check", "Check now  ⌘R", BtnKind::Quiet, cx, move |_, _, cx| e.update(cx, |this, cx| this.check_all(cx)))),
                )
                .child(div().flex().flex_wrap().gap_x(S4).children(cells))
                .child(div().pt(S4).child(ui::rule(cx)))
                .child(
                    div()
                        .flex()
                        .items_baseline()
                        .gap(S3)
                        .child(div().text_lg().text_color(t.ink).child("Public profile"))
                        .when_some(self.profile.as_ref().map(|p| p.updated_at.clone()).filter(|u| !u.is_empty()), |d, u| d.child(ui::mono(format!("updated {}", ui::when(&u)), cx))),
                )
                .child(div().text_sm().text_color(t.ink_2).max_w(px(640.)).child(
                    "What apex publishes at /api/profile for the self-updating GitHub README: the latest feed post, the newest writing, today's art. Public material only.",
                ))
                .when(matches!(self.freshness, Some(Freshness::Stale { .. })), |d| d.child(ui::notice("Offline — this is the last copy fetched.", t.warn, cx)))
                .child(div().pt(S2).pb(px(48.)).child(self.render_profile(cx))),
        )
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn markdown_reads_as_text() {
        assert_eq!(super::md_line("Read [the post](https://x.y/p) **now**"), "Read the post now");
        assert_eq!(super::md_line("![a plate](https://x/y.svg)"), "a plate");
        assert_eq!(super::md_line("a [bracket without link"), "a [bracket without link");
    }
}
