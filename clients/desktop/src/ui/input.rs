//! A single-line text field, v5-style: no box, an underline that is the
//! field's edge, thickening to the accent on focus. Full platform text input
//! (IME composition with marked text, dictation, the character palette)
//! through GPUI's input handler; UTF-8 internally, UTF-16 at the platform
//! boundary.

use crate::theme::{theme, FONT_MONO, FONT_UI};
use gpui::{
    actions, div, fill, point, prelude::*, px, relative, size, App, Bounds, ClipboardItem, Context, CursorStyle,
    ElementId, ElementInputHandler, Entity, EntityInputHandler, EventEmitter, FocusHandle, Focusable, GlobalElementId,
    KeyBinding, LayoutId, MouseButton, MouseDownEvent, MouseMoveEvent, MouseUpEvent, PaintQuad, Pixels, Point,
    ShapedLine, SharedString, Style, TextRun, UTF16Selection, UnderlineStyle, Window,
};
use std::ops::Range;
use unicode_segmentation::UnicodeSegmentation;

actions!(
    field,
    [
        Backspace,
        BackspaceWord,
        Delete,
        Left,
        Right,
        WordLeft,
        WordRight,
        SelectLeft,
        SelectRight,
        SelectWordLeft,
        SelectWordRight,
        SelectAll,
        Home,
        End,
        SelectHome,
        SelectEnd,
        ShowCharacterPalette,
        Paste,
        Cut,
        Copy,
        Submit,
        Cancel,
        Up,
        Down,
    ]
);

pub fn bind_keys(cx: &mut App) {
    let c = Some("Field");
    cx.bind_keys([
        KeyBinding::new("backspace", Backspace, c),
        KeyBinding::new("alt-backspace", BackspaceWord, c),
        KeyBinding::new("delete", Delete, c),
        KeyBinding::new("left", Left, c),
        KeyBinding::new("right", Right, c),
        KeyBinding::new("alt-left", WordLeft, c),
        KeyBinding::new("alt-right", WordRight, c),
        KeyBinding::new("shift-left", SelectLeft, c),
        KeyBinding::new("shift-right", SelectRight, c),
        KeyBinding::new("alt-shift-left", SelectWordLeft, c),
        KeyBinding::new("alt-shift-right", SelectWordRight, c),
        KeyBinding::new("cmd-a", SelectAll, c),
        KeyBinding::new("home", Home, c),
        KeyBinding::new("end", End, c),
        KeyBinding::new("cmd-left", Home, c),
        KeyBinding::new("cmd-right", End, c),
        KeyBinding::new("cmd-shift-left", SelectHome, c),
        KeyBinding::new("cmd-shift-right", SelectEnd, c),
        KeyBinding::new("ctrl-cmd-space", ShowCharacterPalette, c),
        KeyBinding::new("cmd-v", Paste, c),
        KeyBinding::new("cmd-c", Copy, c),
        KeyBinding::new("cmd-x", Cut, c),
        KeyBinding::new("enter", Submit, c),
        KeyBinding::new("escape", Cancel, c),
        KeyBinding::new("up", Up, c),
        KeyBinding::new("down", Down, c),
    ]);
}

#[derive(Clone, Debug, PartialEq)]
pub enum FieldEvent {
    Changed,
    Submit,
    Cancel,
    Blur,
    /// Arrow keys: a filter field moves the selection of the list it filters.
    Up,
    Down,
}

pub struct TextField {
    focus: FocusHandle,
    content: SharedString,
    placeholder: SharedString,
    selected: Range<usize>,
    reversed: bool,
    marked: Option<Range<usize>>,
    last_layout: Option<ShapedLine>,
    last_bounds: Option<Bounds<Pixels>>,
    selecting: bool,
    scroll_x: Pixels,
    mono: bool,
    doc: bool,
    secret: bool,
    pub label: SharedString,
}

impl EventEmitter<FieldEvent> for TextField {}

