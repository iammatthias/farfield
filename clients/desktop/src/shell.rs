//! The window: navigation, the active workspace, the inspector, a status
//! line, and the floating layers (palette, confirmations, toasts).

use crate::app::{self, log, state, AppState, Health};
use crate::theme::{theme, Theme, FONT_MONO, S2, S3, S4, TOP_H};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, floating, Kind};
use crate::workspace::{Handle, PaletteItem};
use gpui::{
    actions, div, prelude::*, px, AnyElement, App, Context, Entity, FocusHandle, Focusable, Global, KeyBinding,
    MouseButton, MouseMoveEvent, MouseUpEvent, Pixels, SharedString, Subscription, Window,
};
use std::time::Duration;

actions!(
    shell,
    [
        OpenPalette,
        ToggleNav,
        ToggleInspector,
        Search,
        NewItem,
        Save,
        Refresh,
        Connections,
        CloseOverlay,
        PaletteUp,
        PaletteDown,
        ConfirmAccept,
        ConfirmCancel,
        Ws1,
        Ws2,
        Ws3,
        Ws4,
        Ws5,
        Ws6,
        Ws7,
        Ws8,
        Ws9,
        CycleTheme,
        NextWs,
        PrevWs
    ]
);

pub fn bind_keys(cx: &mut App) {
    let s = Some("Shell");
    cx.bind_keys([
        KeyBinding::new("cmd-k", OpenPalette, s),
        KeyBinding::new("cmd-shift-p", OpenPalette, None),
        KeyBinding::new("cmd-\\", ToggleNav, s),
        KeyBinding::new("cmd-alt-i", ToggleInspector, s),
        KeyBinding::new("cmd-f", Search, s),
        KeyBinding::new("cmd-n", NewItem, s),
        KeyBinding::new("cmd-s", Save, None),
        KeyBinding::new("cmd-r", Refresh, s),
        KeyBinding::new("cmd-,", Connections, None),
        KeyBinding::new("escape", CloseOverlay, Some("Palette")),
        KeyBinding::new("enter", ConfirmAccept, Some("Confirm")),
        KeyBinding::new("escape", ConfirmCancel, Some("Confirm")),
        KeyBinding::new("up", PaletteUp, Some("Palette")),
        KeyBinding::new("down", PaletteDown, Some("Palette")),
        KeyBinding::new("cmd-1", Ws1, s),
        KeyBinding::new("cmd-2", Ws2, s),
        KeyBinding::new("cmd-3", Ws3, s),
        KeyBinding::new("cmd-4", Ws4, s),
        KeyBinding::new("cmd-5", Ws5, s),
        KeyBinding::new("cmd-6", Ws6, s),
        KeyBinding::new("cmd-7", Ws7, s),
        KeyBinding::new("cmd-8", Ws8, s),
        KeyBinding::new("cmd-9", Ws9, s),
        KeyBinding::new("ctrl-tab", NextWs, s),
        KeyBinding::new("ctrl-shift-tab", PrevWs, s),
        KeyBinding::new("cmd-alt-t", CycleTheme, s),
    ]);
}

/// A request for explicit confirmation (publish, unpublish, delete, revoke).
pub struct Confirm {
    pub title: SharedString,
    pub body: SharedString,
    pub action: SharedString,
    pub danger: bool,
    pub run: Box<dyn FnOnce(&mut Window, &mut App)>,
}

#[derive(Default)]
pub struct Overlay {
    pub confirm: Option<Confirm>,
    /// A workspace asked the shell to switch (palette items, deep links).
    pub goto: Option<String>,
    /// The profile changed: rebuild every workspace against the new session.
    pub reset: bool,
    /// Markdown another workspace wants placed at the caret of the open
    /// document (Blobs → "Insert into open document").
    pub insert: Option<String>,
}

impl Global for Overlay {}

/// Ask the person before doing something they can't take back.
pub fn confirm(
    cx: &mut App,
    title: impl Into<SharedString>,
    body: impl Into<SharedString>,
    action: impl Into<SharedString>,
    danger: bool,
    run: impl FnOnce(&mut Window, &mut App) + 'static,
) {
    cx.global_mut::<Overlay>().confirm =
        Some(Confirm { title: title.into(), body: body.into(), action: action.into(), danger, run: Box::new(run) });
    cx.refresh_windows();
}

pub fn goto(cx: &mut App, id: &str) {
    cx.global_mut::<Overlay>().goto = Some(id.into());
    cx.refresh_windows();
}

