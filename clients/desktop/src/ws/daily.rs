//! Daily: the day's artifact — NASA's photograph as the hero, the explanation
//! in document type, the generative plate beneath it — with day-by-day
//! navigation (←/→) and the archive as a calm grid. Read-only.
//!
//! Photos are third-party http(s) URLs: fetched off-thread without any
//! credential (core's `remote_image`, ≤6 at a time), decoded and downscaled
//! off the UI thread, and kept in a bounded LRU. The plate is SVG, rasterized
//! with resvg off-thread at 3× its view box.

use crate::app::{self, describe, log, Health};
use crate::shell::{set_health, toast};
use crate::theme::{theme, FONT_DOC, FONT_MONO, MEASURE, S1, S2, S3, S4, S5, S6};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::Workspace;
use farfield_core::api::daily::{self, Archive, Day, Photo};
use farfield_core::api::ext_observe::daily as daily_x;
use farfield_core::api::ext_observe::daily::Art;
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{
    div, img, prelude::*, px, AnyElement, App, Context, FocusHandle, KeyDownEvent, ObjectFit, RenderImage,
    SharedString, StyledImage, Window,
};
use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, LazyLock};

/// Long edge for the hero and for archive thumbnails.
const HERO_EDGE: u32 = 1800;
const THUMB_EDGE: u32 = 480;
/// The plate's raster width (its view box is 500 wide).
const PLATE_W: u32 = 1500;
const CACHE_ENTRIES: usize = 200;
const CACHE_BYTES: usize = 320 << 20;

#[derive(Clone, Copy, PartialEq)]
enum View {
    Day,
    Archive,
}

// ── a bounded image cache ────────────────────────────────────────────────

#[derive(Clone)]
struct Pic {
    img: Arc<RenderImage>,
    w: u32,
    h: u32,
}

enum Slot {
    Loading,
    Ready(Pic),
    Failed(String),
}

#[derive(Default)]
struct Gallery {
    slots: HashMap<String, Slot>,
    order: VecDeque<String>,
    bytes: usize,
    /// Evicted images whose GPU textures still need dropping (needs a Window).
    retired: Vec<Arc<RenderImage>>,
}

impl Gallery {
    fn touch(&mut self, k: &str) {
        if let Some(i) = self.order.iter().position(|x| x == k) {
            let k = self.order.remove(i).unwrap();
            self.order.push_back(k);
        }
    }

    fn get(&mut self, k: &str) -> Option<&Slot> {
        if self.slots.contains_key(k) {
            self.touch(k);
        }
        self.slots.get(k)
    }

    fn put(&mut self, k: String, s: Slot) {
        if let Some(Slot::Ready(p)) = self.slots.remove(&k) {
            self.bytes -= (p.w * p.h * 4) as usize;
            self.retired.push(p.img);
        }
        if let Slot::Ready(p) = &s {
            self.bytes += (p.w * p.h * 4) as usize;
        }
        self.order.retain(|x| x != &k);
        self.order.push_back(k.clone());
        self.slots.insert(k, s);
        while self.order.len() > CACHE_ENTRIES || self.bytes > CACHE_BYTES {
            let Some(old) = self.order.pop_front() else { break };
            if let Some(Slot::Ready(p)) = self.slots.remove(&old) {
                self.bytes -= (p.w * p.h * 4) as usize;
                self.retired.push(p.img);
            }
        }
    }
}

/// Decode and downscale to BGRA (what RenderImage wants).
fn decode(bytes: &[u8], max_edge: u32) -> Result<Pic, String> {
    let im = image::load_from_memory(bytes).map_err(|e| format!("not an image this app can show ({e})"))?;
    let im = if im.width().max(im.height()) > max_edge {
        im.resize(max_edge, max_edge, image::imageops::FilterType::Triangle)
    } else {
        im
    };
    let mut rgba = im.into_rgba8();
    for p in rgba.as_chunks_mut::<4>().0 {
        p.swap(0, 2);
    }
    let (w, h) = rgba.dimensions();
    Ok(Pic { img: Arc::new(RenderImage::new(smallvec::smallvec![image::Frame::new(rgba)])), w, h })
}

