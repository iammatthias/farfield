//! Settings: where the fleet is (the tailnet address, and any per-service
//! override), the keys to it, profiles, appearance, local data, and setup.
//!
//! Keys go straight to the Keychain, bound to the endpoint's origin; they are
//! never displayed again, only their hint.

use crate::app::{self, describe, log, state, AppState, Health};
use crate::shell::{self, confirm, set_health, toast, Overlay};
use crate::theme::{theme, Mode, Theme, FONT_DOC, S2, S3, S4, S5};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind};
use crate::workspace::{PaletteItem, Workspace};
use farfield_core::profile::{parse_tailscale_status, tailscale_status, TailnetPeer};
use farfield_core::registry;
use farfield_core::secret::Credential;
use gpui::{div, prelude::*, px, AnyElement, App, Context, Entity, SharedString, Window};

#[derive(Clone, Copy, PartialEq)]
enum Section {
    Fleet,
    Keys,
    Profiles,
    Appearance,
    Data,
    About,
}

const SECTIONS: [(Section, &str); 6] = [
    (Section::Fleet, "Fleet address"),
    (Section::Keys, "Keys"),
    (Section::Profiles, "Profiles"),
    (Section::Appearance, "Appearance"),
    (Section::Data, "Data on this Mac"),
    (Section::About, "About"),
];

pub struct Connections {
    section: Section,
    address: Entity<TextField>,
    address_error: Option<String>,
    peers: Option<Result<(bool, Vec<TailnetPeer>), String>>,
    key_field: Option<(String, Entity<TextField>)>,
    fleet_key: Entity<TextField>,
    endpoint_field: Option<(String, Entity<TextField>)>,
    testing: std::collections::BTreeSet<String>,
    results: std::collections::BTreeMap<String, String>,
    data_summary: Option<(u64, usize)>,
}

fn keyed(service: &str) -> bool {
    farfield_core::api::probe_path(service).is_some()
}