impl TextField {
    pub fn new(
        w: &mut Window,
        cx: &mut Context<Self>,
        label: impl Into<SharedString>,
        placeholder: impl Into<SharedString>,
    ) -> Self {
        let focus = cx.focus_handle();
        cx.on_blur(&focus, w, |_this: &mut Self, _w: &mut Window, cx: &mut Context<Self>| cx.emit(FieldEvent::Blur))
            .detach();
        TextField {
            focus,
            content: "".into(),
            placeholder: placeholder.into(),
            selected: 0..0,
            reversed: false,
            marked: None,
            last_layout: None,
            last_bounds: None,
            selecting: false,
            scroll_x: px(0.),
            mono: false,
            doc: false,
            secret: false,
            label: label.into(),
        }
    }

    /// Technical values (CIDs, slugs, URLs) read in Plex Mono.
    pub fn mono(mut self) -> Self {
        self.mono = true;
        self
    }

    /// A document title: Newsreader, larger, no visible edge until focus.
    pub fn doc(mut self) -> Self {
        self.doc = true;
        self
    }

    /// Credentials: shown as bullets, never copyable.
    pub fn secret(mut self) -> Self {
        self.secret = true;
        self
    }

    pub fn text(&self) -> String {
        self.content.to_string()
    }

    /// Replace the text (from a model); keeps the caret at the end.
    pub fn set_text(&mut self, s: impl Into<SharedString>, cx: &mut Context<Self>) {
        let s: SharedString = s.into();
        if s == self.content {
            return;
        }
        self.content = s;
        self.selected = self.content.len()..self.content.len();
        self.marked = None;
        cx.notify();
    }

    pub fn focus(&self, window: &mut Window) {
        window.focus(&self.focus);
    }

    fn display(&self) -> SharedString {
        if self.secret {
            "•".repeat(self.content.chars().count()).into()
        } else {
            self.content.clone()
        }
    }

    fn changed(&mut self, cx: &mut Context<Self>) {
        cx.emit(FieldEvent::Changed);
        cx.notify();
    }

    fn cursor(&self) -> usize {
        if self.reversed {
            self.selected.start
        } else {
            self.selected.end
        }
    }

    fn move_to(&mut self, o: usize, cx: &mut Context<Self>) {
        self.selected = o..o;
        self.reversed = false;
        cx.notify();
    }

    fn select_to(&mut self, o: usize, cx: &mut Context<Self>) {
        if self.reversed {
            self.selected.start = o
        } else {
            self.selected.end = o
        }
        if self.selected.end < self.selected.start {
            self.reversed = !self.reversed;
            self.selected = self.selected.end..self.selected.start;
        }
        cx.notify();
    }

    fn prev_boundary(&self, o: usize) -> usize {
        self.content.grapheme_indices(true).rev().find_map(|(i, _)| (i < o).then_some(i)).unwrap_or(0)
    }
    fn next_boundary(&self, o: usize) -> usize {
        self.content.grapheme_indices(true).find_map(|(i, _)| (i > o).then_some(i)).unwrap_or(self.content.len())
    }
    fn prev_word(&self, o: usize) -> usize {
        self.content.unicode_word_indices().rev().find_map(|(i, _)| (i < o).then_some(i)).unwrap_or(0)
    }
    fn next_word(&self, o: usize) -> usize {
        self.content
            .unicode_word_indices()
            .find_map(|(i, w)| (i + w.len() > o).then_some(i + w.len()))
            .unwrap_or(self.content.len())
    }