pub fn toast(cx: &mut App, text: impl Into<SharedString>, bad: bool) {
    let id = cx.global_mut::<AppState>().toast(text, bad);
    cx.refresh_windows();
    cx.spawn(async move |cx| {
        cx.background_executor().timer(Duration::from_secs(if bad { 8 } else { 4 })).await;
        let _ = cx.update(|cx| {
            cx.global_mut::<AppState>().toasts.retain(|t| t.id != id);
            cx.refresh_windows();
        });
    })
    .detach();
}

pub fn set_health(cx: &mut App, service: &str, h: Health) {
    let st = cx.global_mut::<AppState>();
    if st.health.get(service) != Some(&h) {
        st.health.insert(service.into(), h);
        cx.refresh_windows();
    }
}

type Factory = fn(&mut Window, &mut App) -> Handle;

pub struct Entry {
    pub id: &'static str,
    pub title: &'static str,
    /// The service whose health this workspace reflects.
    pub service: &'static str,
    pub make: Factory,
}

#[derive(Clone, Copy, PartialEq)]
enum Drag {
    Nav,
    Inspector,
}

pub struct Shell {
    focus: FocusHandle,
    entries: Vec<Entry>,
    open: Vec<Handle>,
    active: String,
    drag: Option<Drag>,
    palette: Option<Entity<TextField>>,
    palette_sel: usize,
    confirm_focus: FocusHandle,
    onboarding: Option<Entity<crate::ws::onboarding::Onboarding>>,
    /// Frames in a row that focus has sat on something not drawn.
    orphaned_focus: u8,
    _subs: Vec<Subscription>,
}

impl Shell {
    pub fn new(entries: Vec<Entry>, w: &mut Window, cx: &mut Context<Self>) -> Self {
        let active = state(cx).prefs.workspace.clone();
        let active = if entries.iter().any(|e| e.id == active) { active } else { entries[0].id.to_string() };
        let mut this = Shell {
            focus: cx.focus_handle(),
            entries,
            open: Vec::new(),
            active: String::new(),
            drag: None,
            palette: None,
            palette_sel: 0,
            confirm_focus: cx.focus_handle(),
            onboarding: None,
            orphaned_focus: 0,
            _subs: Vec::new(),
        };
        this.switch(&active, w, cx);
        this._subs.push(cx.observe_window_appearance(w, |_, w, cx| {
            let mode = state(cx).prefs.mode;
            let rm = state(cx).prefs.reduced_motion;
            let mut t = Theme::for_appearance(mode, w.appearance());
            t.reduced_motion = rm;
            cx.set_global(t);
            cx.refresh_windows();
        }));
        this.poll_health(cx);
        this
    }

    fn poll_health(&mut self, cx: &mut Context<Self>) {
        cx.spawn(async move |this, cx| loop {
            let Ok(session) = cx.update(|cx| app::session(cx)) else { break };
            let services: Vec<String> = farfield_core::registry::services().iter().map(|s| s.name.clone()).collect();
            for svc in services {
                let s = session.clone();
                let name = svc.clone();
                let has_key = session.has_credential(&svc);
                let probe = farfield_core::spawn(async move { farfield_core::api::status(&s, &name).await });
                let h = match probe.await {
                    Ok(Ok(_)) if has_key || farfield_core::api::probe_path(&svc).is_none() => Health::Up,
                    Ok(Ok(_)) => Health::NoAuth,
                    Ok(Err(e)) => Health::Down(e.to_string()),
                    Err(e) => Health::Down(e.to_string()),
                };
                // keep a NoAuth a workspace discovered (a refused key) until
                // the key changes; status alone can't see it
                let _ = cx.update(|cx| {
                    let prev = state(cx).health.get(&svc).cloned();
                    let next = match (prev, &h) {
                        (Some(Health::NoAuth), Health::Up) => Health::NoAuth,
                        _ => h.clone(),
                    };
                    if let (Some(Health::Up | Health::NoAuth), Health::Down(e)) = (state(cx).health.get(&svc), &next) {
                        log("outage", &[("service", &svc), ("error", e)]);
                    }
                    set_health(cx, &svc, next);
                });
            }
            if this.upgrade().is_none() {
                break;
            }
            cx.background_executor().timer(Duration::from_secs(20)).await;
        })
        .detach();
    }