impl Connections {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let address = cx.new(|cx| TextField::new(w, cx, "Address", "host, URL, or IP").mono());
        if let Some(h) = state(cx).session.profile.common_host() {
            address.update(cx, |f, cx| f.set_text(h, cx));
        }
        cx.subscribe_in(&address, w, |this, _, e: &FieldEvent, _, cx| match e {
            FieldEvent::Submit => this.apply_address(cx),
            FieldEvent::Changed => {
                this.address_error = None;
                cx.notify()
            }
            _ => {}
        })
        .detach();
        let fleet_key = cx.new(|cx| TextField::new(w, cx, "Key for all", "ffk_…").secret());
        cx.subscribe_in(&fleet_key, w, |this, f, e: &FieldEvent, _, cx| {
            if *e == FieldEvent::Submit {
                let v = f.read(cx).text();
                this.store_key_all(&v, cx);
                f.update(cx, |f, cx| f.set_text("", cx));
            }
        })
        .detach();
        let mut this = Connections {
            section: Section::Fleet,
            address,
            address_error: None,
            peers: None,
            key_field: None,
            fleet_key,
            endpoint_field: None,
            testing: Default::default(),
            results: Default::default(),
            data_summary: None,
        };
        this.measure_data(cx);
        this
    }

    fn detect(&mut self, cx: &mut Context<Self>) {
        let task = farfield_core::spawn(async { tailscale_status() });
        cx.spawn(async move |this, cx| {
            let out = task.await.ok().flatten();
            let _ = this.update(cx, |this, cx| {
                this.peers = Some(match out {
                    None => Err("Tailscale isn't installed or isn't running.".into()),
                    Some(j) => parse_tailscale_status(&j),
                });
                cx.notify();
            });
        })
        .detach();
    }

    /// Point every service at the typed address (keeping this profile's id,
    /// so its drafts and cache stay with it).
    fn apply_address(&mut self, cx: &mut Context<Self>) {
        let typed = self.address.read(cx).text();
        let cur = state(cx).session.profile.clone();
        // a tailnet profile keeps its id, so its drafts and cache stay with it
        let p = crate::ws::onboarding::profile_for(&typed, None).map(|mut p| {
            if cur.id != "local" && p.id != "local" {
                p.id = cur.id.clone();
            }
            p
        });
        match p {
            Ok(p) => {
                let pid = p.id.clone();
                cx.global_mut::<AppState>().upsert_profile(p);
                activate(cx, &pid);
                log("settings-address", &[("profile", &pid), ("address", &typed)]);
                self.test_all(cx);
            }
            Err(e) => self.address_error = Some(e.to_string()),
        }
        cx.notify();
    }

    fn edit_endpoint(&mut self, service: &str, w: &mut Window, cx: &mut Context<Self>) {
        let svc = service.to_string();
        let cur = state(cx).session.profile.endpoint(service).map(|e| e.api.clone()).unwrap_or_default();
        let f = cx.new(|cx| {
            let mut f = TextField::new(w, cx, format!("{svc} address"), "https://host.tailnet.ts.net:port").mono();
            f.set_text(cur, cx);
            f
        });
        let s2 = svc.clone();
        cx.subscribe_in(&f, w, move |this, f, e: &FieldEvent, _, cx| match e {
            FieldEvent::Submit => {
                let v = f.read(cx).text();
                let mut p = state(cx).session.profile.clone();
                match p.set_api(&s2, &v) {
                    Ok(()) => {
                        if p.id == "local" {
                            p.id = "custom".into();
                            p.name = "Custom".into();
                        }
                        let id = p.id.clone();
                        cx.global_mut::<AppState>().upsert_profile(p);
                        activate(cx, &id);
                        log("settings-endpoint", &[("service", &s2), ("api", &v)]);
                        this.endpoint_field = None;
                        this.test(&s2, cx);
                    }
                    Err(e) => toast(cx, e.to_string(), true),
                }
                cx.notify()
            }
            FieldEvent::Cancel => {
                this.endpoint_field = None;
                cx.notify()
            }
            _ => {}
        })
        .detach();
        f.read(cx).focus(w);
        self.endpoint_field = Some((svc, f));
        cx.notify();
    }

    fn edit_key(&mut self, service: &str, w: &mut Window, cx: &mut Context<Self>) {
        let svc = service.to_string();
        let f = cx
            .new(|cx| TextField::new(w, cx, format!("{svc} key"), "paste an ffk_ token or the app's API key").secret());
        let s2 = svc.clone();
        cx.subscribe_in(&f, w, move |this, f, e: &FieldEvent, _w, cx| match e {
            FieldEvent::Submit => {
                let v = f.read(cx).text();
                this.store_key(&s2, &v, cx);
            }
            FieldEvent::Cancel => {
                this.key_field = None;
                cx.notify();
            }
            _ => {}
        })
        .detach();
        f.read(cx).focus(w);
        self.key_field = Some((svc, f));
        cx.notify();
    }

    fn store_key(&mut self, service: &str, value: &str, cx: &mut Context<Self>) {
        let Some(c) = Credential::new(value) else {
            toast(cx, "That key is empty or has spaces in it.", true);
            return;
        };
        match app::session(cx).set_credential(service, &c) {
            Ok(()) => {
                log("key-set", &[("service", service), ("hint", &c.hint())]);
                self.key_field = None;
                set_health(cx, service, Health::Unknown);
                self.test(service, cx);
            }
            Err(e) => toast(cx, format!("Keychain refused the key: {e}"), true),
        }
        cx.notify();
    }

    fn store_key_all(&mut self, value: &str, cx: &mut Context<Self>) {
        let Some(c) = Credential::new(value) else {
            toast(cx, "That key is empty or has spaces in it.", true);
            return;
        };
        let session = app::session(cx);
        let mut n = 0;
        for s in registry::services().iter().filter(|s| keyed(&s.name)) {
            if let Err(e) = session.set_credential(&s.name, &c) {
                toast(cx, format!("Keychain refused the key: {e}"), true);
                return;
            }
            n += 1;
        }
        log("key-set-all", &[("services", &n.to_string()), ("hint", &c.hint())]);
        toast(cx, format!("Stored for {n} services — testing them now."), false);
        self.test_all(cx);
    }

    fn forget_key(&mut self, service: &str, cx: &mut Context<Self>) {
        if let Err(e) = app::session(cx).forget_credential(service) {
            toast(cx, e, true);
        }
        log("key-forget", &[("service", service)]);
        set_health(cx, service, Health::NoAuth);
        self.results.remove(service);
        cx.notify();
    }

    fn test(&mut self, service: &str, cx: &mut Context<Self>) {
        let session = app::session(cx);
        let svc = service.to_string();
        self.testing.insert(svc.clone());
        let task = {
            let svc = svc.clone();
            farfield_core::spawn(async move { farfield_core::api::probe(&session, &svc).await })
        };
        cx.spawn(async move |this, cx| {
            let r = task.await.unwrap_or_else(|e| Err(farfield_core::ApiError::Offline(e.to_string())));
            let _ = this.update(cx, |this, cx| {
                this.testing.remove(&svc);
                let (h, msg) = match &r {
                    Ok(()) => (Health::Up, "connected".to_string()),
                    Err(e) if e.is_auth() => (Health::NoAuth, describe(e)),
                    Err(e) => (Health::Down(e.to_string()), describe(e)),
                };
                log("probe", &[("service", &svc), ("result", &msg)]);
                this.results.insert(svc.clone(), msg);
                set_health(cx, &svc, h);
                cx.notify();
            });
        })
        .detach();
    }

    fn test_all(&mut self, cx: &mut Context<Self>) {
        for s in registry::services() {
            self.test(&s.name.clone(), cx);
        }
    }

    fn measure_data(&mut self, cx: &mut Context<Self>) {
        let dir = state(cx).data_dir.clone();
        let task = farfield_core::spawn(async move {
            fn walk(p: &std::path::Path, bytes: &mut u64, drafts: &mut usize) {
                for e in std::fs::read_dir(p).into_iter().flatten().flatten() {
                    let path = e.path();
                    if path.is_dir() {
                        walk(&path, bytes, drafts);
                    } else if let Ok(m) = e.metadata() {
                        *bytes += m.len();
                        if path.components().any(|c| c.as_os_str() == "drafts") {
                            *drafts += 1;
                        }
                    }
                }
            }
            let (mut b, mut d) = (0, 0);
            walk(&dir, &mut b, &mut d);
            (b, d)
        });
        cx.spawn(async move |this, cx| {
            if let Ok(v) = task.await {
                let _ = this.update(cx, |this, cx| {
                    this.data_summary = Some(v);
                    cx.notify()
                });
            }
        })
        .detach();
    }

    /// Remove cached responses (never drafts) for every profile.
    fn clear_cache(&mut self, cx: &mut Context<Self>) {
        let dir = state(cx).data_dir.join("p");
        let task = farfield_core::spawn(async move {
            let mut n = 0;
            for prof in std::fs::read_dir(&dir).into_iter().flatten().flatten() {
                for ident in std::fs::read_dir(prof.path()).into_iter().flatten().flatten() {
                    let c = ident.path().join("cache");
                    if c.exists() && std::fs::remove_dir_all(&c).is_ok() {
                        n += 1;
                    }
                }
            }
            n
        });
        cx.spawn(async move |this, cx| {
            let n = task.await.unwrap_or(0);
            let _ = this.update(cx, |this, cx| {
                log("cache-cleared", &[("scopes", &n.to_string())]);
                toast(cx, "Cached responses cleared. Drafts were kept.", false);
                this.measure_data(cx);
                cx.global_mut::<Overlay>().reset = true;
                cx.refresh_windows();
            });
        })
        .detach();
    }

    // ── sections ──

    fn sec_fleet(&mut self, t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let st = state(cx);
        let active = st.session.profile.clone();
        let health = st.health.clone();
        let session = st.session.clone();
        let peers = match &self.peers {
            None => div().into_any_element(),
            Some(Err(e)) => ui::notice(e.clone(), t.warn, cx).into_any_element(),
            Some(Ok((running, peers))) => div()
                .flex()
                .flex_wrap()
                .gap(S2)
                .when(!running, |d| d.child(ui::notice("Tailscale is installed but not connected.", t.warn, cx)))
                .children(peers.iter().take(10).map(|p| {
                    let dns = p.dns_name.clone();
                    div()
                        .id(SharedString::from(format!("peer-{}", p.dns_name)))
                        .px(S3)
                        .py(px(4.))
                        .rounded(px(4.))
                        .border_1()
                        .border_color(t.rule)
                        .cursor_pointer()
                        .flex()
                        .gap(px(6.))
                        .items_center()
                        .child(div().flex_none().w(px(6.)).h(px(6.)).rounded_full().bg(if p.online {
                            t.good
                        } else {
                            t.ink_3
                        }))
                        .child(div().text_sm().child(p.host_name.clone()))
                        .on_click(cx.listener(move |this, _, _, cx| {
                            let d = dns.clone();
                            this.address.update(cx, |f, cx| f.set_text(d, cx));
                        }))
                }))
                .into_any_element(),
        };
        let rows =
            registry::services().iter().map(|s| {
                let name = s.name.clone();
                let ep = active.endpoint(&s.name).cloned();
                let h = health.get(&s.name).cloned().unwrap_or(Health::Unknown);
                let color = match h {
                    Health::Up => t.good,
                    Health::NoAuth => t.warn,
                    Health::Down(_) => t.bad,
                    _ => t.ink_3,
                };
                let editing = self.endpoint_field.as_ref().filter(|(k, _)| *k == name).map(|(_, f)| f.clone());
                let result = self.results.get(&name).cloned();
                let (n1, n2) = (name.clone(), name.clone());
                let testing = self.testing.contains(&name);
                let _ = &session;
                div()
                    .flex()
                    .flex_col()
                    .gap(px(4.))
                    .py(S3)
                    .border_b_1()
                    .border_color(t.rule)
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap(S4)
                            .child(
                                div()
                                    .w(px(104.))
                                    .flex_none()
                                    .text_sm()
                                    .font_weight(gpui::FontWeight::MEDIUM)
                                    .child(name.clone()),
                            )
                            .child(div().w(px(96.)).flex_none().child(ui::chip(h.word(), color, cx)))
                            .child(
                                div().flex_1().min_w_0().child(
                                    ui::mono(ep.as_ref().map(|e| e.api.clone()).unwrap_or_default(), cx).truncate(),
                                ),
                            )
                            .child(ui::button(SharedString::from(format!("ep-{n1}")), "Change", Kind::Quiet, cx, {
                                let e = cx.entity();
                                move |_, w, cx| e.update(cx, |this, cx| this.edit_endpoint(&n1, w, cx))
                            }))
                            .child(ui::button(
                                SharedString::from(format!("test-{n2}")),
                                if testing { "Testing…" } else { "Test" },
                                Kind::Quiet,
                                cx,
                                {
                                    let e = cx.entity();
                                    move |_, _, cx| e.update(cx, |this, cx| this.test(&n2, cx))
                                },
                            )),
                    )
                    .when_some(ep.and_then(|e| e.public), |d, p| {
                        d.child(div().pl(px(120.)).text_xs().text_color(t.ink_3).child(format!("share links: {p}")))
                    })
                    .when_some(result, |d, r| d.child(div().pl(px(120.)).text_xs().text_color(t.ink_2).child(r)))
                    .when_some(editing, |d, f| {
                        d.child(
                            div().pl(px(120.)).pr(S5).pt(S2).child(f).child(
                                div().text_xs().text_color(t.ink_3).pt(px(4.)).child("Enter saves · Esc cancels"),
                            ),
                        )
                    })
            });
        div()
            .flex()
            .flex_col()
            .gap(S4)
            .child(
                div()
                    .flex()
                    .items_end()
                    .gap(S3)
                    .child(div().w(px(420.)).child(self.address.clone()))
                    .child(ui::button("apply-address", "Use this address", Kind::Primary, cx, {
                        let e = cx.entity();
                        move |_, _, cx| e.update(cx, |this, cx| this.apply_address(cx))
                    }))
                    .child(ui::button("detect", "Use Tailscale", Kind::Quiet, cx, {
                        let e = cx.entity();
                        move |_, _, cx| e.update(cx, |this, cx| this.detect(cx))
                    })),
            )
            .when_some(self.address_error.clone(), |d, e| d.child(ui::notice(e, t.bad, cx)))
            .when(active.common_host().is_none(), |d| {
                d.child(ui::notice("Some services have their own address.", t.warn, cx))
            })
            .child(peers)
            .child(
                div()
                    .flex()
                    .justify_between()
                    .items_center()
                    .pt(S3)
                    .child(ui::eyebrow(format!("Services · {}", active.name), cx))
                    .child(ui::button("test-all", "Test all", Kind::Quiet, cx, {
                        let e = cx.entity();
                        move |_, _, cx| e.update(cx, |this, cx| this.test_all(cx))
                    })),
            )
            .child(div().flex().flex_col().border_t_1().border_color(t.rule).children(rows))
            .into_any_element()
    }

    fn sec_keys(&mut self, t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let st = state(cx);
        let session = st.session.clone();
        let health = st.health.clone();
        let rows = registry::services().iter().filter(|s| keyed(&s.name)).map(|s| {
            let name = s.name.clone();
            let hint = session.client(&s.name).ok().and_then(|c| c.credential().map(|c| c.hint()));
            let h = health.get(&s.name).cloned().unwrap_or(Health::Unknown);
            let color = match h {
                Health::Up => t.good,
                Health::NoAuth => t.warn,
                Health::Down(_) => t.bad,
                _ => t.ink_3,
            };
            let editing = self.key_field.as_ref().filter(|(k, _)| *k == name).map(|(_, f)| f.clone());
            let (n1, n2) = (name.clone(), name.clone());
            div()
                .flex()
                .flex_col()
                .py(S3)
                .border_b_1()
                .border_color(t.rule)
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap(S4)
                        .child(
                            div()
                                .w(px(104.))
                                .flex_none()
                                .text_sm()
                                .font_weight(gpui::FontWeight::MEDIUM)
                                .child(name.clone()),
                        )
                        .child(div().w(px(96.)).flex_none().child(ui::chip(h.word(), color, cx)))
                        .child(
                            div().flex_1().child(ui::mono(hint.clone().unwrap_or_else(|| "no key stored".into()), cx)),
                        )
                        .child(ui::button(
                            SharedString::from(format!("key-{n1}")),
                            if hint.is_some() { "Replace" } else { "Add key" },
                            Kind::Quiet,
                            cx,
                            {
                                let e = cx.entity();
                                move |_, w, cx| e.update(cx, |this, cx| this.edit_key(&n1, w, cx))
                            },
                        ))
                        .when(hint.is_some(), |d| {
                            d.child(ui::button(
                                SharedString::from(format!("forget-{n2}")),
                                "Forget",
                                Kind::Danger,
                                cx,
                                {
                                    let e = cx.entity();
                                    move |_, _, cx| {
                                        let e = e.clone();
                                        let n = n2.clone();
                                        confirm(cx, format!("Forget the {n} key?"), "", "Forget", true, move |_, cx| {
                                            e.update(cx, |this, cx| this.forget_key(&n, cx))
                                        })
                                    }
                                },
                            ))
                        }),
                )
                .when_some(editing, |d, f| {
                    d.child(
                        div()
                            .pl(px(120.))
                            .pr(S5)
                            .pt(S2)
                            .child(f)
                            .child(div().text_xs().text_color(t.ink_3).pt(px(4.)).child("Enter saves · Esc cancels")),
                    )
                })
        });
        div()
            .flex()
            .flex_col()
            .gap(S4)
            .child(crate::signin::view("passkey", cx, {
                let e = cx.entity();
                move |cx| {
                    let e = e.clone();
                    crate::signin::run(cx, move |ok, cx| {
                        if ok {
                            e.update(cx, |this, cx| this.test_all(cx))
                        }
                    })
                }
            }))
            .child(
                div()
                    .flex()
                    .items_end()
                    .gap(S3)
                    .child(div().w(px(420.)).child(self.fleet_key.clone()))
                    .child(ui::button("key-all", "Use for all", Kind::Quiet, cx, {
                        let e = cx.entity();
                        move |_, _, cx| {
                            e.update(cx, |this, cx| {
                                let v = this.fleet_key.read(cx).text();
                                this.store_key_all(&v, cx);
                                this.fleet_key.update(cx, |f, cx| f.set_text("", cx));
                            })
                        }
                    }))
                    .child(ui::button("console-keys", "Keys console ↗", Kind::Quiet, cx, |_, _, cx| {
                        open_console(cx, "keys")
                    })),
            )
            .child(div().flex().flex_col().border_t_1().border_color(t.rule).children(rows))
            .into_any_element()
    }

    fn sec_profiles(&mut self, t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let st = state(cx);
        let active = st.session.profile.id.clone();
        let profiles = st.profiles.clone();
        div()
            .flex()
            .flex_col()
            .gap(S4)
            .child(div().flex().flex_col().border_t_1().border_color(t.rule).children(profiles.into_iter().map(|p| {
                let on = p.id == active;
                let (id, id2) = (p.id.clone(), p.id.clone());
                let host = p.common_host().unwrap_or_else(|| "several hosts".into());
                ui::list_row(SharedString::from(format!("profile-{}", p.id)), on, t)
                    .flex()
                    .items_center()
                    .gap(S4)
                    .child(
                        div()
                            .flex_1()
                            .flex()
                            .flex_col()
                            .child(div().text_sm().child(p.name.clone()))
                            .child(ui::mono(host, cx)),
                    )
                    .child(if on {
                        ui::chip("in use", t.good, cx).into_any_element()
                    } else {
                        ui::button(SharedString::from(format!("use-{id}")), "Use", Kind::Quiet, cx, move |_, _, cx| {
                            activate(cx, &id)
                        })
                        .into_any_element()
                    })
                    .when(!on && p.id != "local", |d| {
                        d.child(ui::button(
                            SharedString::from(format!("rm-{id2}")),
                            "Remove",
                            Kind::Danger,
                            cx,
                            move |_, _, cx| {
                                let id = id2.clone();
                                confirm(cx, "Remove this profile?", "", "Remove", true, move |_, cx| {
                                    let st = cx.global_mut::<AppState>();
                                    st.profiles.retain(|p| p.id != id);
                                    st.save_profiles();
                                    log("profile-removed", &[("id", &id)]);
                                    cx.refresh_windows();
                                })
                            },
                        ))
                    })
            })))
            .into_any_element()
    }

    fn sec_appearance(&mut self, t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let prefs = state(cx).prefs.clone();
        let mode_btn = |label: &'static str, m: Mode, cx: &mut Context<Self>| {
            let on = prefs.mode == m;
            div()
                .id(label)
                .px(S3)
                .py(px(5.))
                .text_sm()
                .cursor_pointer()
                .border_b_2()
                .border_color(if on { t.accent } else { gpui::transparent_black() })
                .text_color(if on { t.ink } else { t.ink_2 })
                .child(label)
                .on_click(cx.listener(move |_, _, w, cx| {
                    cx.global_mut::<AppState>().prefs.mode = m;
                    state(cx).save_prefs();
                    shell::apply_theme(w, cx);
                }))
        };
        div()
            .flex()
            .flex_col()
            .gap(S5)
            .child(
                div()
                    .flex()
                    .gap(S2)
                    .child(mode_btn("System", Mode::System, cx))
                    .child(mode_btn("Light", Mode::Light, cx))
                    .child(mode_btn("Dark", Mode::Dark, cx)),
            )
            .child(
                div()
                    .id("rm")
                    .flex()
                    .items_center()
                    .gap(S3)
                    .cursor_pointer()
                    .child(crate::ws::onboarding::toggle(prefs.reduced_motion, t))
                    .child(div().flex().flex_col().child(div().text_sm().child("Reduce motion")).child(
                        div().text_xs().text_color(t.ink_3).child("No blinking caret, no animated transitions."),
                    ))
                    .on_click(cx.listener(|_, _, w, cx| {
                        let p = &mut cx.global_mut::<AppState>().prefs;
                        p.reduced_motion = !p.reduced_motion;
                        state(cx).save_prefs();
                        shell::apply_theme(w, cx);
                    })),
            )
            .into_any_element()
    }

    fn sec_data(&mut self, _t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let dir = state(cx).data_dir.clone();
        let (bytes, drafts) = self.data_summary.unwrap_or((0, 0));
        div()
            .flex()
            .flex_col()
            .gap(S4)
            .child(ui::field_row("Folder", ui::mono(dir.display().to_string(), cx), cx))
            .child(ui::field_row(
                "Size",
                ui::mono(format!("{} · {drafts} draft file(s)", ui::bytes(bytes as i64)), cx),
                cx,
            ))
            .child(
                div()
                    .flex()
                    .gap(S2)
                    .child(ui::button("reveal", "Show in Finder", Kind::Quiet, cx, move |_, _, cx| {
                        cx.reveal_path(&dir)
                    }))
                    .child(ui::button("clear-cache", "Clear cached responses…", Kind::Danger, cx, {
                        let e = cx.entity();
                        move |_, _, cx| {
                            let e = e.clone();
                            confirm(cx, "Clear cached responses?", "Drafts are kept.", "Clear", true, move |_, cx| {
                                e.update(cx, |this, cx| this.clear_cache(cx))
                            })
                        }
                    })),
            )
            .into_any_element()
    }

    fn sec_about(&mut self, _t: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let editor: serde_json::Value = serde_json::from_str(farfield_editor::assets::MANIFEST).unwrap_or_default();
        div()
            .flex()
            .flex_col()
            .gap(S4)
            .child(div().font_family(FONT_DOC).text_size(px(24.)).child("Farfield"))
            .child(ui::field_row("Version", ui::mono(env!("CARGO_PKG_VERSION"), cx), cx))
            .child(ui::field_row(
                "Editor",
                ui::mono(format!("editor.wasm {}", editor["version"].as_str().unwrap_or("?")), cx),
                cx,
            ))
            .child(ui::field_row(
                "Services",
                ui::mono(format!("{} in the fleet registry", registry::services().len()), cx),
                cx,
            ))
            .child(
                div()
                    .flex()
                    .gap(S2)
                    .pt(S2)
                    .child(ui::button("setup-again", "Run setup again", Kind::Quiet, cx, |_, _, cx| {
                        let st = cx.global_mut::<AppState>();
                        st.prefs.onboarded = false;
                        st.save_prefs();
                        log("setup-again", &[]);
                        cx.refresh_windows();
                    }))
                    .child(ui::button("console-pulse", "Pulse admin ↗", Kind::Quiet, cx, |_, _, cx| {
                        open_console(cx, "pulse")
                    }))
                    .child(ui::button("console-backup", "Backup console ↗", Kind::Quiet, cx, |_, _, cx| {
                        open_console(cx, "backup")
                    })),
            )
            .into_any_element()
    }
}