    fn left(&mut self, _: &Left, _: &mut Window, cx: &mut Context<Self>) {
        if self.selected.is_empty() {
            self.move_to(self.prev_boundary(self.cursor()), cx)
        } else {
            self.move_to(self.selected.start, cx)
        }
    }
    fn right(&mut self, _: &Right, _: &mut Window, cx: &mut Context<Self>) {
        if self.selected.is_empty() {
            self.move_to(self.next_boundary(self.selected.end), cx)
        } else {
            self.move_to(self.selected.end, cx)
        }
    }
    fn word_left(&mut self, _: &WordLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(self.prev_word(self.cursor()), cx)
    }
    fn word_right(&mut self, _: &WordRight, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(self.next_word(self.cursor()), cx)
    }
    fn select_left(&mut self, _: &SelectLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.prev_boundary(self.cursor()), cx)
    }
    fn select_right(&mut self, _: &SelectRight, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.next_boundary(self.cursor()), cx)
    }
    fn select_word_left(&mut self, _: &SelectWordLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.prev_word(self.cursor()), cx)
    }
    fn select_word_right(&mut self, _: &SelectWordRight, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.next_word(self.cursor()), cx)
    }
    fn select_all(&mut self, _: &SelectAll, _: &mut Window, cx: &mut Context<Self>) {
        self.selected = 0..self.content.len();
        self.reversed = false;
        cx.notify()
    }
    fn home(&mut self, _: &Home, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(0, cx)
    }
    fn end(&mut self, _: &End, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(self.content.len(), cx)
    }
    fn select_home(&mut self, _: &SelectHome, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(0, cx)
    }
    fn select_end(&mut self, _: &SelectEnd, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.content.len(), cx)
    }
    fn backspace(&mut self, _: &Backspace, w: &mut Window, cx: &mut Context<Self>) {
        if self.selected.is_empty() {
            self.select_to(self.prev_boundary(self.cursor()), cx)
        }
        self.replace_text_in_range(None, "", w, cx)
    }
    fn backspace_word(&mut self, _: &BackspaceWord, w: &mut Window, cx: &mut Context<Self>) {
        if self.selected.is_empty() {
            self.select_to(self.prev_word(self.cursor()), cx)
        }
        self.replace_text_in_range(None, "", w, cx)
    }
    fn delete(&mut self, _: &Delete, w: &mut Window, cx: &mut Context<Self>) {
        if self.selected.is_empty() {
            self.select_to(self.next_boundary(self.cursor()), cx)
        }
        self.replace_text_in_range(None, "", w, cx)
    }
    fn show_character_palette(&mut self, _: &ShowCharacterPalette, w: &mut Window, _: &mut Context<Self>) {
        w.show_character_palette();
    }
    fn paste(&mut self, _: &Paste, w: &mut Window, cx: &mut Context<Self>) {
        if let Some(t) = cx.read_from_clipboard().and_then(|i| i.text()) {
            self.replace_text_in_range(None, &t.replace(['\r', '\n'], " "), w, cx);
        }
    }
    fn copy(&mut self, _: &Copy, _: &mut Window, cx: &mut Context<Self>) {
        if !self.selected.is_empty() && !self.secret {
            cx.write_to_clipboard(ClipboardItem::new_string(self.content[self.selected.clone()].to_string()));
        }
    }
    fn cut(&mut self, _: &Cut, w: &mut Window, cx: &mut Context<Self>) {
        if !self.selected.is_empty() && !self.secret {
            cx.write_to_clipboard(ClipboardItem::new_string(self.content[self.selected.clone()].to_string()));
            self.replace_text_in_range(None, "", w, cx)
        }
    }
    fn submit(&mut self, _: &Submit, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(FieldEvent::Submit)
    }
    fn cancel(&mut self, _: &Cancel, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(FieldEvent::Cancel)
    }
    fn up(&mut self, _: &Up, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(FieldEvent::Up)
    }
    fn down(&mut self, _: &Down, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(FieldEvent::Down)
    }

    fn on_mouse_down(&mut self, e: &MouseDownEvent, w: &mut Window, cx: &mut Context<Self>) {
        w.focus(&self.focus);
        self.selecting = true;
        let i = self.index_for(e.position);
        if e.click_count >= 2 {
            self.selected = self.prev_word(i.saturating_add(1)).min(i)..self.next_word(i);
            self.reversed = false;
            cx.notify();
        } else if e.modifiers.shift {
            self.select_to(i, cx)
        } else {
            self.move_to(i, cx)
        }
    }
    fn on_mouse_up(&mut self, _: &MouseUpEvent, _: &mut Window, _: &mut Context<Self>) {
        self.selecting = false;
    }
    fn on_mouse_move(&mut self, e: &MouseMoveEvent, _: &mut Window, cx: &mut Context<Self>) {
        if self.selecting {
            self.select_to(self.index_for(e.position), cx)
        }
    }

    fn index_for(&self, p: Point<Pixels>) -> usize {
        if self.content.is_empty() {
            return 0;
        }
        let (Some(b), Some(line)) = (self.last_bounds.as_ref(), self.last_layout.as_ref()) else { return 0 };
        if p.y < b.top() {
            return 0;
        }
        if p.y > b.bottom() {
            return self.content.len();
        }
        let i = line.closest_index_for_x(p.x - b.left() + self.scroll_x);
        if self.secret {
            // bullets are 3 bytes each; map back to the content's char index
            let nth = i / '•'.len_utf8();
            return self.content.char_indices().nth(nth).map(|(b, _)| b).unwrap_or(self.content.len());
        }
        i
    }

    fn to16(&self, o: usize) -> usize {
        farfield_editor::utf8_to_utf16(&self.content, o)
    }
    fn from16(&self, o: usize) -> usize {
        farfield_editor::utf16_to_utf8(&self.content, o)
    }
    fn range_to16(&self, r: &Range<usize>) -> Range<usize> {
        self.to16(r.start)..self.to16(r.end)
    }
    fn range_from16(&self, r: &Range<usize>) -> Range<usize> {
        self.from16(r.start)..self.from16(r.end)
    }
}

