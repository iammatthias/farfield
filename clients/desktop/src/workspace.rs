//! A workspace is one service's area of the app. The shell keeps every
//! workspace it has opened alive, so moving between them never loses work:
//! an open editor, a half-typed field, a scroll position all stay put.

use gpui::{AnyElement, AnyView, App, Context, Entity, Render, SharedString, Window};
use std::sync::Arc;

/// An entry in the command palette.
#[derive(Clone)]
pub struct PaletteItem {
    pub title: SharedString,
    pub subtitle: SharedString,
    pub run: Arc<dyn Fn(&mut Window, &mut App)>,
}

impl PaletteItem {
    pub fn new(title: impl Into<SharedString>, subtitle: impl Into<SharedString>, run: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        PaletteItem { title: title.into(), subtitle: subtitle.into(), run: Arc::new(run) }
    }
}

pub trait Workspace: Render + Sized {
    /// The inspector column for the current selection.
    fn inspector(&mut self, _w: &mut Window, _cx: &mut Context<Self>) -> Option<AnyElement> {
        None
    }
    /// Actions this workspace offers to the palette.
    fn palette(&self, _cx: &App) -> Vec<PaletteItem> {
        Vec::new()
    }
    /// Scoped search (⌘F): focus this workspace's filter.
    fn focus_search(&mut self, _w: &mut Window, _cx: &mut Context<Self>) {}
    /// Create something new (⌘N).
    fn new_item(&mut self, _w: &mut Window, _cx: &mut Context<Self>) {}
    /// Save (⌘S).
    fn save(&mut self, _w: &mut Window, _cx: &mut Context<Self>) {}
    /// Reload from the server (⌘R).
    fn refresh(&mut self, _w: &mut Window, _cx: &mut Context<Self>) {}
    /// Unsaved local work, for the nav marker and the quit check.
    fn dirty(&self, _cx: &App) -> bool {
        false
    }
}

type InspectorFn = Box<dyn Fn(&mut Window, &mut App) -> Option<AnyElement>>;
type PaletteFn = Box<dyn Fn(&App) -> Vec<PaletteItem>>;
type ActFn = Box<dyn Fn(&mut Window, &mut App)>;
type DirtyFn = Box<dyn Fn(&App) -> bool>;

/// A type-erased workspace the shell can hold in a list.
pub struct Handle {
    pub id: &'static str,
    pub title: &'static str,
    pub view: AnyView,
    pub inspector: InspectorFn,
    pub palette: PaletteFn,
    pub focus_search: ActFn,
    pub new_item: ActFn,
    pub save: ActFn,
    pub refresh: ActFn,
    pub dirty: DirtyFn,
}

impl Handle {
    pub fn new<T: Workspace + 'static>(id: &'static str, title: &'static str, e: Entity<T>) -> Self {
        let (a, b, c, d, f, g, h) = (e.clone(), e.clone(), e.clone(), e.clone(), e.clone(), e.clone(), e.clone());
        Handle {
            id,
            title,
            view: e.into(),
            inspector: Box::new(move |w, cx| a.update(cx, |ws, cx| ws.inspector(w, cx))),
            palette: Box::new(move |cx| b.read(cx).palette(cx)),
            focus_search: Box::new(move |w, cx| c.update(cx, |ws, cx| ws.focus_search(w, cx))),
            new_item: Box::new(move |w, cx| d.update(cx, |ws, cx| ws.new_item(w, cx))),
            save: Box::new(move |w, cx| f.update(cx, |ws, cx| ws.save(w, cx))),
            refresh: Box::new(move |w, cx| g.update(cx, |ws, cx| ws.refresh(w, cx))),
            dirty: Box::new(move |cx| h.read(cx).dirty(cx)),
        }
    }
}
