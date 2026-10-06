//! First run: where the fleet is, the keys to it, how it should look.
//!
//! Every step can be revisited from Settings, and nothing here is final:
//! the address and keys can be changed later, per service if need be.

use crate::app::{self, log, state, AppState};
use crate::shell::{self, Overlay};
use crate::theme::{theme, Mode, Theme, FONT_DOC, FONT_MONO, S2, S3, S4, S5, S6};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind};
use farfield_core::profile::{parse_tailscale_status, tailscale_status, Profile, TailnetPeer};
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

#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug)]
enum Step {
    Welcome,
    Fleet,
    Keys,
    Look,
    Done,
}

const STEPS: [Step; 5] = [Step::Welcome, Step::Fleet, Step::Keys, Step::Look, Step::Done];

#[derive(Clone, Copy, PartialEq)]
enum Where {
    Tailnet,
    ThisMac,
}

#[derive(Clone, PartialEq)]
enum Probe {
    Waiting,
    Ok,
    NeedsKey,
    Down(String),
}

pub struct Onboarding {
    focus: FocusHandle,
    step: Step,
    where_: Where,
    address: Entity<TextField>,
    address_error: Option<String>,
    tailscale: Option<Result<(bool, Vec<TailnetPeer>), String>>,
    reach: BTreeMap<String, Probe>,
    fleet_key: Entity<TextField>,
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

impl Onboarding {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let address =
            cx.new(|cx| TextField::new(w, cx, "Tailnet address of the homelab", "homelab.tail1234.ts.net").mono());
        cx.subscribe_in(&address, w, |this, _, e: &FieldEvent, _, cx| match e {
            FieldEvent::Changed => {
                this.address_error = None;
                this.reach.clear();
                cx.notify()
            }
            FieldEvent::Submit => this.check_fleet(cx),
            _ => {}
        })
        .detach();
        let fleet_key = cx.new(|cx| {
            TextField::new(w, cx, "A key for the whole fleet", "ffk_… minted for every app, write scope").secret()
        });
        cx.subscribe_in(&fleet_key, w, |this, f, e: &FieldEvent, _, cx| {
            if *e == FieldEvent::Submit {
                let v = f.read(cx).text();
                this.apply_key(None, &v, cx);
            }
        })
        .detach();
        // prefill from the current profile, if it is already a tailnet one
        let current = state(cx).session.profile.clone();
        if current.id != "local" {
            if let Some(h) = current.common_host() {
                address.update(cx, |f, cx| f.set_text(h, cx));
            }
        }
        let mut this = Onboarding {
            focus: cx.focus_handle(),
            step: Step::Welcome,
            where_: Where::Tailnet,
            address,
            address_error: None,
            tailscale: None,
            reach: BTreeMap::new(),
            fleet_key,
            keys: BTreeMap::new(),
            per_service: None,
        };
        this.detect(cx);
        this
    }

    fn detect(&mut self, cx: &mut Context<Self>) {
        let task = farfield_core::spawn(async { tailscale_status() });
        cx.spawn(async move |this, cx| {
            let out = task.await.ok().flatten();
            let _ = this.update(cx, |this, cx| {
                let r = match out {
                    None => Err("Tailscale isn't running on this Mac.".to_string()),
                    Some(j) => parse_tailscale_status(&j),
                };
                // suggest the homelab if nothing is typed yet
                if let Ok((_, peers)) = &r {
                    if this.address.read(cx).text().is_empty() {
                        if let Some(p) = peers.iter().find(|p| p.host_name == "homelab") {
                            let d = p.dns_name.clone();
                            this.address.update(cx, |f, cx| f.set_text(d, cx));
                        }
                    }
                }
                this.tailscale = Some(r);
                cx.notify();
            });
        })
        .detach();
    }

    fn profile(&self, cx: &App) -> Result<Profile, String> {
        match self.where_ {
            Where::ThisMac => Ok(Profile::local()),
            Where::Tailnet => {
                let a = self.address.read(cx).text();
                match farfield_core::profile::parse_fleet_address(&a) {
                    // a loopback address is this Mac's dev fleet
                    Ok((_, h)) if farfield_core::profile::is_loopback(&h) => Ok(Profile::local()),
                    _ => Profile::from_address("homelab", "Homelab (tailnet)", &a).map_err(|e| e.to_string()),
                }
            }
        }
    }

