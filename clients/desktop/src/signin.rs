//! "Sign in with passkey": open the keys app's sign-in in the browser, wait
//! for it to hand back a code, redeem it for this Mac's key, and store that
//! key for every service. All waiting happens off the UI thread.

use crate::app::{self, log};
use crate::shell::{set_health, toast};
use farfield_core::signin::{self, SignInError};
use gpui::App;
use std::time::Duration;

/// Services the client uses a key for.
pub fn keyed() -> Vec<String> {
    farfield_core::registry::services()
        .iter()
        .filter(|s| farfield_core::api::probe_path(&s.name).is_some())
        .map(|s| s.name.clone())
        .collect()
}

/// Run the sign-in; `done(signed_in)` is called on the UI thread at the end.
pub fn run(cx: &mut App, done: impl FnOnce(bool, &mut App) + 'static) {
    let session = app::session(cx);
    let Some(base) = signin::browser_base(&session.profile) else {
        toast(cx, "No keys service in this profile.", true);
        return done(false, cx);
    };
    let pending = match signin::start(&base, &signin::device_name()) {
        Ok(p) => p,
        Err(e) => {
            toast(cx, format!("Couldn't start sign-in: {e}"), true);
            return done(false, cx);
        }
    };
    log("signin-start", &[("base", &base)]);
    cx.open_url(&pending.authorize_url);
    let s = session.clone();
    let task = farfield_core::spawn(async move {
        let code = {
            let p = &pending;
            tokio::task::block_in_place(|| p.wait_for_code(Duration::from_secs(300)))?
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
                toast(cx, "Signed in.", false);
                done(true, cx)
            }
            Ok(Err(e)) => {
                log("signin-failed", &[("error", &e.to_string())]);
                if e != SignInError::Denied {
                    toast(cx, format!("Not signed in: {e}"), true);
                }
                done(false, cx)
            }
            Err(e) => {
                toast(cx, e.to_string(), true);
                done(false, cx)
            }
        });
    })
    .detach();
}
