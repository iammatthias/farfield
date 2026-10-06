//! switchboard — stub, replaced by the real workspace.
use crate::theme::theme;
use crate::workspace::Workspace;
use gpui::{div, prelude::*, Context, Window};

pub struct SwitchboardWs;

impl SwitchboardWs {
    pub fn new(_w: &mut Window, _cx: &mut Context<Self>) -> Self {
        SwitchboardWs
    }
}

impl Workspace for SwitchboardWs {}

impl Render for SwitchboardWs {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        div().p_6().text_color(theme(cx).ink_2).child("switchboard")
    }
}