/// Rasterize the plate SVG. Its text uses the system monospace faces, so the
/// font database is the system's, loaded once.
fn rasterize(svg: &[u8], width: u32) -> Result<Pic, String> {
    use resvg::{tiny_skia, usvg};
    static FONTS: LazyLock<Arc<usvg::fontdb::Database>> = LazyLock::new(|| {
        let mut db = usvg::fontdb::Database::new();
        db.load_system_fonts();
        Arc::new(db)
    });
    let opt = usvg::Options { fontdb: FONTS.clone(), ..Default::default() };
    let tree = usvg::Tree::from_data(svg, &opt).map_err(|e| format!("plate: {e}"))?;
    let size = tree.size();
    let scale = width as f32 / size.width();
    let h = (size.height() * scale).ceil() as u32;
    let mut pm = tiny_skia::Pixmap::new(width, h).ok_or("plate has no size")?;
    resvg::render(&tree, tiny_skia::Transform::from_scale(scale, scale), &mut pm.as_mut());
    let mut data = pm.take();
    // premultiplied RGBA → BGRA; the plate's background is opaque
    for p in data.as_chunks_mut::<4>().0 {
        p.swap(0, 2);
    }
    let buf = image::ImageBuffer::from_raw(width, h, data).ok_or("plate buffer")?;
    Ok(Pic { img: Arc::new(RenderImage::new(smallvec::smallvec![image::Frame::new(buf)])), w: width, h })
}

fn parse_hex(c: &str) -> Option<gpui::Hsla> {
    let n = u32::from_str_radix(c.trim().trim_start_matches('#'), 16).ok()?;
    Some(gpui::rgb(n).into())
}

/// APOD marks some clips as "image"; the URL tells the truth.
fn is_video(p: &Photo) -> bool {
    let u = p.image_url.to_ascii_lowercase();
    p.media_type == "video" || [".mp4", ".mov", ".webm"].iter().any(|e| u.ends_with(e)) || u.contains("youtube.com/")
}

// ── the workspace ────────────────────────────────────────────────────────

pub struct DailyWs {
    focus: FocusHandle,
    /// Take the keyboard once the first day has loaded (the shell refocuses
    /// itself after the first paint, so focusing earlier loses on cold launch).
    grabbed: bool,
    view: View,
    /// The day asked for; None is "today" (the newest the index has).
    want: Option<String>,
    day: Option<Day>,
    art: Option<Art>,
    loading: bool,
    freshness: Option<Freshness>,
    error: Option<String>,
    latest: Arc<Latest>,
    archive: Option<Archive>,
    page: u32,
    archive_loading: bool,
    archive_error: Option<String>,
    archive_latest: Arc<Latest>,
    gallery: Gallery,
}