pub fn activate(cx: &mut App, id: &str) {
    cx.global_mut::<AppState>().activate(id);
    cx.global_mut::<Overlay>().reset = true;
    toast(cx, format!("Using {}", state(cx).session.profile.name), false);
    cx.refresh_windows();
}

impl Workspace for Connections {
    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        vec![
            ("sec-fleet", "Settings: fleet address".into(), ""),
            ("sec-keys", "Settings: keys".into(), ""),
            ("sec-profiles", "Settings: profiles".into(), ""),
            ("sec-look", "Settings: appearance".into(), ""),
            ("sec-data", "Settings: data on this Mac".into(), ""),
            ("sec-about", "Settings: about".into(), ""),
            ("test-all", "Settings: test every connection".into(), "⌘R"),
            ("detect", "Settings: use Tailscale".into(), ""),
            ("setup", "Settings: run setup again".into(), ""),
            ("keys", "Open the keys console (browser)".into(), ""),
            ("pulse", "Open the pulse console (browser)".into(), ""),
        ]
    }
    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "test-all" => self.test_all(cx),
            "sec-keys" => {
                self.section = Section::Keys;
                self.fleet_key.read(cx).focus(w);
            }
            "sec-fleet" => self.section = Section::Fleet,
            "sec-profiles" => self.section = Section::Profiles,
            "sec-look" => self.section = Section::Appearance,
            "sec-data" => {
                self.section = Section::Data;
                self.measure_data(cx)
            }
            "sec-about" => self.section = Section::About,
            "detect" => {
                self.section = Section::Fleet;
                self.detect(cx)
            }
            "setup" => {
                let st = cx.global_mut::<AppState>();
                st.prefs.onboarded = false;
                st.save_prefs();
                cx.refresh_windows();
            }
            "keys" => open_console(cx, "keys"),
            "pulse" => open_console(cx, "pulse"),
            _ => {}
        }
    }
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.test_all(cx);
    }
    fn palette(&self, _cx: &App) -> Vec<PaletteItem> {
        vec![PaletteItem::new("Settings", "⌘,", |_, cx| shell::goto(cx, "connections"))]
    }
}

