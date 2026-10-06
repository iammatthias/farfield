//! pulse — stub, replaced by the real workspace.
use crate::theme::theme;
use crate::workspace::Workspace;
use gpui::{div, prelude::*, Context, Window};

pub struct PulseWs;

impl PulseWs {
    pub fn new(_w: &mut Window, _cx: &mut Context<Self>) -> Self {
        PulseWs
    }
}

impl Workspace for PulseWs {}

impl Render for PulseWs {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        div().p_6().text_color(theme(cx).ink_2).child("pulse")
    }
}
