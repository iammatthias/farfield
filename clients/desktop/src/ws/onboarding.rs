//! First run: a welcome, then where the fleet is, the key to it, how it looks. Everything
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
    is_loopback, parse_fleet_address, parse_tailnet, tailscale_status, Profile, TailnetPeer, TailnetStatus,
};
use farfield_core::registry;
use farfield_core::secret::{Credential, MemoryStore};
use farfield_core::Session;
use gpui::{div, prelude::*, px, AnyElement, App, Context, Entity, FocusHandle, Focusable, Hsla, SharedString, Window};
use std::collections::BTreeMap;
use std::sync::Arc;

gpui::actions!(onboarding, [Next, Back, ByAddress, ByTailscale]);

pub fn bind_keys(cx: &mut App) {
    cx.bind_keys([
        gpui::KeyBinding::new("cmd-enter", Next, Some("Onboarding")),
        gpui::KeyBinding::new("cmd-[", Back, Some("Onboarding")),
        gpui::KeyBinding::new("cmd-1", ByAddress, Some("Onboarding")),
        gpui::KeyBinding::new("cmd-2", ByTailscale, Some("Onboarding")),
    ]);
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Step {
    Welcome,
    Fleet,
    Keys,
    Look,
}

const STEPS: [Step; 4] = [Step::Welcome, Step::Fleet, Step::Keys, Step::Look];

/// How the fleet is found.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Find {
    Address,
    Tailscale,
}

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
    find: Option<Find>,
    /// Read only once Tailscale is chosen; `None` while reading.
    tailnet: Option<Result<TailnetStatus, String>>,
    /// The device picked from the tailnet, which names the profile.
    device: Option<(String, String)>,
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

/// A profile for an address: loopback is this Mac's dev fleet; anything else
/// is named by `name`, else an IP as itself, else a host's first label.
pub fn profile_for(address: &str, name: Option<&str>) -> Result<Profile, String> {
    let (_, host) = parse_fleet_address(address).map_err(|e| e.to_string())?;
    if is_loopback(&host) {
        return Ok(Profile::local());
    }
    let bare = host.trim_start_matches('[').trim_end_matches(']');
    let name = match name {
        Some(n) => n.to_string(),
        None if bare.parse::<std::net::IpAddr>().is_ok() => bare.to_string(),
        None => host.split('.').next().unwrap_or(&host).to_string(),
    };
    Profile::from_address("fleet", &name, address).map_err(|e| e.to_string())
}