    /// Can each service be reached at this address? (/status, no key.)
    fn check_fleet(&mut self, cx: &mut Context<Self>) {
        let p = match self.profile(cx) {
            Ok(p) => p,
            Err(e) => {
                self.address_error = Some(e);
                cx.notify();
                return;
            }
        };
        let probe_session =
            Arc::new(Session::new(p, state(cx).data_dir.join("probe"), Arc::new(MemoryStore::default())));
        for s in registry::services() {
            let name = s.name.clone();
            self.reach.insert(name.clone(), Probe::Waiting);
            let ses = probe_session.clone();
            let n = name.clone();
            let task = farfield_core::spawn(async move { farfield_core::api::status(&ses, &n).await });
            cx.spawn(async move |this, cx| {
                let r = task.await;
                let _ = this.update(cx, |this, cx| {
                    this.reach.insert(
                        name,
                        match r {
                            Ok(Ok(_)) => Probe::Ok,
                            Ok(Err(e)) => Probe::Down(app::describe(&e)),
                            Err(e) => Probe::Down(e.to_string()),
                        },
                    );
                    cx.notify();
                });
            })
            .detach();
        }
        cx.notify();
    }

    fn use_fleet(&mut self, cx: &mut Context<Self>) {
        match self.profile(cx) {
            Ok(p) => {
                let id = p.id.clone();
                cx.global_mut::<AppState>().upsert_profile(p);
                cx.global_mut::<AppState>().activate(&id);
                log("onboarding-fleet", &[("profile", &id)]);
                self.step = Step::Keys;
                self.recheck_keys(cx);
            }
            Err(e) => self.address_error = Some(e),
        }
        cx.notify();
    }

    /// Store a key (for one service, or every keyed service) and test it.
    fn apply_key(&mut self, service: Option<String>, value: &str, cx: &mut Context<Self>) {
        let Some(c) = Credential::new(value) else {
            shell::toast(cx, "That key is empty or has spaces in it.", true);
            return;
        };
        let session = app::session(cx);
        let targets = service.map(|s| vec![s]).unwrap_or_else(keyed);
        for s in &targets {
            if let Err(e) = session.set_credential(s, &c) {
                shell::toast(cx, format!("Keychain refused the key: {e}"), true);
                return;
            }
        }
        log("onboarding-key", &[("services", &targets.join(",")), ("hint", &c.hint())]);
        self.per_service = None;
        self.fleet_key.update(cx, |f, cx| f.set_text("", cx));
        self.recheck_keys(cx);
    }