impl DailyWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let focus = cx.focus_handle();
        // take the keyboard on arrival so ←/→ work straight away
        w.focus(&focus);
        let mut this = DailyWs {
            focus,
            grabbed: false,
            view: View::Day,
            want: None,
            day: None,
            art: None,
            loading: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            archive: None,
            page: 1,
            archive_loading: false,
            archive_error: None,
            archive_latest: Arc::new(Latest::default()),
            gallery: Gallery::default(),
        };
        this.load_day(None, cx);
        this
    }

    fn load_day(&mut self, date: Option<String>, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        self.want = date.clone();
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move {
            let day = match &date {
                Some(d) => daily::day(&s, d).await?,
                None => daily::today(&s).await?,
            };
            // the plate for the photograph's own date
            let art = daily_x::art(&s, Some(&day.value.photo.date)).await.ok().map(|a| a.value);
            Ok::<_, ApiError>((day, art))
        });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                match r {
                    Ok(Ok((day, art))) => {
                        let h = match &day.freshness {
                            Freshness::Live => Health::Up,
                            Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                        };
                        set_health(cx, "daily", h);
                        log("daily-day", &[("date", &day.value.photo.date)]);
                        this.freshness = Some(day.freshness);
                        this.day = Some(day.value);
                        this.art = art;
                        this.error = None;
                    }
                    Ok(Err(ApiError::NotFound)) => {
                        this.error = Some(match &this.want {
                            Some(d) => format!("The index has no photograph for {d}."),
                            None => "The index has no photograph yet — NASA may not have posted today's.".into(),
                        });
                    }
                    Ok(Err(e)) => {
                        if e.is_offline() {
                            set_health(cx, "daily", Health::Down(e.to_string()));
                        }
                        this.error = Some(describe(&e));
                    }
                    Err(e) => this.error = Some(e.to_string()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn load_archive(&mut self, page: u32, cx: &mut Context<Self>) {
        let ticket = self.archive_latest.ticket();
        let latest = self.archive_latest.clone();
        let s = app::session(cx);
        self.page = page.max(1);
        self.archive_loading = true;
        cx.notify();
        let p = self.page;
        let task = farfield_core::spawn(async move { daily::archive(&s, p).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.archive_loading = false;
                match r {
                    Ok(Ok(a)) => {
                        log("daily-archive", &[("page", &a.value.page.to_string())]);
                        this.archive = Some(a.value);
                        this.archive_error = None;
                    }
                    Ok(Err(e)) => this.archive_error = Some(describe(&e)),
                    Err(e) => this.archive_error = Some(e.to_string()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    /// The image under `key`, starting its fetch if it isn't cached.
    fn remote(&mut self, url: &str, edge: u32, cx: &mut Context<Self>) -> Option<Result<Pic, String>> {
        let key = format!("{edge}:{url}");
        match self.gallery.get(&key) {
            Some(Slot::Ready(p)) => return Some(Ok(p.clone())),
            Some(Slot::Failed(e)) => return Some(Err(e.clone())),
            Some(Slot::Loading) => return None,
            None => {}
        }
        self.gallery.put(key.clone(), Slot::Loading);
        let u = url.to_string();
        let task = farfield_core::spawn(async move { daily_x::remote_image(&u).await });
        cx.spawn(async move |this, cx| {
            let r = match task.await {
                Ok(Ok(bytes)) => {
                    let decoded = cx.background_executor().spawn(async move { decode(&bytes, edge) }).await;
                    decoded.map_err(|e| e.to_string())
                }
                Ok(Err(e)) => Err(describe(&e)),
                Err(e) => Err(e.to_string()),
            };
            let _ = this.update(cx, |this, cx| {
                this.gallery.put(
                    key,
                    match r {
                        Ok(p) => Slot::Ready(p),
                        Err(e) => Slot::Failed(e),
                    },
                );
                cx.notify();
            });
        })
        .detach();
        None
    }

    fn plate(&mut self, date: &str, cx: &mut Context<Self>) -> Option<Result<Pic, String>> {
        let key = format!("plate:{date}");
        match self.gallery.get(&key) {
            Some(Slot::Ready(p)) => return Some(Ok(p.clone())),
            Some(Slot::Failed(e)) => return Some(Err(e.clone())),
            Some(Slot::Loading) => return None,
            None => {}
        }
        self.gallery.put(key.clone(), Slot::Loading);
        let s = app::session(cx);
        let d = date.to_string();
        let task = farfield_core::spawn(async move { daily_x::art_svg(&s, Some(&d)).await });
        cx.spawn(async move |this, cx| {
            let r = match task.await {
                Ok(Ok(svg)) => cx.background_executor().spawn(async move { rasterize(&svg, PLATE_W) }).await,
                Ok(Err(e)) => Err(describe(&e)),
                Err(e) => Err(e.to_string()),
            };
            let _ = this.update(cx, |this, cx| {
                if let Ok(p) = &r {
                    log("daily-plate", &[("w", &p.w.to_string()), ("h", &p.h.to_string())]);
                }
                this.gallery.put(
                    key,
                    match r {
                        Ok(p) => Slot::Ready(p),
                        Err(e) => Slot::Failed(e),
                    },
                );
                cx.notify();
            });
        })
        .detach();
        None
    }

    fn step(&mut self, forward: bool, cx: &mut Context<Self>) {
        match self.view {
            View::Day => {
                let Some(d) = &self.day else { return };
                let to = if forward { d.next.clone() } else { d.prev.clone() };
                if to.is_empty() {
                    toast(
                        cx,
                        if forward { "That's the newest day." } else { "That's the first day in the index." },
                        false,
                    );
                    return;
                }
                self.load_day(Some(to), cx);
            }
            View::Archive => {
                // → goes further back in time (the next page), ← towards today
                let pages = self.archive.as_ref().map(|a| a.pages).unwrap_or(1) as u32;
                let p = if forward { (self.page + 1).min(pages.max(1)) } else { self.page.saturating_sub(1).max(1) };
                if p != self.page {
                    self.load_archive(p, cx);
                }
            }
        }
    }

    fn set_view(&mut self, v: View, cx: &mut Context<Self>) {
        self.view = v;
        if v == View::Archive && self.archive.is_none() {
            self.load_archive(1, cx);
        }
        cx.notify();
    }

    fn open_day(&mut self, date: String, cx: &mut Context<Self>) {
        self.view = View::Day;
        self.load_day(Some(date), cx);
    }

    fn on_key(&mut self, e: &KeyDownEvent, _w: &mut Window, cx: &mut Context<Self>) {
        if e.keystroke.modifiers.modified() {
            return;
        }
        match e.keystroke.key.as_str() {
            "left" => self.step(false, cx),
            "right" => self.step(true, cx),
            "t" => self.load_day(None, cx),
            "a" => self.set_view(if self.view == View::Archive { View::Day } else { View::Archive }, cx),
            _ => return,
        }
        cx.stop_propagation();
    }

    fn public(&self, cx: &App, path: &str) -> Option<String> {
        app::session(cx).public_base("daily").map(|b| format!("{}{}", b.trim_end_matches('/'), path))
    }

    // ── rendering ────────────────────────────────────────────────────────

    fn render_bar(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let tab = |id: &'static str, label: &'static str, on: bool| {
            div()
                .id(id)
                .px(S2)
                .py(px(3.))
                .text_sm()
                .cursor_pointer()
                .border_b_2()
                .border_color(if on { t.accent } else { gpui::transparent_black() })
                .text_color(if on { t.ink } else { t.ink_2 })
                .child(label)
        };
        let (prev, next) =
            self.day.as_ref().map(|d| (!d.prev.is_empty(), !d.next.is_empty())).unwrap_or((false, false));
        let in_day = self.view == View::Day;
        let pages = self.archive.as_ref().map(|a| a.pages).unwrap_or(1) as u32;
        let (can_back, can_fwd) = if in_day { (prev, next) } else { (self.page > 1, self.page < pages) };
        let e1 = cx.entity();
        let e2 = cx.entity();
        let e3 = cx.entity();
        let label = if in_day {
            self.day.as_ref().map(|d| d.photo.date.clone()).unwrap_or_default()
        } else {
            self.archive
                .as_ref()
                .map(|a| format!("page {} of {} · {} days", a.page, a.pages, a.total))
                .unwrap_or_default()
        };
        div()
            .flex()
            .items_center()
            .gap(S2)
            .px(S5)
            .py(S2)
            .border_b_1()
            .border_color(t.rule)
            .child(tab("v-day", "Day", in_day).on_click(cx.listener(|this, _, _, cx| this.set_view(View::Day, cx))))
            .child(
                tab("v-archive", "Archive", !in_day)
                    .on_click(cx.listener(|this, _, _, cx| this.set_view(View::Archive, cx))),
            )
            .child(div().flex_1())
            .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_2).child(label))
            .child(div().w(S3))
            .child(if can_back {
                ui::button(
                    "back",
                    if in_day { "←  Earlier" } else { "←  Newer" },
                    BtnKind::Quiet,
                    cx,
                    move |_, _, cx| e1.update(cx, |this, cx| this.step(false, cx)),
                )
                .into_any_element()
            } else {
                ui::button_disabled(if in_day { "←  Earlier" } else { "←  Newer" }, cx).into_any_element()
            })
            .child(ui::button("today", "Today", BtnKind::Quiet, cx, move |_, _, cx| {
                e2.update(cx, |this, cx| {
                    this.view = View::Day;
                    this.load_day(None, cx)
                })
            }))
            .child(if can_fwd {
                ui::button(
                    "fwd",
                    if in_day { "Later  →" } else { "Older  →" },
                    BtnKind::Quiet,
                    cx,
                    move |_, _, cx| e3.update(cx, |this, cx| this.step(true, cx)),
                )
                .into_any_element()
            } else {
                ui::button_disabled(if in_day { "Later  →" } else { "Older  →" }, cx).into_any_element()
            })
            .into_any_element()
    }

    fn status_line(&self, cx: &App) -> Option<AnyElement> {
        let t = theme(cx);
        match (&self.error, &self.freshness) {
            (Some(e), _) if self.day.is_some() => Some(ui::notice(e.clone(), t.bad, cx).into_any_element()),
            (None, Some(Freshness::Stale { age_ms, .. })) => Some(
                ui::notice(
                    format!("Offline — showing the copy loaded {} ago.", crate::ws::content::ago(*age_ms)),
                    t.warn,
                    cx,
                )
                .into_any_element(),
            ),
            _ => None,
        }
    }

    fn render_hero(&mut self, p: &Photo, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let video = is_video(p);
        let src = if video {
            p.thumb_url.clone()
        } else if !p.image_url.is_empty() {
            p.image_url.clone()
        } else {
            p.thumb_url.clone()
        };
        let frame = |child: AnyElement| div().w_full().flex().justify_center().child(child).into_any_element();
        let placeholder = |msg: String, t: &crate::theme::Theme| {
            div()
                .w_full()
                .h(px(380.))
                .bg(t.wash)
                .rounded(px(2.))
                .flex()
                .items_center()
                .justify_center()
                .text_sm()
                .text_color(t.ink_2)
                .child(msg)
                .into_any_element()
        };
        if src.is_empty() {
            let msg = if video {
                "Today's APOD is a video — open the source to watch it."
            } else {
                "No image for this day."
            };
            return placeholder(msg.into(), &t);
        }
        match self.remote(&src, HERO_EDGE, cx) {
            Some(Ok(pic)) => {
                let aspect = pic.w as f32 / pic.h.max(1) as f32;
                // tall images are held to a reading height; wide ones take the width
                // explicit sizes: a held height, the width following the aspect (contained)
                let h = if aspect >= 1.6 { 520. } else { 600. };
                let hero = img(pic.img.clone()).object_fit(ObjectFit::Contain).h(px(h)).w(px(h * aspect)).max_w_full();
                frame(hero.into_any_element())
            }
            // a compact note, not a big empty frame: the words and the plate still stand
            Some(Err(e)) => ui::notice(
                format!(
                    "The photograph didn't load — {}. \"Open source\" in the inspector shows it on NASA's site.",
                    e.trim_start_matches("unexpected response: ")
                ),
                t.warn,
                cx,
            )
            .into_any_element(),
            None => placeholder("Loading the photograph…".into(), &t),
        }
    }

    fn render_day(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let Some(day) = self.day.clone() else {
            return match &self.error {
                Some(e) => div()
                    .p(S6)
                    .flex()
                    .flex_col()
                    .gap(S3)
                    .child(
                        div().font_family(FONT_DOC).text_size(px(22.)).text_color(t.ink).child("Nothing to show yet."),
                    )
                    .child(div().text_sm().text_color(t.ink_2).max_w(px(520.)).child(e.clone()))
                    .child(
                        div()
                            .text_sm()
                            .text_color(t.ink_3)
                            .child("⌘R tries again; the archive (A) may still have earlier days."),
                    )
                    .into_any_element(),
                None => ui::quiet_state("Fetching today's photograph…", cx).into_any_element(),
            };
        };
        let p = day.photo.clone();
        let hero = self.render_hero(&p, cx);
        let plate = self.plate(&p.date, cx);
        let art = self.art.clone().filter(|a| a.date == p.date);

        let mut col = div().flex().flex_col().gap(S4).px(S6).pt(S5).pb(px(64.)).child(hero);
        col = col.child(
            div()
                .flex()
                .flex_col()
                .gap(S2)
                .max_w(MEASURE)
                .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(format!(
                    "{}{}",
                    p.date,
                    if p.placeholder { " · placeholder — NASA was unavailable" } else { "" }
                )))
                .child(
                    div()
                        .font_family(FONT_DOC)
                        .text_size(px(32.))
                        .line_height(px(38.))
                        .text_color(t.ink)
                        .child(if p.title.is_empty() { "Untitled".to_string() } else { p.title.clone() }),
                )
                .when(!p.credit.is_empty(), |d| {
                    d.child(div().text_sm().text_color(t.ink_2).child(format!("Credit: {}", p.credit.trim())))
                }),
        );
        if !p.explanation.is_empty() {
            col = col.child(
                div()
                    .max_w(MEASURE)
                    .font_family(FONT_DOC)
                    .text_size(px(18.))
                    .line_height(px(29.))
                    .text_color(t.ink)
                    .child(p.explanation.clone()),
            );
        }
        // the plate
        col = col.child(div().pt(S4).child(ui::rule(cx)));
        let plate_el = match plate {
            Some(Ok(pic)) => img(pic.img.clone())
                .w(px(560.))
                .h(px(560. * pic.h as f32 / pic.w.max(1) as f32))
                .object_fit(ObjectFit::Contain)
                .into_any_element(),
            Some(Err(e)) => ui::quiet_state(format!("The plate didn't render: {e}"), cx).into_any_element(),
            None => div().w(px(560.)).h(px(347.)).bg(t.wash).into_any_element(),
        };
        let mut facts = div().flex().flex_col().gap(S2).w(px(240.)).flex_none();
        facts = facts.child(ui::eyebrow("The day's plate", cx));
        if let Some(a) = &art {
            facts = facts
                .child(div().font_family(FONT_DOC).text_size(px(20.)).text_color(t.ink).child(a.biome.clone()))
                .child(div().text_sm().text_color(t.ink_2).child(a.zone.name.clone()))
                .child(
                    div().flex().gap(px(4.)).pt(S1).children(
                        a.zone
                            .colors
                            .iter()
                            .chain(std::iter::once(&a.zone.wash))
                            .filter_map(|c| parse_hex(c))
                            .map(|c| div().w(px(18.)).h(px(18.)).rounded(px(2.)).bg(c)),
                    ),
                )
                .child(ui::field_row(
                    "Cell",
                    ui::mono(a.coord.iter().map(|n| n.to_string()).collect::<Vec<_>>().join(" · "), cx),
                    cx,
                ));
        } else {
            facts = facts.child(
                div().text_sm().text_color(t.ink_2).child("A terrain drawn from the date alone — every day has one."),
            );
        }
        col = col.child(
            div().flex().flex_wrap().gap(S5).items_start().child(div().flex_none().child(plate_el)).child(facts),
        );
        col.into_any_element()
    }

    fn render_archive(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let Some(a) = self.archive.clone() else {
            return match &self.archive_error {
                Some(e) => ui::quiet_state(e.clone(), cx).into_any_element(),
                None => ui::quiet_state("Loading the archive…", cx).into_any_element(),
            };
        };
        if a.photos.is_empty() {
            return ui::quiet_state("The archive is empty — the index fills as days are fetched.", cx)
                .into_any_element();
        }
        let current = self.day.as_ref().map(|d| d.photo.date.clone());
        let cells: Vec<AnyElement> = a
            .photos
            .iter()
            .map(|p| {
                let src = if !p.thumb_url.is_empty() {
                    p.thumb_url.clone()
                } else if !is_video(p) {
                    p.image_url.clone()
                } else {
                    String::new()
                };
                let pic = if src.is_empty() { None } else { self.remote(&src, THUMB_EDGE, cx) };
                let on = current.as_deref() == Some(p.date.as_str());
                let date = p.date.clone();
                let thumb = match pic {
                    Some(Ok(pic)) => {
                        img(pic.img.clone()).w_full().h(px(150.)).object_fit(ObjectFit::Cover).into_any_element()
                    }
                    Some(Err(_)) | None if src.is_empty() => div()
                        .w_full()
                        .h(px(150.))
                        .bg(t.wash)
                        .flex()
                        .items_center()
                        .justify_center()
                        .text_xs()
                        .text_color(t.ink_3)
                        .child(if is_video(p) { "video" } else { "no image" })
                        .into_any_element(),
                    Some(Err(_)) => div()
                        .w_full()
                        .h(px(150.))
                        .bg(t.wash)
                        .flex()
                        .items_center()
                        .justify_center()
                        .text_xs()
                        .text_color(t.ink_3)
                        .child("didn't load")
                        .into_any_element(),
                    None => div().w_full().h(px(150.)).bg(t.wash).into_any_element(),
                };
                let accent = t.accent;
                let wash = t.wash;
                div()
                    .id(SharedString::from(format!("arc-{}", p.date)))
                    .w(px(224.))
                    .flex()
                    .flex_col()
                    .gap(px(6.))
                    .p(px(6.))
                    .rounded(px(3.))
                    .cursor_pointer()
                    .when(on, move |d| d.bg(wash))
                    .hover(move |s| s.bg(wash))
                    .child(
                        div()
                            .rounded(px(2.))
                            .overflow_hidden()
                            .when(on, move |d| d.border_b_2().border_color(accent))
                            .child(thumb),
                    )
                    .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(p.date.clone()))
                    .child(div().text_sm().text_color(t.ink).line_clamp(2).child(p.title.clone()))
                    .on_click(cx.listener(move |this, _, _, cx| this.open_day(date.clone(), cx)))
                    .into_any_element()
            })
            .collect();
        div()
            .px(S5)
            .pt(S4)
            .pb(px(64.))
            .flex()
            .flex_col()
            .gap(S3)
            .when(self.archive_loading, |d| d.child(div().text_xs().text_color(t.ink_3).child("Loading…")))
            .when_some(self.archive_error.clone(), |d, e| d.child(ui::notice(e, t.bad, cx)))
            .child(div().flex().flex_wrap().gap(S3).children(cells))
            .child(div().text_xs().text_color(t.ink_3).child("← newer · older →   Click a day to read it."))
            .into_any_element()
    }
}

impl Workspace for DailyWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        let day = self.day.clone()?;
        let p = day.photo;
        let mut col = div().flex().flex_col().gap(S1);
        col = col.child(ui::eyebrow("Photograph", cx));
        col = col.child(ui::field_row("Date", ui::mono(p.date.clone(), cx), cx));
        col = col.child(ui::field_row(
            "Source",
            ui::mono(
                if p.source == "nasa" { "NASA · Astronomy Picture of the Day".to_string() } else { p.source.clone() },
                cx,
            ),
            cx,
        ));
        col = col.child(ui::field_row("Media", ui::mono(p.media_type.clone(), cx), cx));
        if !p.cid.is_empty() {
            col = col.child(ui::field_row("CID", ui::mono(p.cid.clone(), cx), cx));
        }
        let mut links = div().flex().flex_wrap().gap(S2).pt(S1);
        if !p.source_url.is_empty() {
            let u = p.source_url.clone();
            links = links.child(ui::button("src", "Open source", BtnKind::Quiet, cx, move |_, _, cx| cx.open_url(&u)));
        }
        if !p.image_url.is_empty() {
            let u = p.image_url.clone();
            links = links.child(ui::button("copy-img", "Copy image URL", BtnKind::Quiet, cx, move |_, _, cx| {
                cx.write_to_clipboard(gpui::ClipboardItem::new_string(u.clone()));
                toast(cx, "Image URL copied.", false);
            }));
        }
        if let Some(u) = self.public(cx, &format!("/photo/{}", p.date)) {
            links =
                links.child(ui::button("pub", "Open on the web", BtnKind::Quiet, cx, move |_, _, cx| cx.open_url(&u)));
        }
        col = col.child(links);
        if let Some(a) = self.art.clone().filter(|a| a.date == p.date) {
            col = col.child(div().py(S2).child(ui::rule(cx))).child(ui::eyebrow("Plate", cx));
            if let Some(Slot::Ready(pic)) = self.gallery.slots.get(&format!("plate:{}", a.date)) {
                let pic = pic.clone();
                col = col.child(
                    img(pic.img.clone()).w(px(260.)).h(px(260. * pic.h as f32 / pic.w.max(1) as f32)).rounded(px(2.)),
                );
            }
            col = col.child(ui::field_row("Biome", div().text_sm().text_color(t.ink).child(a.biome.clone()), cx));
            col = col.child(ui::field_row("Zone", div().text_sm().text_color(t.ink).child(a.zone.name.clone()), cx));
            col = col.child(ui::field_row(
                "Palette",
                div().flex().flex_col().gap(px(3.)).children(a.zone.colors.iter().filter_map(|c| {
                    parse_hex(c).map(|h| {
                        div()
                            .flex()
                            .items_center()
                            .gap(S2)
                            .child(div().w(px(12.)).h(px(12.)).rounded(px(2.)).bg(h))
                            .child(ui::mono(c.clone(), cx))
                    })
                })),
                cx,
            ));
            col = col.child(ui::field_row("Plate CID", ui::mono(a.cid.clone(), cx), cx));
            if let Some(u) = self.public(cx, &format!("/art/{}", a.date)) {
                col = col.child(ui::button(
                    "pub-art",
                    "Open the plate on the web",
                    BtnKind::Quiet,
                    cx,
                    move |_, _, cx| cx.open_url(&u),
                ));
            }
        }
        col = col.child(div().pt(S3).text_xs().text_color(t.ink_3).child("←/→ move a day · T today · A archive"));
        Some(col.into_any_element())
    }

    fn focus_search(&mut self, w: &mut Window, _cx: &mut Context<Self>) {
        // no filter here: ⌘F hands the keyboard to the day view
        w.focus(&self.focus);
    }

    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        let want = self.want.clone();
        self.load_day(want, cx);
        if self.view == View::Archive {
            self.load_archive(self.page, cx);
        }
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        let mut v = vec![
            ("prev", "Daily: earlier day".into(), "←"),
            ("next", "Daily: later day".into(), "→"),
            ("today", "Daily: today".into(), "T"),
            (
                if self.view == View::Archive { "day" } else { "archive" },
                if self.view == View::Archive { "Daily: back to the day".into() } else { "Daily: archive".into() },
                "A",
            ),
        ];
        if self.day.is_some() {
            v.push(("web", "Daily: open this day on the web".into(), ""));
            v.push(("source", "Daily: open the source (NASA)".into(), ""));
        }
        v
    }

    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "prev" | "next" => {
                if self.view != View::Day {
                    self.view = View::Day;
                }
                self.step(id == "next", cx)
            }
            "today" => {
                self.view = View::Day;
                self.load_day(None, cx)
            }
            "archive" => self.set_view(View::Archive, cx),
            "day" => self.set_view(View::Day, cx),
            "web" => {
                if let Some(u) = self.day.as_ref().and_then(|d| self.public(cx, &format!("/photo/{}", d.photo.date))) {
                    cx.open_url(&u)
                }
            }
            "source" => {
                if let Some(u) = self.day.as_ref().map(|d| d.photo.source_url.clone()).filter(|u| !u.is_empty()) {
                    cx.open_url(&u)
                }
            }
            _ => {}
        }
        w.focus(&self.focus);
    }
}