impl EntityInputHandler for TextField {
    fn text_for_range(
        &mut self,
        r16: Range<usize>,
        actual: &mut Option<Range<usize>>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<String> {
        let r = self.range_from16(&r16);
        actual.replace(self.range_to16(&r));
        Some(self.content[r].to_string())
    }
    fn selected_text_range(&mut self, _: bool, _: &mut Window, _: &mut Context<Self>) -> Option<UTF16Selection> {
        Some(UTF16Selection { range: self.range_to16(&self.selected), reversed: self.reversed })
    }
    fn marked_text_range(&self, _: &mut Window, _: &mut Context<Self>) -> Option<Range<usize>> {
        self.marked.as_ref().map(|r| self.range_to16(r))
    }
    fn unmark_text(&mut self, _: &mut Window, _: &mut Context<Self>) {
        self.marked = None;
    }
    fn replace_text_in_range(&mut self, r16: Option<Range<usize>>, new: &str, _: &mut Window, cx: &mut Context<Self>) {
        let r = r16.as_ref().map(|r| self.range_from16(r)).or(self.marked.clone()).unwrap_or(self.selected.clone());
        self.content = (self.content[..r.start].to_owned() + new + &self.content[r.end..]).into();
        self.selected = r.start + new.len()..r.start + new.len();
        self.reversed = false;
        self.marked = None;
        self.changed(cx);
    }
    fn replace_and_mark_text_in_range(
        &mut self,
        r16: Option<Range<usize>>,
        new: &str,
        sel16: Option<Range<usize>>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let r = r16.as_ref().map(|r| self.range_from16(r)).or(self.marked.clone()).unwrap_or(self.selected.clone());
        self.content = (self.content[..r.start].to_owned() + new + &self.content[r.end..]).into();
        self.marked = (!new.is_empty()).then(|| r.start..r.start + new.len());
        self.selected = sel16
            .as_ref()
            .map(|s| {
                let local = farfield_editor::utf16_to_utf8(new, s.start)..farfield_editor::utf16_to_utf8(new, s.end);
                r.start + local.start..r.start + local.end
            })
            .unwrap_or_else(|| r.start + new.len()..r.start + new.len());
        cx.notify();
    }
    fn bounds_for_range(
        &mut self,
        r16: Range<usize>,
        b: Bounds<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<Bounds<Pixels>> {
        let line = self.last_layout.as_ref()?;
        let r = self.range_from16(&r16);
        Some(Bounds::from_corners(
            point(b.left() + line.x_for_index(r.start) - self.scroll_x, b.top()),
            point(b.left() + line.x_for_index(r.end) - self.scroll_x, b.bottom()),
        ))
    }
    fn character_index_for_point(&mut self, p: Point<Pixels>, _: &mut Window, _: &mut Context<Self>) -> Option<usize> {
        let b = self.last_bounds?;
        let line = self.last_layout.as_ref()?;
        let i = line.index_for_x(p.x - b.left() + self.scroll_x)?;
        Some(self.to16(i))
    }
}

struct FieldElement {
    input: Entity<TextField>,
}

struct Prepaint {
    line: Option<ShapedLine>,
    cursor: Option<PaintQuad>,
    selection: Option<PaintQuad>,
    scroll_x: Pixels,
}

impl IntoElement for FieldElement {
    type Element = Self;
    fn into_element(self) -> Self {
        self
    }
}

impl Element for FieldElement {
    type RequestLayoutState = ();
    type PrepaintState = Prepaint;
    fn id(&self) -> Option<ElementId> {
        None
    }
    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }
    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        w: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, ()) {
        let mut style = Style::default();
        style.size.width = relative(1.).into();
        style.flex_grow = 1.;
        style.min_size.width = px(8.).into();
        style.size.height = w.line_height().into();
        (w.request_layout(style, [], cx), ())
    }
    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut (),
        w: &mut Window,
        cx: &mut App,
    ) -> Prepaint {
        let t = theme(cx).clone();
        let input = self.input.read(cx);
        let content = input.display();
        let style = w.text_style();
        // the selection/marked ranges index the real text; with bullets the
        // byte offsets differ, so map them through char counts
        let map = |o: usize| -> usize {
            if input.secret {
                input.content[..o].chars().count() * '•'.len_utf8()
            } else {
                o
            }
        };
        let (text, color) =
            if content.is_empty() { (input.placeholder.clone(), t.ink_3) } else { (content, style.color) };
        let run = TextRun {
            len: text.len(),
            font: style.font(),
            color,
            background_color: None,
            underline: None,
            strikethrough: None,
        };
        let runs = match input.marked.as_ref().filter(|_| !input.content.is_empty()) {
            Some(m) => vec![
                TextRun { len: map(m.start), ..run.clone() },
                TextRun {
                    len: map(m.end) - map(m.start),
                    underline: Some(UnderlineStyle { color: Some(run.color), thickness: px(1.), wavy: false }),
                    ..run.clone()
                },
                TextRun { len: text.len() - map(m.end), ..run },
            ]
            .into_iter()
            .filter(|r| r.len > 0)
            .collect(),
            None => vec![run],
        };
        let font_size = style.font_size.to_pixels(w.rem_size());
        let line = w.text_system().shape_line(text, font_size, &runs, None);
        let sel = map(input.selected.start)..map(input.selected.end);
        let cur_x = line.x_for_index(if input.content.is_empty() { 0 } else { map(input.cursor()) });
        // keep the caret in view: scroll by whole steps, never jitter
        let width = bounds.size.width;
        // an unfocused field shows the start of its text
        let focused = input.focus.is_focused(w);
        let mut scroll = if focused { input.scroll_x } else { px(0.) };
        if focused && cur_x - scroll > width - px(2.) {
            scroll = cur_x - width + px(24.);
        } else if cur_x < scroll {
            scroll = (cur_x - px(24.)).max(px(0.));
        }
        // a field laid out before it has a real width (a first frame, a
        // collapsed column) must not keep a scroll offset computed from it
        if line.width <= width || width < px(24.) {
            scroll = px(0.);
        }
        scroll = scroll.max(px(0.)).min((line.width - width + px(2.)).max(px(0.)));
        let (selection, cursor) = if sel.is_empty() || input.content.is_empty() {
            (
                None,
                Some(fill(
                    Bounds::new(point(bounds.left() + cur_x - scroll, bounds.top()), size(px(1.5), bounds.size.height)),
                    t.accent,
                )),
            )
        } else {
            (
                Some(fill(
                    Bounds::from_corners(
                        point(bounds.left() + line.x_for_index(sel.start) - scroll, bounds.top()),
                        point(bounds.left() + line.x_for_index(sel.end) - scroll, bounds.bottom()),
                    ),
                    t.select,
                )),
                None,
            )
        };
        Prepaint { line: Some(line), cursor, selection, scroll_x: scroll }
    }
    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut (),
        p: &mut Prepaint,
        w: &mut Window,
        cx: &mut App,
    ) {
        let focus = self.input.read(cx).focus.clone();
        w.handle_input(&focus, ElementInputHandler::new(bounds, self.input.clone()), cx);
        let line = p.line.take().unwrap();
        w.with_content_mask(Some(gpui::ContentMask { bounds }), |w| {
            if let Some(s) = p.selection.take() {
                w.paint_quad(s)
            }
            let _ = line.paint(point(bounds.origin.x - p.scroll_x, bounds.origin.y), w.line_height(), w, cx);
            if focus.is_focused(w) {
                if let Some(c) = p.cursor.take() {
                    w.paint_quad(c);
                }
            }
        });
        let scroll = p.scroll_x;
        self.input.update(cx, |i, _| {
            i.last_layout = Some(line);
            i.last_bounds = Some(bounds);
            i.scroll_x = scroll;
        });
    }
}