impl Onboarding {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let address = cx.new(|cx| TextField::new(w, cx, "Address", "host, URL, or IP").mono());
        cx.subscribe_in(&address, w, |this, f, e: &FieldEvent, _, cx| match e {
            FieldEvent::Changed => {
                this.address_error = None;
                let text = f.read(cx).text();
                if this.device.as_ref().is_some_and(|(_, a)| *a != text) {
                    this.device = None;
                }
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
        // only a finished setup has an address worth keeping
        let returning = current.common_host().filter(|_| state(cx).prefs.onboarded);
        if let Some(h) = &returning {
            address.update(cx, |f, cx| f.set_text(h.clone(), cx));
        }
        let focus = cx.focus_handle();
        w.focus(&focus);
        Onboarding {
            focus,
            step: Step::Welcome,
            address,
            address_error: None,
            find: returning.map(|_| Find::Address),
            tailnet: None,
            device: None,
            reach: BTreeMap::new(),
            key,
            keys: BTreeMap::new(),
            per_service: None,
        }
    }

    fn choose(&mut self, find: Find, w: &mut Window, cx: &mut Context<Self>) {
        self.find = Some(find);
        self.address_error = None;
        match find {
            Find::Address => self.address.read(cx).focus(w),
            Find::Tailscale => {
                w.focus(&self.focus);
                self.read_tailnet(cx)
            }
        }
        cx.notify();
    }

    /// Opt-in: read the tailnet and its devices from the OS Tailscale, live.
    fn read_tailnet(&mut self, cx: &mut Context<Self>) {
        self.tailnet = None;
        let task = farfield_core::spawn(async { tailscale_status() });
        cx.spawn(async move |this, cx| {
            let out = task.await.ok().flatten();
            let _ = this.update(cx, |this, cx| {
                this.tailnet = Some(match out {
                    None => Err("Tailscale isn't installed.".to_string()),
                    Some(j) => parse_tailnet(&j).and_then(|s| {
                        if s.running {
                            Ok(s)
                        } else {
                            Err("Tailscale isn't connected.".to_string())
                        }
                    }),
                });
                log("onboarding-tailnet", &[("ok", &this.tailnet.as_ref().is_some_and(|r| r.is_ok()).to_string())]);
                cx.notify();
            });
        })
        .detach();
    }

    fn pick_device(&mut self, p: &TailnetPeer, cx: &mut Context<Self>) {
        let addr = if p.dns_name.is_empty() { p.ips.first().cloned().unwrap_or_default() } else { p.dns_name.clone() };
        self.address.update(cx, |f, cx| f.set_text(addr.clone(), cx));
        self.device = Some((p.host_name.clone(), addr));
        self.check(cx);
    }

    /// Ask each service's /status at this address (no key).
    fn check(&mut self, cx: &mut Context<Self>) {
        let p = match profile_for(&self.address.read(cx).text(), self.device.as_ref().map(|(n, _)| n.as_str())) {
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
        if self.find.is_none() {
            self.address_error = Some("Choose one.".into());
            return cx.notify();
        }
        match profile_for(&self.address.read(cx).text(), self.device.as_ref().map(|(n, _)| n.as_str())) {
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
            Step::Welcome => w.focus(&self.focus),
            Step::Fleet if self.find == Some(Find::Address) => self.address.read(cx).focus(w),
            Step::Fleet => w.focus(&self.focus),
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
            Step::Welcome => self.go(Step::Fleet, w, cx),
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

    /// One way of finding the fleet.
    fn tile(
        &self,
        find: Find,
        title: &'static str,
        sub: &'static str,
        t: &Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let on = self.find == Some(find);
        div()
            .id(SharedString::from(format!("find-{find:?}")))
            .flex_1()
            .px(S4)
            .py(S3)
            .rounded(px(6.))
            .border_1()
            .border_color(if on { t.accent } else { t.rule })
            .when(on, |d| d.bg(t.accent_soft))
            .cursor_pointer()
            .flex()
            .flex_col()
            .gap(px(2.))
            .child(
                div()
                    .flex()
                    .justify_between()
                    .child(div().text_sm().font_weight(gpui::FontWeight::SEMIBOLD).child(title))
                    .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(match find {
                        Find::Address => "⌘1",
                        Find::Tailscale => "⌘2",
                    })),
            )
            .child(div().text_xs().text_color(t.ink_2).child(sub))
            .on_click(cx.listener(move |this, _, w, cx| this.choose(find, w, cx)))
            .into_any_element()
    }

    /// The tailnet as Tailscale reports it right now: its name, then every
    /// device. Nothing is preselected.
    fn tailnet_list(&self, t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let s = match &self.tailnet {
            None => return ui::mono("Reading Tailscale…", cx).into_any_element(),
            Some(Err(e)) => {
                return div()
                    .flex()
                    .items_center()
                    .gap(S3)
                    .child(div().text_sm().text_color(t.ink_2).child(e.clone()))
                    .child(ui::button("retry", "Try again", Kind::Quiet, cx, {
                        let e = cx.entity();
                        move |_, _, cx| e.update(cx, |this, cx| this.read_tailnet(cx))
                    }))
                    .into_any_element()
            }
            Some(Ok(s)) => s.clone(),
        };
        let chosen = self.device.as_ref().map(|(n, _)| n.clone());
        let this_mac = s.this_device.clone().map(|mut d| {
            d.host_name = format!("{} (this Mac)", d.host_name);
            d
        });
        let rows = this_mac.into_iter().chain(s.peers.iter().cloned()).map(|p| {
            let on = chosen.as_deref() == Some(p.host_name.as_str());
            let addr =
                if p.dns_name.is_empty() { p.ips.first().cloned().unwrap_or_default() } else { p.dns_name.clone() };
            let pick = p.clone();
            ui::list_row(SharedString::from(format!("dev-{}", p.host_name)), on, t)
                .flex()
                .items_center()
                .gap(S3)
                .child(div().flex_none().w(px(7.)).h(px(7.)).rounded_full().bg(if p.online { t.good } else { t.ink_3 }))
                .child(div().text_sm().w(px(180.)).truncate().child(p.host_name.clone()))
                .child(div().flex_1().truncate().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(addr))
                .on_click(cx.listener(move |this, _, _, cx| this.pick_device(&pick, cx)))
        });
        div()
            .flex()
            .flex_col()
            .gap(S2)
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .child(ui::mono(s.tailnet.clone().unwrap_or_else(|| "tailnet".into()), cx))
                    .child(ui::button("refresh", "Refresh", Kind::Quiet, cx, {
                        let e = cx.entity();
                        move |_, _, cx| e.update(cx, |this, cx| this.read_tailnet(cx))
                    })),
            )
            .child(div().flex().flex_col().border_t_1().border_color(t.rule).children(rows))
            .into_any_element()
    }

    fn body(&mut self, t: &Theme, cx: &mut Context<Self>) -> (&'static str, AnyElement) {
        match self.step {
            Step::Welcome => ("", div().into_any_element()),
            Step::Fleet => {
                let tiles = div()
                    .flex()
                    .gap(S3)
                    .child(self.tile(Find::Address, "Address", "host, URL, or IP", t, cx))
                    .child(self.tile(Find::Tailscale, "Tailscale", "find it on your tailnet", t, cx));
                let found: AnyElement = match self.find {
                    None => div().into_any_element(),
                    Some(Find::Address) => div()
                        .flex()
                        .items_end()
                        .gap(S3)
                        .child(div().flex_1().child(self.address.clone()))
                        .child(ui::button("check", "Check", Kind::Quiet, cx, {
                            let e = cx.entity();
                            move |_, _, cx| e.update(cx, |this, cx| this.check(cx))
                        }))
                        .into_any_element(),
                    Some(Find::Tailscale) => self.tailnet_list(t, cx),
                };
                let reached = self.reach.values().filter(|p| **p == Probe::Ok).count();
                let col = div()
                    .flex()
                    .flex_col()
                    .gap(S4)
                    .child(tiles)
                    .child(found)
                    .when_some(self.address_error.clone(), |d, e| d.child(div().text_sm().text_color(t.bad).child(e)))
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
                    .child(crate::signin::view("passkey", cx, {
                        let e = cx.entity();
                        move |cx| {
                            let e = e.clone();
                            crate::signin::run(cx, move |_, cx| e.update(cx, |this, cx| this.recheck_keys(cx)))
                        }
                    }))
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

impl Onboarding {
    /// The splash: a large field, a low horizon, one point of light on it.
    fn welcome(&self, t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let e = cx.entity();
        div()
            .id("onboarding")
            .key_context("Onboarding")
            .track_focus(&self.focus)
            .on_action(cx.listener(|this, _: &Next, w, cx| this.next(w, cx)))
            .size_full()
            .flex()
            .flex_col()
            .bg(t.paper)
            .text_color(t.ink)
            .font_family(crate::theme::FONT_UI)
            .child(
                div()
                    .flex_1()
                    .flex()
                    .flex_col()
                    .justify_end()
                    .px(px(72.))
                    .pb(S6)
                    .child(div().font_family(FONT_DOC).text_size(px(64.)).line_height(px(72.)).child("Farfield")),
            )
            .child(
                div().relative().w_full().h(px(1.)).bg(t.rule_strong).child(
                    div()
                        .absolute()
                        .left(gpui::relative(0.68))
                        .top(px(-4.))
                        .w(px(9.))
                        .h(px(9.))
                        .rounded_full()
                        .bg(t.signal),
                ),
            )
            .child(div().h(gpui::relative(0.38)).px(px(72.)).pt(S6).flex().items_start().child(ui::button(
                "begin",
                "Begin  ⌘↩",
                Kind::Primary,
                cx,
                move |_, w, cx| e.update(cx, |this, cx| this.next(w, cx)),
            )))
            .into_any_element()
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
        if self.step == Step::Welcome {
            return self.welcome(&t, cx);
        }
        // the welcome isn't counted
        let idx = STEPS.iter().position(|s| *s == self.step).unwrap_or(1) - 1;
        let steps = STEPS.len() - 1;
        let (headline, body) = self.body(&t, cx);
        let next_label = if self.step == Step::Look { "Done" } else { "Next" };
        let ent = cx.entity();
        div()
            .id("onboarding")
            .key_context("Onboarding")
            .track_focus(&self.focus)
            .on_action(cx.listener(|this, _: &Next, w, cx| this.next(w, cx)))
            .on_action(cx.listener(|this, _: &Back, w, cx| this.back(w, cx)))
            .on_action(cx.listener(|this, _: &ByAddress, w, cx| {
                if this.step == Step::Fleet {
                    this.choose(Find::Address, w, cx)
                }
            }))
            .on_action(cx.listener(|this, _: &ByTailscale, w, cx| {
                if this.step == Step::Fleet {
                    this.choose(Find::Tailscale, w, cx)
                }
            }))
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
                    .child(div().h_full().w(gpui::relative((idx as f32 + 1.0) / steps as f32)).bg(t.signal)),
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
                                    steps
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
            .into_any_element()
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
        assert_eq!(profile_for("127.0.0.1", None).unwrap().id, "local");
        let p = profile_for("box.tail1234.ts.net", None).unwrap();
        assert_eq!(p.name, "box");
        assert_eq!(p.endpoint("content").unwrap().api, "https://box.tail1234.ts.net:8787");
        assert!(profile_for("http://box", None).is_err());
        assert_eq!(profile_for("100.101.102.103", None).unwrap().name, "100.101.102.103");
        assert_eq!(profile_for("server.tail1.ts.net", Some("server")).unwrap().name, "server");
    }
}