    pub fn switch(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        if self.active == id {
            return;
        }
        if !self.open.iter().any(|h| h.id == id) {
            let Some(e) = self.entries.iter().find(|e| e.id == id) else { return };
            let h = (e.make)(w, cx);
            self.open.push(h);
        }
        self.active = id.into();
        // focus left in the workspace being hidden would leave keystrokes with
        // no path to the shell's bindings; take it back
        w.focus(&self.focus);
        cx.global_mut::<AppState>().prefs.workspace = id.into();
        state(cx).save_prefs();
        log("workspace", &[("id", id)]);
        cx.notify();
    }

    /// Profile switch: every workspace is rebuilt against the new session,
    /// so nothing from the previous profile's cache or drafts stays on screen.
    pub fn reset_workspaces(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let keep = self.active.clone();
        self.open.retain(|h| h.id == "connections");
        self.active.clear();
        self.switch(&keep, w, cx);
    }

    /// After handing a keystroke to a workspace, make sure focus landed on
    /// something drawn; a workspace that focuses a field it isn't showing
    /// (an empty state, a closed form) would otherwise strand the keyboard.
    fn reclaim_focus_soon(&self, w: &mut Window, cx: &mut Context<Self>) {
        cx.spawn_in(w, async move |this, cx| {
            cx.background_executor().timer(Duration::from_millis(80)).await;
            let _ = this.update_in(cx, |this, w, cx| {
                if !this.focus.contains_focused(w, cx)
                    && this.palette.is_none()
                    && cx.global::<Overlay>().confirm.is_none()
                {
                    w.focus(&this.focus);
                }
            });
        })
        .detach();
    }

    fn active_handle(&self) -> Option<&Handle> {
        self.open.iter().find(|h| h.id == self.active)
    }

    fn nth(&mut self, n: usize, w: &mut Window, cx: &mut Context<Self>) {
        if let Some(id) = self.entries.get(n).map(|e| e.id) {
            self.switch(id, w, cx);
        }
    }

    fn open_palette(&mut self, _: &OpenPalette, w: &mut Window, cx: &mut Context<Self>) {
        let f = cx.new(|cx| TextField::new(w, cx, "", "Go to, run, or search…"));
        cx.subscribe_in(&f, w, |this, _f, e: &FieldEvent, w, cx| match e {
            FieldEvent::Changed => {
                this.palette_sel = 0;
                cx.notify()
            }
            FieldEvent::Submit => this.run_palette(w, cx),
            FieldEvent::Cancel => this.close_palette(w, cx),
            FieldEvent::Up => {
                this.palette_sel = this.palette_sel.saturating_sub(1);
                cx.notify()
            }
            FieldEvent::Down => {
                this.palette_sel += 1;
                cx.notify()
            }
            FieldEvent::Blur => {}
        })
        .detach();
        f.read(cx).focus(w);
        self.palette = Some(f);
        self.palette_sel = 0;
        cx.notify();
    }

    fn close_palette(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.palette = None;
        w.focus(&self.focus);
        cx.notify();
    }

    fn palette_items(&self, cx: &mut Context<Self>) -> Vec<PaletteItem> {
        let mut items = Vec::new();
        for (i, e) in self.entries.iter().enumerate() {
            let id = e.id;
            items.push(PaletteItem::new(
                format!("Go to {}", e.title),
                if i < 9 { format!("⌘{}", i + 1) } else { String::new() },
                move |_, cx| goto(cx, id),
            ));
        }
        if let Some(h) = self.active_handle() {
            items.extend((h.palette)(cx));
        }
        for h in &self.open {
            if h.id != self.active {
                items.extend((h.palette)(cx));
            }
        }
        // the fleet's quick actions, from lib/capability's table
        for c in farfield_core::registry::commands() {
            let target = match c.name.as_str() {
                "feed" => "feed",
                "bm" => "bookmarks",
                "scrap" => "scrap",
                "qr" => "qr",
                "status" => "apex",
                "pulse" => "pulse",
                _ => continue,
            };
            items.push(PaletteItem::new(format!("/{} — {}", c.name, c.summary), c.usage.clone(), move |_, cx| {
                goto(cx, target)
            }));
        }
        items.push(PaletteItem::new("Theme: cycle system / light / dark", "⌘⌥T", cycle_theme));
        items.push(PaletteItem::new("Toggle reduced motion", "", |w, cx| {
            let st = cx.global_mut::<AppState>();
            st.prefs.reduced_motion = !st.prefs.reduced_motion;
            st.save_prefs();
            apply_theme(w, cx);
        }));
        items.push(PaletteItem::new("Settings", "⌘,", |_, cx| goto(cx, "connections")));
        items
    }