impl Render for TextField {
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let focused = self.focus.is_focused(w);
        div()
            .flex()
            .flex_col()
            .gap(px(2.))
            .w_full()
            .when(!self.label.is_empty(), |d| {
                d.child(div().text_xs().text_color(if focused { t.accent } else { t.ink_2 }).child(self.label.clone()))
            })
            .child(
                div()
                    .id("field")
                    .flex()
                    .key_context("Field")
                    .track_focus(&self.focus)
                    .cursor(CursorStyle::IBeam)
                    .on_action(cx.listener(Self::backspace))
                    .on_action(cx.listener(Self::backspace_word))
                    .on_action(cx.listener(Self::delete))
                    .on_action(cx.listener(Self::left))
                    .on_action(cx.listener(Self::right))
                    .on_action(cx.listener(Self::word_left))
                    .on_action(cx.listener(Self::word_right))
                    .on_action(cx.listener(Self::select_left))
                    .on_action(cx.listener(Self::select_right))
                    .on_action(cx.listener(Self::select_word_left))
                    .on_action(cx.listener(Self::select_word_right))
                    .on_action(cx.listener(Self::select_all))
                    .on_action(cx.listener(Self::home))
                    .on_action(cx.listener(Self::end))
                    .on_action(cx.listener(Self::select_home))
                    .on_action(cx.listener(Self::select_end))
                    .on_action(cx.listener(Self::show_character_palette))
                    .on_action(cx.listener(Self::paste))
                    .on_action(cx.listener(Self::cut))
                    .on_action(cx.listener(Self::copy))
                    .on_action(cx.listener(Self::submit))
                    .on_action(cx.listener(Self::cancel))
                    .on_action(cx.listener(Self::up))
                    .on_action(cx.listener(Self::down))
                    .on_mouse_down(MouseButton::Left, cx.listener(Self::on_mouse_down))
                    .on_mouse_up(MouseButton::Left, cx.listener(Self::on_mouse_up))
                    .on_mouse_up_out(MouseButton::Left, cx.listener(Self::on_mouse_up))
                    .on_mouse_move(cx.listener(Self::on_mouse_move))
                    .w_full()
                    .pt(px(4.))
                    .pb(px(5.))
                    .text_color(t.ink)
                    .font_family(if self.doc {
                        crate::theme::FONT_DOC
                    } else if self.mono {
                        FONT_MONO
                    } else {
                        FONT_UI
                    })
                    .text_size(px(if self.doc {
                        26.
                    } else if self.mono {
                        13.
                    } else {
                        14.
                    }))
                    .line_height(px(if self.doc { 34. } else { 20. }))
                    // the underline is the field's edge; focus thickens it
                    .border_b(if focused { px(2.) } else { px(1.) })
                    .border_color(if focused {
                        t.accent
                    } else if self.doc {
                        t.rule
                    } else {
                        t.rule_strong
                    })
                    .mb(if focused { px(0.) } else { px(1.) })
                    .child(FieldElement { input: cx.entity() }),
            )
    }
}

impl Focusable for TextField {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}
