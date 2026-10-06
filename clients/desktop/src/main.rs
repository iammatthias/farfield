//! Farfield — the native client for the farfield fleet.

// GPUI callbacks are boxed closures over (&mut Window, &mut App); spelling
// them through aliases would hide the signature without simplifying it, and
// a few view builders take the theme, context and their own options.
#![allow(clippy::type_complexity, clippy::too_many_arguments)]
mod app;
mod evidence;
mod menu;
mod perf;
mod shell;
mod signin;
mod theme;
mod ui;
mod workspace;
mod ws;

use gpui::{prelude::*, px, size, App, Application, Bounds, Focusable, TitlebarOptions, WindowBounds, WindowOptions};
use shell::{Entry, Overlay, Shell};
use workspace::Handle;

fn entries() -> Vec<Entry> {
    macro_rules! e {
        ($id:literal, $title:literal, $svc:literal, $ty:path) => {
            Entry {
                id: $id,
                title: $title,
                service: $svc,
                make: |w, cx| Handle::new($id, $title, cx.new(|cx| <$ty>::new(w, cx))),
            }
        };
    }
    vec![
        e!("content", "Content", "content", ws::content::ContentWs),
        e!("feed", "Feed", "feed", ws::feed::FeedWs),
        e!("blobs", "Blobs", "blobs", ws::blobs::BlobsWs),
        e!("bookmarks", "Bookmarks", "bookmarks", ws::bookmarks::BookmarksWs),
        e!("library", "Library", "library", ws::library::LibraryWs),
        e!("daily", "Daily", "daily", ws::daily::DailyWs),
        e!("qr", "QR", "qr", ws::qr::QrWs),
        e!("scrap", "Scrap", "scrap", ws::scrap::ScrapWs),
        e!("sideload", "Sideload", "sideload", ws::sideload::SideloadWs),
        e!("pulse", "Pulse", "pulse", ws::pulse::PulseWs),
        e!("switchboard", "Switchboard", "switchboard", ws::switchboard::SwitchboardWs),
        e!("backup", "Backup", "backup", ws::backup::BackupWs),
        e!("keys", "Keys", "keys", ws::keys::KeysWs),
        e!("apex", "Apex", "apex", ws::apex::ApexWs),
        e!("connections", "Settings", "apex", ws::connections::Connections),
    ]
}

fn main() {
    perf::process_start();
    // compile editor.wasm off the main thread while the window comes up
    std::thread::spawn(farfield_editor::prewarm);
    Application::new().run(|cx: &mut App| {
        theme::load_fonts(cx);
        cx.set_global(app::AppState::load());
        cx.set_global(Overlay::default());
        cx.set_global(theme::Theme::new(false));
        ui::input::bind_keys(cx);
        ui::doc_editor::bind_keys(cx);
        shell::bind_keys(cx);
        ws::onboarding::bind_keys(cx);
        menu::install(cx);
        let bounds = Bounds::centered(None, size(px(1280.), px(820.)), cx);
        let win = cx
            .open_window(
                WindowOptions {
                    window_bounds: Some(WindowBounds::Windowed(bounds)),
                    titlebar: Some(TitlebarOptions {
                        title: Some("Farfield".into()),
                        appears_transparent: true,
                        traffic_light_position: Some(gpui::point(px(14.), px(17.))),
                    }),
                    window_min_size: Some(size(px(760.), px(480.))),
                    // measurement runs keep the window above others: macOS stops
                    // drawing a covered window, which would time the occlusion,
                    // not the app (FARFIELD_TOPMOST=1, set by perf/run.sh)
                    kind: if std::env::var("FARFIELD_TOPMOST").as_deref() == Ok("1") {
                        gpui::WindowKind::PopUp
                    } else {
                        gpui::WindowKind::Normal
                    },
                    ..Default::default()
                },
                |w, cx| {
                    shell::apply_theme(w, cx);
                    cx.new(|cx| Shell::new(entries(), w, cx))
                },
            )
            .unwrap();
        win.update(cx, |s, w, cx| w.focus(&s.focus_handle(cx))).ok();
        cx.activate(true);
        evidence::maybe_run(win, cx);
    });
}
