//! Connections: onboarding, profiles, per-service endpoints and keys, health,
//! and appearance. Keys go straight to the Keychain, bound to the endpoint's
//! origin; they are never displayed again, only their hint.

use crate::app::{self, describe, log, state, AppState, Health};
use crate::shell::{self, set_health, toast, Overlay};
use crate::theme::{theme, Mode, FONT_MONO, S2, S3, S4, S5};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind};
use crate::workspace::{PaletteItem, Workspace};
use farfield_core::profile::{parse_tailscale_status, tailscale_status, Profile, TailnetPeer};
use farfield_core::registry;
use farfield_core::secret::Credential;
use gpui::{div, prelude::*, px, App, Context, Entity, SharedString, Window};

pub struct Connections {
    key_field: Option<(String, Entity<TextField>)>,
    peers: Option<Result<(bool, Vec<TailnetPeer>), String>>,
    host_field: Entity<TextField>,
    testing: Option<String>,
    results: std::collections::BTreeMap<String, String>,
}

impl Connections {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let host_field = cx.new(|cx| TextField::new(w, cx, "Tailnet host", "homelab.tailXXXX.ts.net").mono());
        Connections { key_field: None, peers: None, host_field, testing: None, results: Default::default() }
    }

    fn detect(&mut self, cx: &mut Context<Self>) {
        let task = farfield_core::spawn(async { tailscale_status() });
        cx.spawn(async move |this, cx| {
            let out = task.await.ok().flatten();
            let _ = this.update(cx, |this, cx| {
                this.peers = Some(match out {
                    None => Err("Tailscale isn't installed or isn't running. Start the Tailscale app, then detect again.".into()),
                    Some(j) => parse_tailscale_status(&j),
                });
                if let Some(Ok((_, peers))) = &this.peers {
                    if let Some(p) = peers.first() {
                        let dns = p.dns_name.clone();
                        this.host_field.update(cx, |f, cx| f.set_text(dns, cx));
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn use_host(&mut self, cx: &mut Context<Self>) {
        let host = self.host_field.read(cx).text().trim().trim_end_matches('.').to_string();
        if host.is_empty() || host.contains('/') || host.contains(' ') {
            toast(cx, "Enter the homelab's tailnet name, like homelab.tail1234.ts.net", true);
            return;
        }
        let p = Profile::tailnet("homelab", "Homelab (tailnet)", &host);
        if let Err(e) = p.validate() {
            toast(cx, e.to_string(), true);
            return;
        }
        cx.global_mut::<AppState>().upsert_profile(p);
        activate(cx, "homelab");
    }

    fn edit_key(&mut self, service: &str, w: &mut Window, cx: &mut Context<Self>) {
        let svc = service.to_string();
        let f = cx.new(|cx| TextField::new(w, cx, format!("{svc} key"), "paste an ffk_ token or the app's API key").secret());
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
        let session = app::session(cx);
        match session.set_credential(service, &c) {
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

    fn forget_key(&mut self, service: &str, cx: &mut Context<Self>) {
        let session = app::session(cx);
        if let Err(e) = session.forget_credential(service) {
            toast(cx, e, true);
        }
        log("key-forget", &[("service", service)]);
        set_health(cx, service, Health::NoAuth);
        cx.notify();
    }

    fn test(&mut self, service: &str, cx: &mut Context<Self>) {
        let session = app::session(cx);
        let svc = service.to_string();
        self.testing = Some(svc.clone());
        let task = {
            let svc = svc.clone();
            farfield_core::spawn(async move { farfield_core::api::probe(&session, &svc).await })
        };
        cx.spawn(async move |this, cx| {
            let r = task.await.unwrap_or_else(|e| Err(farfield_core::ApiError::Offline(e.to_string())));
            let _ = this.update(cx, |this, cx| {
                this.testing = None;
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
}

pub fn activate(cx: &mut App, id: &str) {
    cx.global_mut::<AppState>().activate(id);
    cx.global_mut::<Overlay>().reset = true;
    toast(cx, format!("Using {}", state(cx).session.profile.name), false);
    cx.refresh_windows();
}

impl Workspace for Connections {
    fn palette(&self, _cx: &App) -> Vec<PaletteItem> {
        let mut v = vec![PaletteItem::new("Test all connections", "", |_, cx| shell::goto(cx, "connections"))];
        v.push(PaletteItem::new("Open the keys console (browser)", "private", |_, cx| open_console(cx, "keys")));
        v.push(PaletteItem::new("Open the pulse console (browser)", "private", |_, cx| open_console(cx, "pulse")));
        v
    }
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.test_all(cx);
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
        let st = state(cx);
        let active = st.session.profile.clone();
        let profiles = st.profiles.clone();
        let prefs = st.prefs.clone();
        let health = st.health.clone();
        let session = st.session.clone();

        let profile_rows = profiles.into_iter().map(|p| {
            let on = p.id == active.id;
            let id = p.id.clone();
            let sample = p.endpoint("content").map(|e| e.api.clone()).unwrap_or_default();
            ui::list_row(SharedString::from(format!("profile-{}", p.id)), on, &t)
                .flex()
                .justify_between()
                .child(div().flex().flex_col().child(div().text_sm().child(p.name.clone())).child(ui::mono(sample, cx)))
                .child(if on { ui::chip("active", t.good, cx) } else { ui::chip("use", t.ink_3, cx) })
                .on_click(cx.listener(move |_, _, _, cx| activate(cx, &id)))
        });

        let peers = match &self.peers {
            None => div(),
            Some(Err(e)) => ui::notice(e.clone(), t.warn, cx),
            Some(Ok((running, peers))) => div()
                .flex()
                .flex_col()
                .gap(S2)
                .when(!running, |d| d.child(ui::notice("Tailscale is installed but not connected.", t.warn, cx)))
                .children(peers.iter().take(6).map(|p| {
                    let dns = p.dns_name.clone();
                    div()
                        .id(SharedString::from(format!("peer-{}", p.dns_name)))
                        .flex()
                        .gap(S3)
                        .cursor_pointer()
                        .child(ui::chip(if p.online { "online" } else { "offline" }, if p.online { t.good } else { t.ink_3 }, cx))
                        .child(ui::mono(p.dns_name.clone(), cx))
                        .on_click(cx.listener(move |this, _, _, cx| {
                            let d = dns.clone();
                            this.host_field.update(cx, |f, cx| f.set_text(d, cx));
                        }))
                })),
        };

        let services = registry::services().iter().map(|s| {
            let name = s.name.clone();
            let ep = active.endpoint(&s.name).cloned();
            let h = health.get(&s.name).cloned().unwrap_or(Health::Unknown);
            let color = match h {
                Health::Up => t.good,
                Health::NoAuth => t.warn,
                Health::Down(_) => t.bad,
                _ => t.ink_3,
            };
            let hint = session.client(&s.name).ok().and_then(|c| c.credential().map(|c| c.hint()));
            let needs_key = farfield_core::api::probe_path(&s.name).is_some();
            let editing = self.key_field.as_ref().filter(|(k, _)| *k == name).map(|(_, f)| f.clone());
            let result = self.results.get(&name).cloned();
            let (n1, n2, n3) = (name.clone(), name.clone(), name.clone());
            div()
                .flex()
                .flex_col()
                .gap(S2)
                .py(S3)
                .border_b_1()
                .border_color(t.rule)
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap(S4)
                        .child(div().w(px(110.)).text_sm().font_weight(gpui::FontWeight::MEDIUM).child(name.clone()))
                        .child(ui::chip(h.word(), color, cx))
                        .child(div().flex_1().child(ui::mono(ep.as_ref().map(|e| e.api.clone()).unwrap_or_default(), cx)))
                        .when(needs_key, |d| {
                            d.child(ui::mono(hint.clone().unwrap_or_else(|| "no key".into()), cx))
                                .child(ui::button(SharedString::from(format!("key-{n1}")), if hint.is_some() { "Replace key" } else { "Add key" }, Kind::Quiet, cx, {
                                    let e = cx.entity();
                                    move |_, w, cx| e.update(cx, |this, cx| this.edit_key(&n1, w, cx))
                                }))
                                .when(hint.is_some(), |d| {
                                    d.child(ui::button(SharedString::from(format!("forget-{n2}")), "Forget", Kind::Danger, cx, {
                                        let e = cx.entity();
                                        move |_, _, cx| e.update(cx, |this, cx| this.forget_key(&n2, cx))
                                    }))
                                })
                        })
                        .child(ui::button(SharedString::from(format!("test-{n3}")), if self.testing.as_deref() == Some(&n3) { "Testing…" } else { "Test" }, Kind::Quiet, cx, {
                            let e = cx.entity();
                            move |_, _, cx| e.update(cx, |this, cx| this.test(&n3, cx))
                        })),
                )
                .when_some(ep.and_then(|e| e.public), |d, p| d.child(div().pl(px(126.)).text_xs().text_color(t.ink_3).child(format!("share links: {p}"))))
                .when_some(result, |d, r| d.child(div().pl(px(126.)).text_xs().text_color(t.ink_2).child(r)))
                .when_some(editing, |d, f| {
                    d.child(div().pl(px(126.)).pr(S5).child(f).child(div().text_xs().text_color(t.ink_3).pt(px(4.)).child("Enter stores it in the Keychain for this endpoint only · Esc cancels")))
                })
        });

        let mode_btn = |label: &'static str, m: Mode, cx: &mut Context<Self>| {
            let on = prefs.mode == m;
            div()
                .id(label)
                .px(S3)
                .py(px(4.))
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
            .id("connections")
            .size_full()
            .overflow_y_scroll()
            .child(
                div()
                    .max_w(px(920.))
                    .px(px(40.))
                    .py(S5)
                    .flex()
                    .flex_col()
                    .gap(S5)
                    .child(div().text_xl().font_weight(gpui::FontWeight::SEMIBOLD).child("Connections"))
                    .child(div().text_sm().text_color(t.ink_2).max_w(px(640.)).child(
                        "API and media traffic goes to each service's private address on your tailnet, so the app keeps working when the public site doesn't. Public addresses are used only for links you share.",
                    ))
                    .child(ui::eyebrow("Profiles", cx))
                    .child(div().flex().flex_col().children(profile_rows))
                    .child(ui::eyebrow("Find the homelab on your tailnet", cx))
                    .child(
                        div()
                            .flex()
                            .items_end()
                            .gap(S4)
                            .child(div().w(px(380.)).child(self.host_field.clone()))
                            .child(ui::button("detect", "Detect with Tailscale", Kind::Quiet, cx, {
                                let e = cx.entity();
                                move |_, _, cx| e.update(cx, |this, cx| this.detect(cx))
                            }))
                            .child(ui::button("use-host", "Use this host", Kind::Primary, cx, {
                                let e = cx.entity();
                                move |_, _, cx| e.update(cx, |this, cx| this.use_host(cx))
                            })),
                    )
                    .child(peers)
                    .child(
                        div()
                            .flex()
                            .justify_between()
                            .items_center()
                            .child(ui::eyebrow(format!("Services · {}", active.name), cx))
                            .child(ui::button("test-all", "Test all", Kind::Quiet, cx, {
                                let e = cx.entity();
                                move |_, _, cx| e.update(cx, |this, cx| this.test_all(cx))
                            })),
                    )
                    .child(div().flex().flex_col().border_t_1().border_color(t.rule).children(services))
                    .child(ui::eyebrow("Consoles", cx))
                    .child(
                        div()
                            .flex()
                            .gap(S2)
                            .child(ui::button("console-keys", "Keys console ↗", Kind::Quiet, cx, |_, _, cx| open_console(cx, "keys")))
                            .child(ui::button("console-pulse", "Pulse admin ↗", Kind::Quiet, cx, |_, _, cx| open_console(cx, "pulse")))
                            .child(ui::button("console-backup", "Backup console ↗", Kind::Quiet, cx, |_, _, cx| open_console(cx, "backup"))),
                    )
                    .child(div().text_xs().text_color(t.ink_3).child("Consoles open in your browser at their private address; sign in there. Administrator passwords never pass through this app."))
                    .child(ui::eyebrow("Appearance", cx))
                    .child(
                        div()
                            .flex()
                            .gap(S2)
                            .child(mode_btn("System", Mode::System, cx))
                            .child(mode_btn("Light", Mode::Light, cx))
                            .child(mode_btn("Dark", Mode::Dark, cx))
                            .child(div().w(px(24.)))
                            .child(
                                div()
                                    .id("rm")
                                    .px(S3)
                                    .py(px(4.))
                                    .text_sm()
                                    .cursor_pointer()
                                    .text_color(t.ink_2)
                                    .child(if prefs.reduced_motion { "Reduced motion: on" } else { "Reduced motion: off" })
                                    .on_click(cx.listener(|_, _, w, cx| {
                                        let p = &mut cx.global_mut::<AppState>().prefs;
                                        p.reduced_motion = !p.reduced_motion;
                                        state(cx).save_prefs();
                                        shell::apply_theme(w, cx);
                                    })),
                            ),
                    )
                    .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(format!("data: {}", st_data(cx))))
                    .child(div().h(S4)),
            )
    }
}

fn st_data(cx: &App) -> String {
    state(cx).data_dir.display().to_string()
}
