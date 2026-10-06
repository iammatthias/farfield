//! First run: where the fleet is, the key to it, how it looks. Everything
//! here can be changed later in Settings.
//!
//! Tailscale is read only when asked to: the device list is the person's own
//! network, and nothing about it is shown (or assumed) until they opt in.

use crate::app::{self, log, state, AppState};
use crate::shell::{self, Overlay};
use crate::theme::{theme, Mode, Theme, FONT_DOC, FONT_MONO, S2, S3, S4, S5, S6};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind};
use farfield_core::profile::{
    is_loopback, parse_fleet_address, parse_tailscale_status, tailscale_status, Profile, TailnetPeer,
};
use farfield_core::registry;
use farfield_core::secret::{Credential, MemoryStore};
use farfield_core::Session;
use gpui::{div, prelude::*, px, AnyElement, App, Context, Entity, FocusHandle, Focusable, Hsla, SharedString, Window};
use std::collections::BTreeMap;
use std::sync::Arc;

gpui::actions!(onboarding, [Next, Back]);

pub fn bind_keys(cx: &mut App) {
    cx.bind_keys([
        gpui::KeyBinding::new("cmd-enter", Next, Some("Onboarding")),
        gpui::KeyBinding::new("cmd-[", Back, Some("Onboarding")),
    ]);
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Step {
    Fleet,
    Keys,
    Look,
}

const STEPS: [Step; 3] = [Step::Fleet, Step::Keys, Step::Look];

#[derive(Clone, PartialEq)]
enum Probe {
    Waiting,
    Ok,
    NeedsKey,
    Down,
}

pub struct Onboarding {
    focus: FocusHandle,
    step: Step,
    address: Entity<TextField>,
    address_error: Option<String>,
    /// Filled only after "Use Tailscale" is pressed.
    tailnet: Option<Result<Vec<TailnetPeer>, String>>,
    reach: BTreeMap<String, Probe>,
    key: Entity<TextField>,
    keys: BTreeMap<String, Probe>,
    per_service: Option<(String, Entity<TextField>)>,
}

/// Services the client needs a key for.
fn keyed() -> Vec<String> {
    registry::services()
        .iter()
        .filter(|s| farfield_core::api::probe_path(&s.name).is_some())
        .map(|s| s.name.clone())
        .collect()
}

/// A profile for a typed address: loopback is this Mac's dev fleet, anything
/// else is named after its first label.
pub fn profile_for(address: &str) -> Result<Profile, String> {
    let (_, host) = parse_fleet_address(address).map_err(|e| e.to_string())?;
    if is_loopback(&host) {
        return Ok(Profile::local());
    }
    let name = host.split('.').next().unwrap_or(&host).to_string();
    Profile::from_address("fleet", &name, address).map_err(|e| e.to_string())
}

impl Onboarding {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let address = cx.new(|cx| TextField::new(w, cx, "Address", "name.tailnet.ts.net").mono());
        cx.subscribe_in(&address, w, |this, _, e: &FieldEvent, _, cx| match e {
            FieldEvent::Changed => {
                this.address_error = None;
                this.reach.clear();
                cx.notify()
            }
            FieldEvent::Submit => this.check(cx),
            _ => {}
        })
        .detach();
        let key = cx.new(|cx| TextField::new(w, cx, "Or a key", "ffk_…").secret());
        cx.subscribe_in(&key, w, |this, f, e: &FieldEvent, _, cx| {
            if *e == FieldEvent::Submit {
                let v = f.read(cx).text();
                this.apply_key(None, &v, cx);
            }
        })
        .detach();
        // returning (Settings → Run setup again): start from what's in use
        let current = state(cx).session.profile.clone();
        if let Some(h) = current.common_host() {
            address.update(cx, |f, cx| f.set_text(h, cx));
        }
        address.read(cx).focus(w);
        Onboarding {
            focus: cx.focus_handle(),
            step: Step::Fleet,
            address,
            address_error: None,
            tailnet: None,
            reach: BTreeMap::new(),
            key,
            keys: BTreeMap::new(),
            per_service: None,
        }
    }

    /// Opt-in: read the device list from the OS Tailscale.
    fn use_tailscale(&mut self, cx: &mut Context<Self>) {
        let task = farfield_core::spawn(async { tailscale_status() });
        cx.spawn(async move |this, cx| {
            let out = task.await.ok().flatten();
            let _ = this.update(cx, |this, cx| {
                this.tailnet = Some(match out {
                    None => Err("Tailscale isn't running.".to_string()),
                    Some(j) => parse_tailscale_status(&j).map(|(_, peers)| peers),
                });
                cx.notify();
            });
        })
        .detach();
    }

    /// Ask each service's /status at this address (no key).
    fn check(&mut self, cx: &mut Context<Self>) {
        let p = match profile_for(&self.address.read(cx).text()) {
            Ok(p) => p,
            Err(e) => {
                self.address_error = Some(e);
                cx.notify();
                return;
            }
        };
        let probe = Arc::new(Session::new(p, state(cx).data_dir.join("probe"), Arc::new(MemoryStore::default())));
        for s in registry::services() {
            let name = s.name.clone();
            self.reach.insert(name.clone(), Probe::Waiting);
            let (ses, n) = (probe.clone(), name.clone());
            let task = farfield_core::spawn(async move { farfield_core::api::status(&ses, &n).await });
            cx.spawn(async move |this, cx| {
                let r = task.await;
                let _ = this.update(cx, |this, cx| {
                    this.reach.insert(name, if matches!(r, Ok(Ok(_))) { Probe::Ok } else { Probe::Down });
                    cx.notify();
                });
            })
            .detach();
        }
        cx.notify();
    }

    fn use_fleet(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        match profile_for(&self.address.read(cx).text()) {
            Ok(p) => {
                let id = p.id.clone();
                cx.global_mut::<AppState>().upsert_profile(p);
                cx.global_mut::<AppState>().activate(&id);
                log("onboarding-fleet", &[("profile", &id)]);
                self.go(Step::Keys, w, cx);
            }
            Err(e) => {
                self.address_error = Some(e);
                cx.notify();
            }
        }
    }

    /// Store a key for one service, or every service that takes one.
    fn apply_key(&mut self, service: Option<String>, value: &str, cx: &mut Context<Self>) {
        let Some(c) = Credential::new(value) else { return };
        let session = app::session(cx);
        let targets = service.map(|s| vec![s]).unwrap_or_else(keyed);
        for s in &targets {
            if let Err(e) = session.set_credential(s, &c) {
                shell::toast(cx, format!("Keychain: {e}"), true);
                return;
            }
        }
        log("onboarding-key", &[("services", &targets.join(",")), ("hint", &c.hint())]);
        self.per_service = None;
        self.key.update(cx, |f, cx| f.set_text("", cx));
        self.recheck_keys(cx);
    }

    fn recheck_keys(&mut self, cx: &mut Context<Self>) {
        let session = app::session(cx);
        for name in keyed() {
            self.keys.insert(name.clone(), Probe::Waiting);
            let (s, n) = (session.clone(), name.clone());
            let task = farfield_core::spawn(async move { farfield_core::api::probe(&s, &n).await });
            cx.spawn(async move |this, cx| {
                let r = task.await;
                let _ = this.update(cx, |this, cx| {
                    let (p, h) = match r {
                        Ok(Ok(())) => (Probe::Ok, app::Health::Up),
                        Ok(Err(e)) if e.is_auth() => (Probe::NeedsKey, app::Health::NoAuth),
                        Ok(Err(e)) => (Probe::Down, app::Health::Down(e.to_string())),
                        Err(e) => (Probe::Down, app::Health::Down(e.to_string())),
                    };
                    shell::set_health(cx, &name, h);
                    this.keys.insert(name, p);
                    cx.notify();
                });
            })
            .detach();
        }
        cx.notify();
    }

    fn edit_one(&mut self, service: String, w: &mut Window, cx: &mut Context<Self>) {
        let f = cx.new(|cx| TextField::new(w, cx, service.clone(), "key").secret());
        let s2 = service.clone();
        cx.subscribe_in(&f, w, move |this, f, e: &FieldEvent, _, cx| match e {
            FieldEvent::Submit => {
                let v = f.read(cx).text();
                this.apply_key(Some(s2.clone()), &v, cx)
            }
            FieldEvent::Cancel => {
                this.per_service = None;
                cx.notify()
            }
            _ => {}
        })
        .detach();
        f.read(cx).focus(w);
        self.per_service = Some((service, f));
        cx.notify();
    }

    fn go(&mut self, step: Step, w: &mut Window, cx: &mut Context<Self>) {
        self.step = step;
        match step {
            Step::Fleet => self.address.read(cx).focus(w),
            Step::Keys => {
                self.key.read(cx).focus(w);
                self.recheck_keys(cx)
            }
            Step::Look => w.focus(&self.focus),
        }
        log("onboarding-step", &[("step", &format!("{step:?}"))]);
        cx.notify();
    }

    fn next(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        match self.step {
            Step::Fleet => self.use_fleet(w, cx),
            Step::Keys => self.go(Step::Look, w, cx),
            Step::Look => self.finish(w, cx),
        }
    }

    fn back(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let i = STEPS.iter().position(|s| *s == self.step).unwrap_or(0);
        self.go(STEPS[i.saturating_sub(1)], w, cx)
    }

    fn finish(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let st = cx.global_mut::<AppState>();
        st.prefs.onboarded = true;
        st.save_prefs();
        cx.global_mut::<Overlay>().reset = true;
        log("onboarding-done", &[]);
        shell::apply_theme(w, cx);
    }

    // ── pieces ──

    fn dot(p: &Probe, t: &Theme) -> Hsla {
        match p {
            Probe::Waiting => t.ink_3,
            Probe::Ok => t.good,
            Probe::NeedsKey => t.warn,
            Probe::Down => t.bad,
        }
    }

    /// Services as a quiet grid of dots; on the keys step a name opens its own
    /// key field.
    fn grid(&self, probes: &BTreeMap<String, Probe>, editable: bool, t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        div()
            .flex()
            .flex_wrap()
            .gap_x(S5)
            .gap_y(S2)
            .children(probes.iter().map(|(name, p)| {
                let svc = name.clone();
                div()
                    .id(SharedString::from(format!("svc-{name}")))
                    .w(px(140.))
                    .flex()
                    .items_center()
                    .gap(S2)
                    .text_sm()
                    .text_color(t.ink)
                    .child(div().flex_none().w(px(7.)).h(px(7.)).rounded_full().bg(Self::dot(p, t)))
                    .child(name.clone())
                    .when(editable, |d| {
                        d.cursor_pointer()
                            .on_click(cx.listener(move |this, _, w, cx| this.edit_one(svc.clone(), w, cx)))
                    })
            }))
            .into_any_element()
    }

    fn mode_swatch(&self, label: &'static str, m: Mode, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let on = state(cx).prefs.mode == m;
        let (l, d) = (Theme::new(false), Theme::new(true));
        let (bg, ink, acc, side) = match m {
            Mode::Light => (l.paper, l.ink, l.accent, l.paper_2),
            Mode::Dark => (d.paper, d.ink, d.accent, d.paper_2),
            Mode::System => (l.paper, d.paper, d.accent, d.paper),
        };
        div()
            .id(label)
            .flex()
            .flex_col()
            .gap(S2)
            .cursor_pointer()
            .child(
                div()
                    .w(px(132.))
                    .h(px(84.))
                    .rounded(px(6.))
                    .overflow_hidden()
                    .border_2()
                    .border_color(if on { t.accent } else { t.rule })
                    .flex()
                    .bg(bg)
                    .child(div().w(px(32.)).h_full().bg(side))
                    .child(
                        div()
                            .flex_1()
                            .p(px(10.))
                            .flex()
                            .flex_col()
                            .gap(px(6.))
                            .child(div().w(px(60.)).h(px(6.)).rounded(px(2.)).bg(ink))
                            .child(div().w(px(72.)).h(px(4.)).rounded(px(2.)).bg(ink).opacity(0.4))
                            .child(div().mt(px(6.)).w(px(32.)).h(px(10.)).rounded(px(3.)).bg(acc)),
                    ),
            )
            .child(div().text_sm().text_color(if on { t.ink } else { t.ink_2 }).child(label))
            .on_click(cx.listener(move |_, _, w, cx| {
                cx.global_mut::<AppState>().prefs.mode = m;
                state(cx).save_prefs();
                shell::apply_theme(w, cx);
            }))
            .into_any_element()
    }

    fn body(&mut self, t: &Theme, cx: &mut Context<Self>) -> (&'static str, AnyElement) {
        match self.step {
            Step::Fleet => {
                let tailnet = match &self.tailnet {
                    None => ui::button("tailscale", "Use Tailscale", Kind::Quiet, cx, {
                        let e = cx.entity();
                        move |_, _, cx| e.update(cx, |this, cx| this.use_tailscale(cx))
                    })
                    .into_any_element(),
                    Some(Err(e)) => div().text_sm().text_color(t.ink_2).child(e.clone()).into_any_element(),
                    Some(Ok(peers)) => div()
                        .flex()
                        .flex_wrap()
                        .gap(S2)
                        .children(peers.iter().map(|p| {
                            let dns = p.dns_name.clone();
                            let chosen = self.address.read(cx).text() == p.dns_name;
                            div()
                                .id(SharedString::from(format!("peer-{}", p.dns_name)))
                                .px(S3)
                                .py(px(4.))
                                .rounded(px(4.))
                                .border_1()
                                .border_color(if chosen { t.accent } else { t.rule })
                                .when(chosen, |d| d.bg(t.accent_soft))
                                .cursor_pointer()
                                .flex()
                                .gap(px(6.))
                                .items_center()
                                .text_sm()
                                .child(div().flex_none().w(px(6.)).h(px(6.)).rounded_full().bg(if p.online {
                                    t.good
                                } else {
                                    t.ink_3
                                }))
                                .child(p.host_name.clone())
                                .on_click(cx.listener(move |this, _, _, cx| {
                                    let d = dns.clone();
                                    this.address.update(cx, |f, cx| f.set_text(d, cx));
                                    this.check(cx);
                                }))
                        }))
                        .into_any_element(),
                };
                let reached = self.reach.values().filter(|p| **p == Probe::Ok).count();
                let col = div()
                    .flex()
                    .flex_col()
                    .gap(S4)
                    .child(div().flex().items_end().gap(S3).child(div().flex_1().child(self.address.clone())).child(
                        ui::button("check", "Check", Kind::Quiet, cx, {
                            let e = cx.entity();
                            move |_, _, cx| e.update(cx, |this, cx| this.check(cx))
                        }),
                    ))
                    .when_some(self.address_error.clone(), |d, e| d.child(div().text_sm().text_color(t.bad).child(e)))
                    .child(tailnet)
                    .when(!self.reach.is_empty(), |d| {
                        d.child(div().pt(S2).child(ui::mono(format!("{reached} of {} answered", self.reach.len()), cx)))
                            .child(self.grid(&self.reach.clone(), false, t, cx))
                    });
                ("Where's the fleet?", col.into_any_element())
            }
            Step::Keys => {
                let ready = self.keys.values().filter(|p| **p == Probe::Ok).count();
                let col = div()
                    .flex()
                    .flex_col()
                    .gap(S4)
                    .child(div().flex().child(ui::button("passkey", "Sign in with passkey", Kind::Primary, cx, {
                        let e = cx.entity();
                        move |_, _, cx| {
                            let e = e.clone();
                            crate::signin::run(cx, move |_, cx| e.update(cx, |this, cx| this.recheck_keys(cx)))
                        }
                    })))
                    .child(div().flex().items_end().gap(S3).child(div().flex_1().child(self.key.clone())).child(
                        ui::button("apply", "Use for all", Kind::Quiet, cx, {
                            let e = cx.entity();
                            move |_, _, cx| {
                                e.update(cx, |this, cx| {
                                    let v = this.key.read(cx).text();
                                    this.apply_key(None, &v, cx)
                                })
                            }
                        }),
                    ))
                    .when_some(self.per_service.clone().map(|(_, f)| f), |d, f| d.child(f))
                    .child(ui::mono(format!("{ready} of {} ready", self.keys.len()), cx))
                    .child(self.grid(&self.keys.clone(), true, t, cx));
                ("Sign in", col.into_any_element())
            }
            Step::Look => {
                let rm = state(cx).prefs.reduced_motion;
                let col = div()
                    .flex()
                    .flex_col()
                    .gap(S5)
                    .child(
                        div()
                            .flex()
                            .gap(S5)
                            .child(self.mode_swatch("System", Mode::System, cx))
                            .child(self.mode_swatch("Light", Mode::Light, cx))
                            .child(self.mode_swatch("Dark", Mode::Dark, cx)),
                    )
                    .child(
                        div()
                            .id("rm")
                            .flex()
                            .items_center()
                            .gap(S3)
                            .cursor_pointer()
                            .text_sm()
                            .child(toggle(rm, t))
                            .child("Reduce motion")
                            .on_click(cx.listener(|_, _, w, cx| {
                                let p = &mut cx.global_mut::<AppState>().prefs;
                                p.reduced_motion = !p.reduced_motion;
                                state(cx).save_prefs();
                                shell::apply_theme(w, cx);
                            })),
                    );
                ("Look", col.into_any_element())
            }
        }
    }
}