    fn filtered(&self, cx: &mut Context<Self>) -> Vec<PaletteItem> {
        let q = self.palette.as_ref().map(|f| f.read(cx).text()).unwrap_or_default().to_lowercase();
        let mut scored: Vec<(i32, PaletteItem)> = self
            .palette_items(cx)
            .into_iter()
            .filter_map(|it| fuzzy(&q, &it.title.to_lowercase()).map(|s| (s, it)))
            .collect();
        scored.sort_by_key(|s| std::cmp::Reverse(s.0));
        scored.into_iter().map(|(_, i)| i).take(40).collect()
    }

    fn run_palette(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let items = self.filtered(cx);
        if let Some(it) = items.get(self.palette_sel).cloned() {
            self.close_palette(w, cx);
            log("palette", &[("run", &it.title)]);
            (it.run)(w, cx);
            self.reclaim_focus_soon(w, cx);
        }
    }

    fn on_drag_move(&mut self, e: &MouseMoveEvent, w: &mut Window, cx: &mut Context<Self>) {
        let Some(d) = self.drag else { return };
        let width = w.viewport_size().width;
        let prefs = &mut cx.global_mut::<AppState>().prefs;
        match d {
            Drag::Nav => prefs.nav_width = f32::from(e.position.x).clamp(150.0, 360.0),
            Drag::Inspector => prefs.inspector_width = f32::from(width - e.position.x).clamp(220.0, 560.0),
        }
        cx.notify();
    }

    fn on_drag_end(&mut self, _: &MouseUpEvent, _: &mut Window, cx: &mut Context<Self>) {
        if self.drag.take().is_some() {
            state(cx).save_prefs();
        }
    }

    fn render_nav(&self, t: &Theme, cx: &mut Context<Self>) -> impl IntoElement {
        let health = state(cx).health.clone();
        let width = state(cx).prefs.nav_width;
        div()
            .id("nav")
            .flex()
            .flex_col()
            .w(px(width))
            .h_full()
            .flex_none()
            .bg(t.paper_2)
            .overflow_y_scroll()
            .py(S2)
            .children(self.entries.iter().enumerate().map(|(i, e)| {
                let selected = e.id == self.active;
                let h = health.get(e.service).cloned().unwrap_or(Health::Unknown);
                let dot = match h {
                    Health::Up => t.good,
                    Health::NoAuth => t.warn,
                    Health::Down(_) => t.bad,
                    _ => t.ink_3,
                };
                let dirty = self.open.iter().find(|o| o.id == e.id).map(|o| (o.dirty)(cx)).unwrap_or(false);
                let id = e.id;
                let ink = t.ink;
                let wash = t.wash;
                div()
                    .id(SharedString::from(format!("nav-{id}")))
                    .flex()
                    .items_center()
                    .gap(S2)
                    .mx(S2)
                    .px(S3)
                    .py(px(6.))
                    .rounded(px(4.))
                    .text_sm()
                    .cursor_pointer()
                    .text_color(if selected { t.ink } else { t.ink_2 })
                    .when(selected, |d| d.bg(t.accent_soft).font_weight(gpui::FontWeight::MEDIUM))
                    .when(!selected, move |d| d.hover(move |s| s.bg(wash).text_color(ink)))
                    .child(div().w(px(6.)).h(px(6.)).rounded_full().bg(dot))
                    .child(div().flex_1().child(e.title))
                    .when(dirty, |d| d.child(div().text_xs().text_color(t.signal).child("●")))
                    .when(i < 9, |d| {
                        d.child(div().text_xs().font_family(FONT_MONO).text_color(t.ink_3).child(format!("⌘{}", i + 1)))
                    })
                    .on_click(cx.listener(move |this, _, w, cx| this.switch(id, w, cx)))
            }))
    }