/// Hand off to a session-authenticated web console on its *private* address.
/// The administrator password is typed there, in the browser, never here.
pub fn open_console(cx: &mut App, service: &str) {
    let s = app::session(cx);
    match s.profile.endpoint(service) {
        Some(ep) => {
            log("console-handoff", &[("service", service), ("url", &ep.api)]);
            cx.open_url(&format!("{}/", ep.api.trim_end_matches('/')));
        }
        None => toast(cx, format!("{service} has no endpoint in this profile"), true),
    }
}

impl Render for Connections {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let body = match self.section {
            Section::Fleet => self.sec_fleet(&t, cx),
            Section::Keys => self.sec_keys(&t, cx),
            Section::Profiles => self.sec_profiles(&t, cx),
            Section::Appearance => self.sec_appearance(&t, cx),
            Section::Data => self.sec_data(&t, cx),
            Section::About => self.sec_about(&t, cx),
        };
        let title = SECTIONS.iter().find(|(s, _)| *s == self.section).map(|(_, l)| *l).unwrap_or("");
        div()
            .size_full()
            .flex()
            .child(
                div()
                    .w(px(210.))
                    .flex_none()
                    .h_full()
                    .border_r_1()
                    .border_color(t.rule)
                    .pt(S5)
                    .px(S3)
                    .flex()
                    .flex_col()
                    .gap(px(2.))
                    .child(div().px(S3).pb(S3).text_xs().text_color(t.ink_3).child("SETTINGS"))
                    .children(SECTIONS.iter().map(|(s, label)| {
                        let on = *s == self.section;
                        let sec = *s;
                        let wash = t.wash;
                        div()
                            .id(*label)
                            .px(S3)
                            .py(px(6.))
                            .rounded(px(4.))
                            .text_sm()
                            .cursor_pointer()
                            .text_color(if on { t.ink } else { t.ink_2 })
                            .when(on, |d| d.bg(t.accent_soft).font_weight(gpui::FontWeight::MEDIUM))
                            .when(!on, move |d| d.hover(move |s| s.bg(wash)))
                            .child(*label)
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.section = sec;
                                if sec == Section::Data {
                                    this.measure_data(cx);
                                }
                                cx.notify()
                            }))
                    })),
            )
            .child(
                div().id("settings-body").flex_1().h_full().overflow_y_scroll().child(
                    div()
                        .max_w(px(900.))
                        .px(px(40.))
                        .py(S5)
                        .flex()
                        .flex_col()
                        .gap(S5)
                        .child(div().font_family(FONT_DOC).text_size(px(28.)).text_color(t.ink).child(title))
                        .child(body)
                        .child(div().h(S4)),
                ),
            )
    }
}
