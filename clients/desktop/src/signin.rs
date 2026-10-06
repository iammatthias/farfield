//! "Sign in with passkey": open the keys app's sign-in in the browser, wait
//! for it to hand back a code, redeem it for this Mac's key, and store that
//! key for every service. All waiting happens off the UI thread.
//!
//! The state is app-wide, so onboarding and Settings show the same thing:
//! waiting (with Cancel), then signed in — and the app comes back to the front
//! when the browser is done with it.

use crate::app::{self, log};
use crate::shell::{set_health, toast};
use crate::theme::{theme, S2, S3};
use crate::ui::{self, Kind};
use farfield_core::signin::{self, SignInError};
use gpui::{div, prelude::*, px, AnyElement, App, Global, SharedString};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

#[derive(Clone, Default)]
enum Status {
    #[default]
    Idle,
    Waiting(Arc<AtomicBool>),
    SignedIn(String),
}

#[derive(Default)]
struct SignIn(Status);

impl Global for SignIn {}

fn status(cx: &App) -> Status {
    cx.try_global::<SignIn>().map(|s| s.0.clone()).unwrap_or_default()
}

fn set(cx: &mut App, s: Status) {
    cx.set_global(SignIn(s));
    cx.refresh_windows();
}

/// Services the client uses a key for.
pub fn keyed() -> Vec<String> {
    farfield_core::registry::services()
        .iter()
        .filter(|s| farfield_core::api::probe_path(&s.name).is_some())
        .map(|s| s.name.clone())
        .collect()
}

/// Stop waiting for the browser.
pub fn cancel(cx: &mut App) {
    if let Status::Waiting(flag) = status(cx) {
        flag.store(true, Ordering::Relaxed);
    }
}

/// Run the sign-in; `done(signed_in)` is called on the UI thread at the end.
pub fn run(cx: &mut App, done: impl FnOnce(bool, &mut App) + 'static) {
    if matches!(status(cx), Status::Waiting(_)) {
        return;
    }
    let session = app::session(cx);
    let Some(base) = signin::browser_base(&session.profile) else {
        toast(cx, "No keys service in this profile.", true);
        return done(false, cx);
    };
    let device = signin::device_name();
    let pending = match signin::start(&base, &device) {
        Ok(p) => p,
        Err(e) => {
            toast(cx, format!("Couldn't start sign-in: {e}"), true);
            return done(false, cx);
        }
    };
    log("signin-start", &[("base", &base)]);
    let flag = Arc::new(AtomicBool::new(false));
    set(cx, Status::Waiting(flag.clone()));
    cx.open_url(&pending.authorize_url);
    let s = session.clone();
    let task = farfield_core::spawn(async move {
        let code = {
            let p = &pending;
            tokio::task::block_in_place(|| p.wait_for_code_or(Duration::from_secs(300), &flag))?
        };
        let keys = s.client("keys")?;
        let cred = pending.redeem(&keys, &code).await?;
        for svc in keyed() {
            s.set_credential(&svc, &cred).map_err(SignInError::Io)?;
        }
        Ok::<_, SignInError>(cred.hint())
    });
    cx.spawn(async move |cx| {
        let r = task.await;
        let _ = cx.update(|cx| match r {
            Ok(Ok(hint)) => {
                log("signin-done", &[("hint", &hint)]);
                for svc in keyed() {
                    set_health(cx, &svc, app::Health::Unknown);
                }
                set(cx, Status::SignedIn(device));
                // the browser had the foreground; this is where it went
                cx.activate(true);
                done(true, cx)
            }
            Ok(Err(e)) => {
                log("signin-failed", &[("error", &e.to_string())]);
                set(cx, Status::Idle);
                if !matches!(e, SignInError::Denied | SignInError::Cancelled) {
                    toast(cx, format!("Not signed in: {e}"), true);
                }
                done(false, cx)
            }
            Err(e) => {
                set(cx, Status::Idle);
                toast(cx, e.to_string(), true);
                done(false, cx)
            }
        });
    })
    .detach();
}

/// The sign-in control: a button, then "waiting" with Cancel, then the
/// signed-in line. `start` runs the sign-in (so each caller picks its `done`).
pub fn view(id: &'static str, cx: &App, start: impl Fn(&mut App) + 'static) -> AnyElement {
    let t = theme(cx).clone();
    match status(cx) {
        Status::Idle => div()
            .flex()
            .child(ui::button(SharedString::from(id), "Sign in with passkey", Kind::Primary, cx, move |_, _, cx| {
                start(cx)
            }))
            .into_any_element(),
        Status::Waiting(_) => div()
            .flex()
            .items_center()
            .gap(S3)
            .child(div().flex_none().w(px(7.)).h(px(7.)).rounded_full().bg(t.warn))
            .child(div().text_sm().child("Finish in your browser — approve with your passkey."))
            .child(ui::button(SharedString::from(format!("{id}-cancel")), "Cancel", Kind::Quiet, cx, |_, _, cx| {
                cancel(cx)
            }))
            .into_any_element(),
        Status::SignedIn(device) => div()
            .flex()
            .items_center()
            .gap(S2)
            .child(div().flex_none().w(px(7.)).h(px(7.)).rounded_full().bg(t.good))
            .child(div().text_sm().child(format!("Signed in on {device}.")))
            .child(ui::button(
                SharedString::from(format!("{id}-again")),
                "Sign in again",
                Kind::Quiet,
                cx,
                move |_, _, cx| start(cx),
            ))
            .into_any_element(),
    }
}