    fn render_status(&self, t: &Theme, cx: &mut Context<Self>) -> impl IntoElement {
        let st = state(cx);
        let profile = st.session.profile.name.clone();
        let down: Vec<String> =
            st.health.iter().filter(|(_, h)| matches!(h, Health::Down(_))).map(|(k, _)| k.clone()).collect();
        let noauth = st.health.values().filter(|h| matches!(h, Health::NoAuth)).count();
        let up = st.health.values().filter(|h| matches!(h, Health::Up)).count();
        div()
            .flex()
            .items_center()
            .gap(S4)
            .h(px(26.))
            .px(S4)
            .border_t_1()
            .border_color(t.rule)
            .text_xs()
            .text_color(t.ink_2)
            .child(div().font_family(FONT_MONO).child(profile))
            .child(ui::chip(format!("{up} online"), t.good, cx))
            .when(noauth > 0, |d| d.child(ui::chip(format!("{noauth} need a key"), t.warn, cx)))
            .when(!down.is_empty(), |d| d.child(ui::chip(format!("offline: {}", down.join(", ")), t.bad, cx)))
            .child(div().flex_1())
            .child("⌘K palette · ⌘F search · ⌘S save")
    }

    fn render_palette(&self, t: &Theme, cx: &mut Context<Self>) -> Option<AnyElement> {
        let f = self.palette.clone()?;
        let items = self.filtered(cx);
        let sel = self.palette_sel.min(items.len().saturating_sub(1));
        Some(
            div()
                .absolute()
                .size_full()
                .flex()
                .justify_center()
                .pt(px(90.))
                .bg(gpui::hsla(0., 0., 0., if t.dark { 0.35 } else { 0.12 }))
                .on_mouse_down(MouseButton::Left, cx.listener(|this, _, w, cx| this.close_palette(w, cx)))
                .child(
                    floating(cx)
                        .id("palette")
                        .key_context("Palette")
                        .on_action(cx.listener(|this, _: &CloseOverlay, w, cx| this.close_palette(w, cx)))
                        .on_action(cx.listener(|this, _: &PaletteUp, _, cx| {
                            this.palette_sel = this.palette_sel.saturating_sub(1);
                            cx.notify()
                        }))
                        .on_action(cx.listener(|this, _: &PaletteDown, _, cx| {
                            this.palette_sel += 1;
                            cx.notify()
                        }))
                        .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                        .w(px(560.))
                        .flex()
                        .flex_col()
                        .child(div().px(S4).pt(S3).pb(S2).child(f))
                        .child(
                            div()
                                .id("palette-items")
                                .flex()
                                .flex_col()
                                .max_h(px(400.))
                                .overflow_y_scroll()
                                .pb(S2)
                                .children(items.into_iter().enumerate().map(|(i, it)| {
                                    let run = it.run.clone();
                                    let title = it.title.clone();
                                    div()
                                        .id(("pi", i))
                                        .flex()
                                        .justify_between()
                                        .px(S4)
                                        .py(px(7.))
                                        .text_sm()
                                        .cursor_pointer()
                                        .text_color(t.ink)
                                        .when(i == sel, |d| d.bg(t.accent_soft))
                                        .child(it.title.clone())
                                        .child(
                                            div()
                                                .text_xs()
                                                .font_family(FONT_MONO)
                                                .text_color(t.ink_3)
                                                .child(it.subtitle.clone()),
                                        )
                                        .on_click(cx.listener(move |this, _, w, cx| {
                                            this.close_palette(w, cx);
                                            log("palette", &[("run", &title)]);
                                            (run)(w, cx);
                                        }))
                                })),
                        ),
                )
                .into_any_element(),
        )
    }

