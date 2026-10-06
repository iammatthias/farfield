//! Farfield — the native client for the farfield fleet.
mod app;
mod evidence;
mod shell;
mod theme;
mod ui;
mod workspace;
mod ws;

use gpui::{prelude::*, Focusable, px, size, App, Application, Bounds, TitlebarOptions, WindowBounds, WindowOptions};
use shell::{Entry, Overlay, Shell};
use workspace::Handle;

fn entries() -> Vec<Entry> {
    vec![
        Entry { id: "content", title: "Content", service: "content", make: |w, cx| Handle::new("content", "Content", cx.new(|cx| ws::content::ContentWs::new(w, cx))) },
        Entry { id: "connections", title: "Connections", service: "apex", make: |w, cx| Handle::new("connections", "Connections", cx.new(|cx| ws::connections::Connections::new(w, cx))) },
    ]
}

fn main() {
    Application::new().run(|cx: &mut App| {
        theme::load_fonts(cx);
        cx.set_global(app::AppState::load());
        cx.set_global(Overlay::default());
        cx.set_global(theme::Theme::new(false));
        ui::input::bind_keys(cx);
        ui::doc_editor::bind_keys(cx);
        shell::bind_keys(cx);
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