impl Render for DailyWs {
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        for old in std::mem::take(&mut self.gallery.retired) {
            let _ = w.drop_image(old);
        }
        // the first day arrives after startup has settled focus on the shell
        if (!self.grabbed && self.day.is_some()) || w.focused(cx).is_none() {
            self.grabbed = true;
            w.focus(&self.focus);
        }
        let body = match self.view {
            View::Day => self.render_day(cx),
            View::Archive => self.render_archive(cx),
        };
        let bar = self.render_bar(cx);
        let status = self.status_line(cx);
        let loading = self.loading && self.view == View::Day && self.day.is_some();
        let t = theme(cx).clone();
        div()
            .size_full()
            .flex()
            .flex_col()
            .track_focus(&self.focus)
            .key_context("Daily")
            .on_key_down(cx.listener(Self::on_key))
            .on_mouse_down(gpui::MouseButton::Left, cx.listener(|this, _, w, _| w.focus(&this.focus)))
            .child(bar)
            .when_some(status, |d, s| d.child(div().px(S5).py(S2).child(s)))
            .when(loading, |d| d.child(div().px(S5).py(px(4.)).text_xs().text_color(t.ink_3).child("Loading…")))
            .child(div().id("daily-scroll").flex_1().min_h_0().overflow_y_scroll().child(body))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn plate_rasterizes_to_bgra_at_the_asked_width() {
        let svg = br##"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 500 310"><rect width="500" height="310" fill="#ff0000"/><text x="250" y="50" font-family="Menlo, monospace" font-size="11">~</text></svg>"##;
        let p = rasterize(svg, 1500).unwrap();
        assert_eq!((p.w, p.h), (1500, 930));
        // red in BGRA: blue byte first
        assert_eq!(&p.img.as_bytes(0).unwrap()[..4], &[0, 0, 255, 255]);
    }