    fn render_confirm(&self, t: &Theme, cx: &mut Context<Self>) -> Option<AnyElement> {
        let c = cx.global::<Overlay>().confirm.as_ref()?;
        let (title, body, action, danger) = (c.title.clone(), c.body.clone(), c.action.clone(), c.danger);
        Some(
            div()
                .absolute()
                .size_full()
                .flex()
                .items_center()
                .justify_center()
                .bg(gpui::hsla(0., 0., 0., if t.dark { 0.4 } else { 0.16 }))
                .child(
                    floating(cx)
                        .id("confirm")
                        .key_context("Confirm")
                        .track_focus(&self.confirm_focus)
                        .on_action(cx.listener(|this, _: &ConfirmAccept, w, cx| {
                            if let Some(c) = cx.global_mut::<Overlay>().confirm.take() {
                                log("confirmed", &[("what", &c.title)]);
                                (c.run)(w, cx);
                            }
                            w.focus(&this.focus);
                            cx.refresh_windows();
                        }))
                        .on_action(cx.listener(|this, _: &ConfirmCancel, w, cx| {
                            cx.global_mut::<Overlay>().confirm = None;
                            w.focus(&this.focus);
                            cx.refresh_windows();
                        }))
                        .w(px(440.))
                        .p(px(22.))
                        .flex()
                        .flex_col()
                        .gap(S3)
                        .child(div().text_base().font_weight(gpui::FontWeight::SEMIBOLD).text_color(t.ink).child(title))
                        .child(div().text_sm().text_color(t.ink_2).child(body))
                        .child(div().text_xs().text_color(t.ink_3).child("Enter to confirm · Esc to cancel"))
                        .child(
                            div()
                                .flex()
                                .justify_end()
                                .gap(S2)
                                .pt(S2)
                                .child(ui::button("confirm-cancel", "Cancel", Kind::Quiet, cx, |_, _, cx| {
                                    cx.global_mut::<Overlay>().confirm = None;
                                    cx.refresh_windows();
                                }))
                                .child(ui::button(
                                    "confirm-go",
                                    action,
                                    if danger { Kind::Danger } else { Kind::Primary },
                                    cx,
                                    |_, w, cx| {
                                        if let Some(c) = cx.global_mut::<Overlay>().confirm.take() {
                                            log("confirmed", &[("what", &c.title)]);
                                            (c.run)(w, cx);
                                        }
                                        cx.refresh_windows();
                                    },
                                )),
                        ),
                )
                .into_any_element(),
        )
    }
}

/// Subsequence match with a bonus for contiguous and word-start hits.
pub fn fuzzy(q: &str, s: &str) -> Option<i32> {
    if q.is_empty() {
        return Some(0);
    }
    let mut score = 0;
    let mut last: Option<usize> = None;
    let chars: Vec<char> = s.chars().collect();
    let mut i = 0;
    for qc in q.chars() {
        let mut found = None;
        while i < chars.len() {
            if chars[i] == qc {
                found = Some(i);
                i += 1;
                break;
            }
            i += 1;
        }
        let at = found?;
        score += 1;
        if last.is_some_and(|l| l + 1 == at) {
            score += 3;
        }
        if at == 0 || chars.get(at.wrapping_sub(1)).is_some_and(|c| !c.is_alphanumeric()) {
            score += 2;
        }
        last = Some(at);
    }
    Some(score * 10 - s.len() as i32 / 8)
}

pub fn apply_theme(w: &mut Window, cx: &mut App) {
    let p = &state(cx).prefs;
    let mut t = Theme::for_appearance(p.mode, w.appearance());
    t.reduced_motion = p.reduced_motion;
    cx.set_global(t);
    cx.refresh_windows();
}

fn cycle_theme(w: &mut Window, cx: &mut App) {
    use crate::theme::Mode;
    let st = cx.global_mut::<AppState>();
    st.prefs.mode = match st.prefs.mode {
        Mode::System => Mode::Light,
        Mode::Light => Mode::Dark,
        Mode::Dark => Mode::System,
    };
    st.save_prefs();
    apply_theme(w, cx);
}

impl Render for Shell {
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        // first run (or "Run setup again"): the onboarding owns the window
        if !state(cx).prefs.onboarded {
            let ob = match &self.onboarding {
                Some(o) => o.clone(),
                None => {
                    let o = cx.new(|cx| crate::ws::onboarding::Onboarding::new(w, cx));
                    self.onboarding = Some(o.clone());
                    o
                }
            };
            // the shell's own root is not drawn during onboarding: hand focus over
            if w.focused(cx).is_none_or(|f| f == self.focus) {
                let h = ob.read(cx).focus_handle(cx);
                w.focus(&h);
            }
            let t = theme(cx).clone();
            let toasts = state(cx).toasts.clone();
            return div()
                .size_full()
                .child(ob)
                .child(div().absolute().bottom(px(24.)).right(px(16.)).flex().flex_col().gap(S2).children(
                    toasts.into_iter().map(|to| {
                        floating(cx)
                            .px(S4)
                            .py(px(9.))
                            .max_w(px(420.))
                            .text_sm()
                            .text_color(if to.bad { t.bad } else { t.ink })
                            .child(to.text.clone())
                    }),
                ))
                .into_any_element();
        }
        self.onboarding = None;
        // Focus on an element that is no longer drawn (a field in a hidden
        // panel, a form that closed) strands the keyboard: no shortcut can
        // reach the shell. If it stays stranded past a frame (a field created
        // this frame is not in the tree until the next), take focus back.
        if !self.focus.contains_focused(w, cx) && cx.global::<Overlay>().confirm.is_none() && self.palette.is_none() {
            self.orphaned_focus = self.orphaned_focus.saturating_add(1);
            if self.orphaned_focus > 2 {
                w.focus(&self.focus);
                self.orphaned_focus = 0;
            } else {
                cx.notify();
            }
        } else {
            self.orphaned_focus = 0;
        }
        // a confirmation takes the keyboard: Enter confirms, Esc cancels
        if cx.global::<Overlay>().confirm.is_some() && !self.confirm_focus.is_focused(w) {
            w.focus(&self.confirm_focus);
        }
        if std::mem::take(&mut cx.global_mut::<Overlay>().reset) {
            self.reset_workspaces(w, cx);
        }
        if let Some(id) = cx.global_mut::<Overlay>().goto.take() {
            self.switch(&id, w, cx);
        }
        let t = theme(cx).clone();
        let prefs = state(cx).prefs.clone();
        let view = self.active_handle().map(|h| h.view.clone());
        let inspector = if prefs.inspector_open {
            self.open.iter().find(|h| h.id == self.active).and_then(|h| (h.inspector)(w, cx))
        } else {
            None
        };
        let title = self.active_handle().map(|h| h.title).unwrap_or("");
        let toasts = state(cx).toasts.clone();
        let dragging = self.drag.is_some();

