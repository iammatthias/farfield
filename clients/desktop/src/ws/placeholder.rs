use crate::theme::theme;
use crate::workspace::Workspace;
use gpui::{div, prelude::*, Context, Window};

pub struct Placeholder(pub &'static str);

impl Workspace for Placeholder {}

impl Render for Placeholder {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx);
        div().p_6().text_color(t.ink_2).child(self.0)
    }
}