    #[test]
    fn photos_downscale_and_html_is_refused() {
        let mut png = Vec::new();
        image::RgbaImage::from_pixel(4000, 1000, image::Rgba([0, 255, 0, 255]))
            .write_to(&mut std::io::Cursor::new(&mut png), image::ImageFormat::Png)
            .unwrap();
        let p = decode(&png, THUMB_EDGE).unwrap();
        assert_eq!((p.w, p.h), (480, 120));
        assert!(decode(b"<html>moved</html>", HERO_EDGE).is_err());
    }

    #[test]
    fn gallery_is_bounded_and_retires_evicted_textures() {
        let mut g = Gallery::default();
        let px = |n: u8| {
            decode(
                &{
                    let mut b = Vec::new();
                    image::RgbaImage::from_pixel(2, 2, image::Rgba([n, n, n, 255]))
                        .write_to(&mut std::io::Cursor::new(&mut b), image::ImageFormat::Png)
                        .unwrap();
                    b
                },
                10,
            )
            .unwrap()
        };
        for i in 0..(CACHE_ENTRIES + 5) {
            g.put(format!("k{i}"), Slot::Ready(px(i as u8)));
        }
        assert_eq!(g.slots.len(), CACHE_ENTRIES);
        assert_eq!(g.retired.len(), 5);
        assert!(g.get("k0").is_none() && g.get("k5").is_some());
    }
}