pub fn toggle(on: bool, t: &Theme) -> impl IntoElement {
    div()
        .w(px(30.))
        .h(px(18.))
        .rounded_full()
        .p(px(2.))
        .bg(if on { t.accent } else { t.rule_strong })
        .flex()
        .when(on, |d| d.justify_end())
        .child(div().w(px(14.)).h(px(14.)).rounded_full().bg(t.paper))
}

impl Render for Onboarding {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let idx = STEPS.iter().position(|s| *s == self.step).unwrap_or(0);
        let (headline, body) = self.body(&t, cx);
        let next_label = if self.step == Step::Look { "Done" } else { "Next" };
        let ent = cx.entity();
        div()
            .id("onboarding")
            .key_context("Onboarding")
            .track_focus(&self.focus)
            .on_action(cx.listener(|this, _: &Next, w, cx| this.next(w, cx)))
            .on_action(cx.listener(|this, _: &Back, w, cx| this.back(w, cx)))
            .size_full()
            .flex()
            .flex_col()
            .bg(t.paper)
            .text_color(t.ink)
            .font_family(crate::theme::FONT_UI)
            .child(div().h(px(48.)))
            // the horizon: its lit segment is progress
            .child(
                div()
                    .w_full()
                    .h(px(2.))
                    .bg(t.rule)
                    .child(div().h_full().w(gpui::relative((idx as f32 + 1.0) / STEPS.len() as f32)).bg(t.signal)),
            )
            .child(
                div().id("onboarding-scroll").flex_1().overflow_y_scroll().flex().justify_center().child(
                    div()
                        .w(px(560.))
                        .pt(px(72.))
                        .pb(S6)
                        .flex()
                        .flex_col()
                        .gap(S5)
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .justify_between()
                                .child(
                                    div()
                                        .flex()
                                        .items_center()
                                        .gap(px(8.))
                                        .child(div().w(px(9.)).h(px(9.)).rounded_full().bg(t.signal))
                                        .child(
                                            div().text_sm().font_weight(gpui::FontWeight::SEMIBOLD).child("farfield"),
                                        ),
                                )
                                .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(format!(
                                    "{} / {}",
                                    idx + 1,
                                    STEPS.len()
                                ))),
                        )
                        .child(
                            div().pt(S4).font_family(FONT_DOC).text_size(px(34.)).line_height(px(42.)).child(headline),
                        )
                        .child(body),
                ),
            )
            .child(
                div().border_t_1().border_color(t.rule).flex().justify_center().child(
                    div()
                        .w(px(560.))
                        .py(S4)
                        .flex()
                        .items_center()
                        .justify_between()
                        .child(if idx > 0 {
                            let e = ent.clone();
                            ui::button("back", "Back", Kind::Quiet, cx, move |_, w, cx| {
                                e.update(cx, |this, cx| this.back(w, cx))
                            })
                            .into_any_element()
                        } else {
                            div().into_any_element()
                        })
                        .child({
                            let e = ent.clone();
                            ui::button("next", format!("{next_label}  ⌘↩"), Kind::Primary, cx, move |_, w, cx| {
                                e.update(cx, |this, cx| this.next(w, cx))
                            })
                        }),
                ),
            )
    }
}

impl Focusable for Onboarding {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}

#[cfg(test)]
mod tests {
    use super::profile_for;

    #[test]
    fn addresses_make_profiles_without_assumptions() {
        assert_eq!(profile_for("127.0.0.1").unwrap().id, "local");
        let p = profile_for("box.tail1234.ts.net").unwrap();
        assert_eq!(p.name, "box");
        assert_eq!(p.endpoint("content").unwrap().api, "https://box.tail1234.ts.net:8787");
        assert!(profile_for("http://box").is_err());
    }
}