        div()
            .id("shell")
            .key_context("Shell")
            .track_focus(&self.focus)
            .size_full()
            .flex()
            .flex_col()
            .bg(t.paper)
            .text_color(t.ink)
            .font_family(crate::theme::FONT_UI)
            .text_size(px(14.))
            .on_action(cx.listener(Self::open_palette))
            .on_action(cx.listener(|_, _: &ToggleNav, _, cx| {
                let p = &mut cx.global_mut::<AppState>().prefs;
                p.nav_open = !p.nav_open;
                state(cx).save_prefs();
                cx.notify()
            }))
            .on_action(cx.listener(|_, _: &ToggleInspector, _, cx| {
                let p = &mut cx.global_mut::<AppState>().prefs;
                p.inspector_open = !p.inspector_open;
                state(cx).save_prefs();
                cx.notify()
            }))
            .on_action(cx.listener(|this, _: &Search, w, cx| {
                if let Some(h) = this.active_handle() {
                    (h.focus_search)(w, cx)
                }
                this.reclaim_focus_soon(w, cx);
            }))
            .on_action(cx.listener(|this, _: &NewItem, w, cx| {
                if let Some(h) = this.active_handle() {
                    (h.new_item)(w, cx)
                }
                this.reclaim_focus_soon(w, cx);
            }))
            .on_action(cx.listener(|this, _: &Save, w, cx| {
                if let Some(h) = this.active_handle() {
                    (h.save)(w, cx)
                }
            }))
            .on_action(cx.listener(|this, _: &Refresh, w, cx| {
                if let Some(h) = this.active_handle() {
                    (h.refresh)(w, cx)
                }
            }))
            .on_action(cx.listener(|this, _: &Connections, w, cx| this.switch("connections", w, cx)))
            .on_action(cx.listener(|_, _: &CycleTheme, w, cx| cycle_theme(w, cx)))
            .on_action(cx.listener(|this, _: &NextWs, w, cx| {
                let i = this.entries.iter().position(|e| e.id == this.active).unwrap_or(0);
                this.nth((i + 1) % this.entries.len(), w, cx)
            }))
            .on_action(cx.listener(|this, _: &PrevWs, w, cx| {
                let i = this.entries.iter().position(|e| e.id == this.active).unwrap_or(0);
                this.nth((i + this.entries.len() - 1) % this.entries.len(), w, cx)
            }))
            .on_action(cx.listener(|this, _: &Ws1, w, cx| this.nth(0, w, cx)))
            .on_action(cx.listener(|this, _: &Ws2, w, cx| this.nth(1, w, cx)))
            .on_action(cx.listener(|this, _: &Ws3, w, cx| this.nth(2, w, cx)))
            .on_action(cx.listener(|this, _: &Ws4, w, cx| this.nth(3, w, cx)))
            .on_action(cx.listener(|this, _: &Ws5, w, cx| this.nth(4, w, cx)))
            .on_action(cx.listener(|this, _: &Ws6, w, cx| this.nth(5, w, cx)))
            .on_action(cx.listener(|this, _: &Ws7, w, cx| this.nth(6, w, cx)))
            .on_action(cx.listener(|this, _: &Ws8, w, cx| this.nth(7, w, cx)))
            .on_action(cx.listener(|this, _: &Ws9, w, cx| this.nth(8, w, cx)))
            .on_mouse_move(cx.listener(Self::on_drag_move))
            .on_mouse_up(MouseButton::Left, cx.listener(Self::on_drag_end))
            .when(dragging, |d| d.cursor_col_resize())
            // top bar: the wordmark, where you are, the palette
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(S4)
                    .h(TOP_H)
                    .pl(px(84.)) // clear the traffic lights
                    .pr(S4)
                    .border_b_1()
                    .border_color(t.rule)
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap(px(7.))
                            .child(div().w(px(9.)).h(px(9.)).rounded_full().bg(t.signal))
                            .child(div().text_sm().font_weight(gpui::FontWeight::SEMIBOLD).child("farfield")),
                    )
                    .child(div().text_sm().text_color(t.ink_2).child(title))
                    .child(div().flex_1())
                    .child(
                        div()
                            .id("palette-open")
                            .flex()
                            .items_center()
                            .gap(S2)
                            .px(S3)
                            .py(px(4.))
                            .w(px(260.))
                            .border_b_1()
                            .border_color(t.rule_strong)
                            .text_sm()
                            .text_color(t.ink_3)
                            .cursor_pointer()
                            .child(div().flex_1().child("Search or run a command"))
                            .child(div().font_family(FONT_MONO).text_xs().child("⌘K"))
                            .on_click(cx.listener(|this, _, w, cx| this.open_palette(&OpenPalette, w, cx))),
                    ),
            )
            .child(
                div()
                    .flex()
                    .flex_1()
                    .min_h_0()
                    .when(prefs.nav_open, |d| {
                        d.child(self.render_nav(&t, cx)).child(
                            div()
                                .id("nav-resize")
                                .w(px(5.))
                                .h_full()
                                .cursor_col_resize()
                                .border_l_1()
                                .border_color(t.rule)
                                .hover(|s| s.bg(t.wash))
                                .on_mouse_down(
                                    MouseButton::Left,
                                    cx.listener(|this, _, _, cx| {
                                        this.drag = Some(Drag::Nav);
                                        cx.notify()
                                    }),
                                ),
                        )
                    })
                    .child(div().flex_1().min_w_0().h_full().children(view))
                    .when_some(inspector, |d, insp| {
                        d.child(
                            div()
                                .id("insp-resize")
                                .w(px(5.))
                                .h_full()
                                .cursor_col_resize()
                                .border_r_1()
                                .border_color(t.rule)
                                .hover(|s| s.bg(t.wash))
                                .on_mouse_down(
                                    MouseButton::Left,
                                    cx.listener(|this, _, _, cx| {
                                        this.drag = Some(Drag::Inspector);
                                        cx.notify()
                                    }),
                                ),
                        )
                        .child(
                            div()
                                .id("inspector")
                                .w(px(prefs.inspector_width))
                                .flex_none()
                                .h_full()
                                .overflow_y_scroll()
                                .px(S4)
                                .py(S3)
                                .child(insp),
                        )
                    }),
            )
            .child(self.render_status(&t, cx))
            .children(self.render_palette(&t, cx))
            .children(self.render_confirm(&t, cx))
            .child(div().absolute().bottom(px(36.)).right(px(16.)).flex().flex_col().gap(S2).children(
                toasts.into_iter().map(|to| {
                    floating(cx)
                        .px(S4)
                        .py(px(9.))
                        .max_w(px(420.))
                        .text_sm()
                        .text_color(if to.bad { t.bad } else { t.ink })
                        .child(to.text.clone())
                }),
            ))
            .into_any_element()
    }
}

impl Focusable for Shell {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}

#[allow(dead_code)]
fn _px(p: Pixels) -> f32 {
    f32::from(p)
}

#[cfg(test)]
mod tests {
    use super::fuzzy;

    #[test]
    fn palette_ranks_the_named_target_first() {
        let q = "go to switchboard";
        let sw = fuzzy(q, "go to switchboard").unwrap();
        assert!(fuzzy(q, "go to pulse").is_none_or(|p| p < sw), "pulse outranks switchboard");
        assert!(fuzzy("go to keys", "go to keys").unwrap() > fuzzy("go to keys", "settings: keys").unwrap_or(i32::MIN));
    }
}