    fn recheck_keys(&mut self, cx: &mut Context<Self>) {
        let session = app::session(cx);
        for name in keyed() {
            self.keys.insert(name.clone(), Probe::Waiting);
            let s = session.clone();
            let n = name.clone();
            let task = farfield_core::spawn(async move { farfield_core::api::probe(&s, &n).await });
            cx.spawn(async move |this, cx| {
                let r = task.await;
                let _ = this.update(cx, |this, cx| {
                    let p = match r {
                        Ok(Ok(())) => Probe::Ok,
                        Ok(Err(e)) if e.is_auth() => Probe::NeedsKey,
                        Ok(Err(e)) => Probe::Down(app::describe(&e)),
                        Err(e) => Probe::Down(e.to_string()),
                    };
                    let h = match &p {
                        Probe::Ok => app::Health::Up,
                        Probe::NeedsKey => app::Health::NoAuth,
                        Probe::Down(e) => app::Health::Down(e.clone()),
                        Probe::Waiting => app::Health::Unknown,
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
        let f =
            cx.new(|cx| TextField::new(w, cx, format!("Key for {service}"), "paste it here, Enter to store").secret());
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
                self.fleet_key.read(cx).focus(w);
                self.recheck_keys(cx)
            }
            _ => w.focus(&self.focus),
        }
        log("onboarding-step", &[("step", &format!("{step:?}"))]);
        cx.notify();
    }

    fn next(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        match self.step {
            Step::Fleet => {
                if self.profile(cx).is_ok() {
                    self.use_fleet(cx);
                    if self.step == Step::Keys {
                        self.fleet_key.read(cx).focus(w);
                    }
                }
            }
            Step::Done => self.finish(w, cx),
            s => {
                let i = STEPS.iter().position(|x| *x == s).unwrap_or(0);
                self.go(STEPS[(i + 1).min(STEPS.len() - 1)], w, cx)
            }
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

    fn probe_dot(p: &Probe, t: &Theme) -> (Hsla, &'static str) {
        match p {
            Probe::Waiting => (t.ink_3, "checking"),
            Probe::Ok => (t.good, "ready"),
            Probe::NeedsKey => (t.warn, "needs a key"),
            Probe::Down(_) => (t.bad, "can't reach"),
        }
    }

    fn choice(
        &self,
        id: &'static str,
        title: &'static str,
        body: &'static str,
        on: bool,
        t: &Theme,
        cx: &mut Context<Self>,
        w: Where,
    ) -> AnyElement {
        let wash = t.wash;
        div()
            .id(id)
            .flex()
            .gap(S4)
            .py(S4)
            .px(S4)
            .border_b_1()
            .border_color(t.rule)
            .cursor_pointer()
            .when(on, |d| d.bg(t.accent_soft))
            .when(!on, move |d| d.hover(move |s| s.bg(wash)))
            .child(
                div()
                    .mt(px(3.))
                    .w(px(14.))
                    .h(px(14.))
                    .rounded_full()
                    .border_1()
                    .border_color(if on { t.accent } else { t.rule_strong })
                    .flex()
                    .items_center()
                    .justify_center()
                    .when(on, |d| d.child(div().w(px(6.)).h(px(6.)).rounded_full().bg(t.accent))),
            )
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(3.))
                    .child(div().text_base().text_color(t.ink).font_weight(gpui::FontWeight::MEDIUM).child(title))
                    .child(div().text_sm().text_color(t.ink_2).child(body)),
            )
            .on_click(cx.listener(move |this, _, _, cx| {
                this.where_ = w;
                this.reach.clear();
                this.address_error = None;
                cx.notify()
            }))
            .into_any_element()
    }

    fn service_grid(&self, probes: &BTreeMap<String, Probe>, t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        div()
            .flex()
            .flex_wrap()
            .gap_x(S5)
            .gap_y(S2)
            .children(probes.iter().map(|(name, p)| {
                let (c, word) = Self::probe_dot(p, t);
                let svc = name.clone();
                let can_edit = self.step == Step::Keys;
                div()
                    .id(SharedString::from(format!("svc-{name}")))
                    .w(px(176.))
                    .flex()
                    .items_center()
                    .gap(S2)
                    .child(div().flex_none().w(px(7.)).h(px(7.)).rounded_full().bg(c))
                    .child(div().text_sm().text_color(t.ink).child(name.clone()))
                    .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(word))
                    .when(can_edit, |d| {
                        d.cursor_pointer()
                            .on_click(cx.listener(move |this, _, w, cx| this.edit_one(svc.clone(), w, cx)))
                    })
            }))
            .into_any_element()
    }

    fn mode_swatch(&self, label: &'static str, m: Mode, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let on = state(cx).prefs.mode == m;
        let (bg, ink, acc, bg2) = match m {
            Mode::Light => {
                (Theme::new(false).paper, Theme::new(false).ink, Theme::new(false).accent, Theme::new(false).paper_2)
            }
            Mode::Dark => {
                (Theme::new(true).paper, Theme::new(true).ink, Theme::new(true).accent, Theme::new(true).paper_2)
            }
            Mode::System => {
                (Theme::new(false).paper, Theme::new(true).paper, Theme::new(true).accent, Theme::new(true).paper)
            }
        };
        div()
            .id(label)
            .flex()
            .flex_col()
            .gap(S2)
            .cursor_pointer()
            .child(
                div()
                    .w(px(150.))
                    .h(px(96.))
                    .rounded(px(6.))
                    .overflow_hidden()
                    .border_2()
                    .border_color(if on { t.accent } else { t.rule })
                    .flex()
                    .bg(bg)
                    .child(div().w(px(38.)).h_full().bg(bg2))
                    .child(
                        div()
                            .flex_1()
                            .p(px(10.))
                            .flex()
                            .flex_col()
                            .gap(px(6.))
                            .child(div().w(px(70.)).h(px(6.)).rounded(px(2.)).bg(ink))
                            .child(div().w(px(84.)).h(px(4.)).rounded(px(2.)).bg(ink).opacity(0.4))
                            .child(div().w(px(60.)).h(px(4.)).rounded(px(2.)).bg(ink).opacity(0.4))
                            .child(div().mt(px(8.)).w(px(36.)).h(px(12.)).rounded(px(3.)).bg(acc)),
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

    fn body(&mut self, t: &Theme, w: &mut Window, cx: &mut Context<Self>) -> (SharedString, SharedString, AnyElement) {
        let _ = w;
        match self.step {
            Step::Welcome => (
                "Farfield, on this Mac.".into(),
                "Write, publish and look after the whole fleet from one window. It talks to each service over your tailnet, so it keeps working when the public internet doesn't, and every edit is saved here first, so nothing is lost to a dropped connection.".into(),
                div()
                    .flex()
                    .flex_col()
                    .gap(S3)
                    .child(feature("Private by default", "API and media traffic stays on your tailnet. Public addresses are only for links you share.", t))
                    .child(feature("Drafts that survive", "Work is kept on this Mac until you choose to save it to the server, and conflicts never overwrite anyone.", t))
                    .child(feature("One place for the fleet", "Content, feed, media, bookmarks, the library, QR codes, pastes, builds, and the health of all of it.", t))
                    .into_any_element(),
            ),
            Step::Fleet => {
                let mut col = div()
                    .flex()
                    .flex_col()
                    .border_t_1()
                    .border_color(t.rule)
                    .child(self.choice("w-tail", "My homelab, over Tailscale", "Each service at its private HTTPS address on the tailnet.", self.where_ == Where::Tailnet, t, cx, Where::Tailnet))
                    .child(self.choice("w-mac", "This Mac", "The development fleet from `make dev`, on 127.0.0.1.", self.where_ == Where::ThisMac, t, cx, Where::ThisMac));
                if self.where_ == Where::Tailnet {
                    let ts = match &self.tailscale {
                        None => ui::chip("Looking for Tailscale…", t.ink_3, cx).into_any_element(),
                        Some(Err(e)) => ui::notice(format!("{e} Start the Tailscale app, or type the address anyway."), t.warn, cx).into_any_element(),
                        Some(Ok((running, peers))) => div()
                            .flex()
                            .flex_col()
                            .gap(S2)
                            .child(ui::chip(if *running { "Tailscale is connected" } else { "Tailscale is installed but not connected" }, if *running { t.good } else { t.warn }, cx))
                            .when(!peers.is_empty(), |d| {
                                d.child(div().text_xs().text_color(t.ink_3).child("Devices on your tailnet — choose the homelab:")).child(
                                    div().flex().flex_wrap().gap(S2).children(peers.iter().take(8).map(|p| {
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
                                            .child(div().w(px(6.)).h(px(6.)).rounded_full().bg(if p.online { t.good } else { t.ink_3 }))
                                            .child(div().text_sm().child(p.host_name.clone()))
                                            .on_click(cx.listener(move |this, _, _, cx| {
                                                let d = dns.clone();
                                                this.address.update(cx, |f, cx| f.set_text(d, cx));
                                                this.check_fleet(cx);
                                            }))
                                    })),
                                )
                            })
                            .into_any_element(),
                    };
                    col = col.child(
                        div()
                            .flex()
                            .flex_col()
                            .gap(S3)
                            .pt(S4)
                            .child(ts)
                            .child(div().flex().items_end().gap(S3).child(div().flex_1().child(self.address.clone())).child(ui::button("check", "Check", Kind::Quiet, cx, {
                                let e = cx.entity();
                                move |_, _, cx| e.update(cx, |this, cx| this.check_fleet(cx))
                            })))
                            .child(div().text_xs().text_color(t.ink_3).child("The name only — each service adds its own port (content :8787, feed :8788 …). You can change any of them later in Settings.")),
                    );
                }
                if let Some(e) = &self.address_error {
                    col = col.child(div().pt(S2).child(ui::notice(e.clone(), t.bad, cx)));
                }
                if !self.reach.is_empty() {
                    let ok = self.reach.values().filter(|p| **p == Probe::Ok).count();
                    col = col
                        .child(div().pt(S4).child(ui::eyebrow(format!("{ok} of {} services answered", self.reach.len()), cx)))
                        .child(div().pt(S2).child(self.service_grid(&self.reach.clone(), t, cx)));
                    if ok == 0 && !self.reach.values().any(|p| *p == Probe::Waiting) {
                        col = col.child(div().pt(S2).child(ui::notice(
                            "Nothing answered. If this is the homelab, its services need private addresses on the tailnet (tailscale serve) — see the README. You can continue and fix it later.",
                            t.warn,
                            cx,
                        )));
                    }
                }
                ("Where is your fleet?".into(), "Pick where the services live. API and media traffic only ever goes to these private addresses.".into(), col.into_any_element())
            }
            Step::Keys => {
                let mut col = div()
                    .flex()
                    .flex_col()
                    .gap(S4)
                    .child(div().flex().items_end().gap(S3).child(div().flex_1().child(self.fleet_key.clone())).child(ui::button("apply", "Use for every service", Kind::Quiet, cx, {
                        let e = cx.entity();
                        move |_, _, cx| {
                            e.update(cx, |this, cx| {
                                let v = this.fleet_key.read(cx).text();
                                this.apply_key(None, &v, cx)
                            })
                        }
                    })))
                    .child(div().text_xs().text_color(t.ink_3).child(
                        "Mint one in the keys console: app “*”, scope write. Keys are stored in your Keychain, bound to the address they were entered for, and never shown again. Click a service below to give it its own key.",
                    ))
                    .child(ui::button("console", "Open the keys console ↗", Kind::Quiet, cx, |_, _, cx| crate::ws::connections::open_console(cx, "keys")));
                if let Some((svc, f)) = self.per_service.clone() {
                    col = col.child(div().flex().flex_col().gap(px(4.)).child(f).child(div().text_xs().text_color(t.ink_3).child(format!("Enter stores it for {svc} only · Esc cancels"))));
                }
                let ready = self.keys.values().filter(|p| **p == Probe::Ok).count();
                col = col
                    .child(ui::rule(cx))
                    .child(ui::eyebrow(format!("{ready} of {} ready", self.keys.len()), cx))
                    .child(self.service_grid(&self.keys.clone(), t, cx));
                ("Keys to the fleet".into(), "One key minted for every app is simplest. Scoped keys per service work too — read keys see only what's public.".into(), col.into_any_element())
            }
            Step::Look => (
                "How it should look".into(),
                "Paper by day, Deep Space by night — or follow the system.".into(),
                div()
                    .flex()
                    .flex_col()
                    .gap(S5)
                    .child(div().flex().gap(S5).child(self.mode_swatch("System", Mode::System, cx)).child(self.mode_swatch("Light", Mode::Light, cx)).child(self.mode_swatch("Dark", Mode::Dark, cx)))
                    .child(
                        div()
                            .id("rm")
                            .flex()
                            .items_center()
                            .gap(S3)
                            .cursor_pointer()
                            .child(toggle(state(cx).prefs.reduced_motion, t))
                            .child(div().flex().flex_col().child(div().text_sm().child("Reduce motion")).child(div().text_xs().text_color(t.ink_3).child("No blinking caret, no animated transitions.")))
                            .on_click(cx.listener(|_, _, w, cx| {
                                let p = &mut cx.global_mut::<AppState>().prefs;
                                p.reduced_motion = !p.reduced_motion;
                                state(cx).save_prefs();
                                shell::apply_theme(w, cx);
                            })),
                    )
                    .into_any_element(),
            ),
            Step::Done => {
                let st = state(cx);
                let p = st.session.profile.clone();
                let ready = self.keys.values().filter(|p| **p == Probe::Ok).count();
                (
                    "Ready.".into(),
                    "Everything here can be changed in Settings (⌘,).".into(),
                    div()
                        .flex()
                        .flex_col()
                        .gap(S2)
                        .child(summary("Fleet", p.name.clone(), cx))
                        .child(summary("Address", p.common_host().unwrap_or_else(|| "per service".into()), cx))
                        .child(summary("Keys", format!("{ready} of {} services ready", keyed().len()), cx))
                        .child(summary("Look", format!("{:?}{}", st.prefs.mode, if st.prefs.reduced_motion { ", reduced motion" } else { "" }), cx))
                        .child(div().pt(S4).text_sm().text_color(t.ink_2).child("⌘K finds anything · ⌘1–9 switch areas · ⌘S saves to the server"))
                        .into_any_element(),
                )
            }
        }
    }
}

fn feature(title: &'static str, body: &'static str, t: &Theme) -> impl IntoElement {
    div()
        .flex()
        .gap(S4)
        .py(S3)
        .border_t_1()
        .border_color(t.rule)
        .child(div().flex_none().mt(px(7.)).w(px(7.)).h(px(7.)).rounded_full().bg(t.signal))
        .child(
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .flex_col()
                .gap(px(3.))
                .child(div().text_base().font_weight(gpui::FontWeight::MEDIUM).text_color(t.ink).child(title))
                .child(div().text_sm().text_color(t.ink_2).child(body)),
        )
}

fn summary(k: &'static str, v: String, cx: &App) -> impl IntoElement {
    let t = theme(cx);
    div()
        .flex()
        .gap(S4)
        .py(px(6.))
        .border_b_1()
        .border_color(t.rule)
        .child(div().w(px(90.)).text_sm().text_color(t.ink_2).child(k))
        .child(div().font_family(FONT_MONO).text_sm().text_color(t.ink).child(v))
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
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let idx = STEPS.iter().position(|s| *s == self.step).unwrap_or(0);
        let (headline, lede, body) = self.body(&t, w, cx);
        let can_next = match self.step {
            Step::Fleet => self.profile(cx).is_ok(),
            _ => true,
        };
        let next_label = match self.step {
            Step::Welcome => "Get started",
            Step::Fleet => "Use this fleet",
            Step::Keys => {
                if self.keys.values().all(|p| *p == Probe::Ok) {
                    "Continue"
                } else {
                    "Continue — add the rest later"
                }
            }
            Step::Look => "Continue",
            Step::Done => "Open Farfield",
        };
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
            // the horizon: a full-width rule whose lit segment is progress
            .child(div().h(px(48.)))
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
                        .w(px(640.))
                        .pt(px(56.))
                        .pb(S6)
                        .flex()
                        .flex_col()
                        .gap(S4)
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
                                        .child(div().w(px(10.)).h(px(10.)).rounded_full().bg(t.signal))
                                        .child(
                                            div().text_sm().font_weight(gpui::FontWeight::SEMIBOLD).child("farfield"),
                                        ),
                                )
                                .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(format!(
                                    "{:02} / {:02}",
                                    idx + 1,
                                    STEPS.len()
                                ))),
                        )
                        .child(
                            div()
                                .pt(S5)
                                .font_family(FONT_DOC)
                                .text_size(px(38.))
                                .line_height(px(46.))
                                .text_color(t.ink)
                                .child(headline),
                        )
                        .child(
                            div()
                                .text_size(px(15.))
                                .line_height(px(23.))
                                .text_color(t.ink_2)
                                .max_w(px(560.))
                                .child(lede),
                        )
                        .child(div().pt(S4).child(body)),
                ),
            )
            .child(
                div().border_t_1().border_color(t.rule).flex().justify_center().child(
                    div()
                        .w(px(640.))
                        .py(S4)
                        .flex()
                        .items_center()
                        .justify_between()
                        .child(if idx > 0 {
                            let e = ent.clone();
                            ui::button("back", "← Back", Kind::Quiet, cx, move |_, w, cx| {
                                e.update(cx, |this, cx| {
                                    let i = STEPS.iter().position(|s| *s == this.step).unwrap_or(0);
                                    this.go(STEPS[i.saturating_sub(1)], w, cx)
                                })
                            })
                            .into_any_element()
                        } else {
                            div().into_any_element()
                        })
                        .child(if can_next {
                            let e = ent.clone();
                            ui::button("next", format!("{next_label}  ⌘↩"), Kind::Primary, cx, move |_, w, cx| {
                                e.update(cx, |this, cx| this.next(w, cx))
                            })
                            .into_any_element()
                        } else {
                            ui::button_disabled(next_label, cx).into_any_element()
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
