//! The macOS menu bar. GPUI shows none unless the app sets one, and the
//! standard app commands (Hide, Quit, Minimize…) are actions like any other.

use crate::shell;
use crate::ui::{doc_editor, input};
use gpui::{actions, App, KeyBinding, Menu, MenuItem, SystemMenuType};

actions!(app_menu, [Quit, Hide, HideOthers, ShowAll, Minimize, Zoom, Fullscreen, CloseWindow]);

pub fn install(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("cmd-q", Quit, None),
        KeyBinding::new("cmd-h", Hide, None),
        KeyBinding::new("cmd-alt-h", HideOthers, None),
        KeyBinding::new("cmd-m", Minimize, None),
        KeyBinding::new("cmd-w", CloseWindow, None),
        KeyBinding::new("ctrl-cmd-f", Fullscreen, None),
    ]);
    cx.on_action(|_: &Quit, cx| cx.quit());
    cx.on_action(|_: &Hide, cx| cx.hide());
    cx.on_action(|_: &HideOthers, cx| cx.hide_other_apps());
    cx.on_action(|_: &ShowAll, cx| cx.unhide_other_apps());
    cx.on_action(|_: &Minimize, cx| with_window(cx, |w| w.minimize_window()));
    cx.on_action(|_: &Zoom, cx| with_window(cx, |w| w.zoom_window()));
    cx.on_action(|_: &Fullscreen, cx| with_window(cx, |w| w.toggle_fullscreen()));
    // one window: closing it ends the app, as Quit would
    cx.on_action(|_: &CloseWindow, cx| cx.quit());
    cx.set_menus(vec![
        Menu {
            name: "Farfield".into(),
            items: vec![
                MenuItem::action("Settings…", shell::Connections),
                MenuItem::separator(),
                MenuItem::os_submenu("Services", SystemMenuType::Services),
                MenuItem::separator(),
                MenuItem::action("Hide Farfield", Hide),
                MenuItem::action("Hide Others", HideOthers),
                MenuItem::action("Show All", ShowAll),
                MenuItem::separator(),
                MenuItem::action("Quit Farfield", Quit),
            ],
        },
        Menu {
            name: "File".into(),
            items: vec![
                MenuItem::action("New", shell::NewItem),
                MenuItem::action("Save", shell::Save),
                MenuItem::separator(),
                MenuItem::action("Close Window", CloseWindow),
            ],
        },
        Menu {
            name: "Edit".into(),
            items: vec![
                MenuItem::action("Undo", doc_editor::Undo),
                MenuItem::action("Redo", doc_editor::Redo),
                MenuItem::separator(),
                MenuItem::action("Cut", input::Cut),
                MenuItem::action("Copy", input::Copy),
                MenuItem::action("Paste", input::Paste),
                MenuItem::action("Select All", input::SelectAll),
                MenuItem::separator(),
                MenuItem::action("Find", shell::Search),
            ],
        },
        Menu {
            name: "View".into(),
            items: vec![
                MenuItem::action("Command Palette", shell::OpenPalette),
                MenuItem::action("Toggle Sidebar", shell::ToggleNav),
                MenuItem::action("Toggle Inspector", shell::ToggleInspector),
                MenuItem::action("Refresh", shell::Refresh),
                MenuItem::separator(),
                MenuItem::action("Next Workspace", shell::NextWs),
                MenuItem::action("Previous Workspace", shell::PrevWs),
                MenuItem::separator(),
                MenuItem::action("Cycle Appearance", shell::CycleTheme),
                MenuItem::action("Enter Full Screen", Fullscreen),
            ],
        },
        Menu {
            name: "Window".into(),
            items: vec![MenuItem::action("Minimize", Minimize), MenuItem::action("Zoom", Zoom)],
        },
    ]);
}

fn with_window(cx: &mut App, f: impl FnOnce(&mut gpui::Window)) {
    if let Some(w) = cx.active_window().or_else(|| cx.windows().into_iter().next()) {
        let _ = w.update(cx, |_, w, _| f(w));
    }
}
