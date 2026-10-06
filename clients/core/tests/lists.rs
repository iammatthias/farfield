//! Integration tests for the lists workspaces — bookmarks, qr, scrap —
//! against the real Go services.

mod fleet;

use farfield_core::api::{bookmarks, ext_lists, qr, scrap};
use farfield_core::ApiError;
use fleet::{block, key, read_key, Fleet};
use serde_json::json;

/// A raw client like the transport's: no redirects followed.
fn raw() -> reqwest::Client {
    reqwest::Client::builder().redirect(reqwest::redirect::Policy::none()).build().unwrap()
}

fn nonce() -> String {
    format!("{}-{}", std::process::id(), farfield_core::store::now_ms())
}

#[test]
fn admin_routes_auth_visibility_and_private_ingress() {
    let f = Fleet::start(&["bookmarks", "qr", "scrap"]);
    let admin = f.admin();
    block(async {
        let private = bookmarks::create(
            &admin,
            &bookmarks::Bookmark {
                url: "http://127.0.0.1:9/private".into(),
                title: "Private one".into(),
                category: "zz".into(),
                public: false,
                admin_notes: "only for me".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap()
        .value;
        let public = bookmarks::create(
            &admin,
            &bookmarks::Bookmark {
                url: "http://127.0.0.1:9/public".into(),
                title: "Public one".into(),
                public: true,
                ..Default::default()
            },
        )
        .await
        .unwrap()
        .value;

        // admin route: both, with notes
        let all = bookmarks::all(&admin).await.unwrap().value;
        let p = all.iter().find(|b| b.id == private.id).expect("private bookmark in admin list");
        assert_eq!(p.admin_notes, "only for me");
        assert!(all.iter().any(|b| b.id == public.id));
        assert_eq!(bookmarks::get(&admin, &private.id).await.unwrap().value.title, "Private one");

        // read key: the public list has only the public one, no notes; the
        // admin routes refuse it
        let (reader, _) = f.session_with(&[
            ("bookmarks", &read_key("bookmarks")),
            ("qr", &read_key("qr")),
            ("scrap", &read_key("scrap")),
        ]);
        let pl = ext_lists::bookmarks::public_list(&reader).await.unwrap();
        assert!(pl.iter().any(|b| b.id == public.id));
        assert!(!pl.iter().any(|b| b.id == private.id), "private bookmark leaked to the public list");
        assert!(pl.iter().all(|b| b.admin_notes.is_empty()));
        assert!(bookmarks::all(&reader).await.unwrap_err().is_auth());
        assert!(qr::all(&reader).await.unwrap_err().is_auth());
        assert!(scrap::list(&reader, 1, 50).await.unwrap_err().is_auth());

        // no key and a wrong key: also 401
        let (anon, _) = f.session_with(&[]);
        assert!(bookmarks::all(&anon).await.unwrap_err().is_auth());
        let (wrong, _) = f.session_with(&[("qr", "nope")]);
        assert!(qr::all(&wrong).await.unwrap_err().is_auth());

        // through the tunnel (Cf-Ray / Cf-Connecting-IP) the admin API does
        // not exist, even with the write key
        let c = raw();
        for (app, path) in
            [("bookmarks", "/api/admin/bookmarks"), ("qr", "/api/admin/codes"), ("scrap", "/api/admin/pastes")]
        {
            let ok = c.get(format!("{}{path}", f.url(app))).header("X-API-Key", key(app)).send().await.unwrap();
            assert_eq!(ok.status(), 200, "{app} admin with key");
            assert_eq!(ok.headers().get("cache-control").and_then(|v| v.to_str().ok()), Some("no-store"), "{app}");
            for h in ["Cf-Ray", "Cf-Connecting-IP"] {
                let r = c
                    .get(format!("{}{path}", f.url(app)))
                    .header("X-API-Key", key(app))
                    .header(h, "x")
                    .send()
                    .await
                    .unwrap();
                assert_eq!(r.status(), 404, "{app} {h}");
            }
        }
    });
}

#[test]
fn bookmark_and_code_edits_are_conditional() {
    let f = Fleet::start(&["bookmarks", "qr"]);
    let admin = f.admin();
    block(async {
        // bookmarks: a stale If-Match is a 412 carrying the server's copy
        let b = bookmarks::create(
            &admin,
            &bookmarks::Bookmark { url: "http://127.0.0.1:9/a".into(), title: "A".into(), ..Default::default() },
        )
        .await
        .unwrap();
        let v1 = b.value.cid.clone();
        assert_eq!(b.etag.as_deref().map(|e| e.trim_matches('"')), Some(v1.as_str()));
        let up = bookmarks::update(&admin, &b.value.id, &json!({"title": "A2"}), Some(&v1)).await.unwrap();
        assert_eq!(up.value.title, "A2");
        assert_eq!(up.value.url, "http://127.0.0.1:9/a", "partial PUT kept the url");
        match bookmarks::update(&admin, &b.value.id, &json!({"title": "A3"}), Some(&v1)).await {
            Err(ApiError::Precondition { current, etag }) => {
                let cur = ext_lists::bookmarks::from_conflict(&current).expect("current bookmark");
                assert_eq!(cur.title, "A2");
                assert_eq!(etag.map(|e| e.trim_matches('"').to_string()), Some(up.value.cid.clone()));
                // reapply against the server's version
                let again = bookmarks::update(&admin, &cur.id, &json!({"title": "A3"}), Some(&cur.cid)).await.unwrap();
                assert_eq!(again.value.title, "A3");
            }
            other => panic!("expected 412, got {other:?}"),
        }
        // delete with a stale tag is refused too
        assert!(matches!(bookmarks::delete(&admin, &b.value.id, Some(&v1)).await, Err(ApiError::Precondition { .. })));

        // qr
        let c = qr::create(
            &admin,
            &qr::Code {
                label: "L".into(),
                mode: "direct".into(),
                target: "hello".into(),
                ec: "M".into(),
                public: true,
                enabled: true,
                ..Default::default()
            },
        )
        .await
        .unwrap()
        .value;
        let up = qr::update(&admin, &c.id, &json!({"target": "hello 2"}), Some(&c.cid)).await.unwrap().value;
        assert!(up.public && up.enabled && up.label == "L", "partial PUT kept the flags: {up:?}");
        match qr::update(&admin, &c.id, &json!({"target": "hello 3"}), Some(&c.cid)).await {
            Err(ApiError::Precondition { current, .. }) => {
                assert_eq!(ext_lists::qr::from_conflict(&current).unwrap().target, "hello 2");
            }
            other => panic!("expected 412, got {other:?}"),
        }
        assert_eq!(ext_lists::qr::get(&admin, &c.id).await.unwrap().value.target, "hello 2");
        qr::delete(&admin, &c.id, Some(&up.cid)).await.unwrap();
        assert_eq!(ext_lists::qr::get(&admin, &c.id).await.unwrap_err(), ApiError::NotFound);
    });
}

#[test]
fn qr_preview_of_a_private_disabled_code() {
    let f = Fleet::start(&["qr"]);
    let admin = f.admin();
    block(async {
        let c = qr::create(
            &admin,
            &qr::Code {
                label: "hidden".into(),
                mode: "proxy".into(),
                target: "https://example.com/x".into(),
                ec: "Q".into(),
                public: false,
                enabled: false,
                ..Default::default()
            },
        )
        .await
        .unwrap()
        .value;
        assert!(!c.public && !c.enabled);
        assert!(qr::all(&admin).await.unwrap().value.iter().any(|x| x.id == c.id), "disabled code in the admin list");

        let png = qr::preview(&admin, &c.id, Some(128)).await.unwrap();
        assert_eq!(&png[..8], b"\x89PNG\r\n\x1a\n");
        let svg = qr::preview(&admin, &c.id, None).await.unwrap();
        assert!(String::from_utf8_lossy(&svg).contains("<svg"));

        // the public scan image and redirect do not serve it
        let c2 = raw();
        let url = ext_lists::qr::public_png_url(&admin, &c.id).unwrap();
        assert_eq!(c2.get(&url).send().await.unwrap().status(), 404);
        let r = qr::redirect_url(&admin, &c.id).unwrap();
        assert_eq!(c2.get(&r).send().await.unwrap().status(), 404);

        // once public and enabled, both work; the proxy code redirects
        let up =
            qr::update(&admin, &c.id, &json!({"public": true, "enabled": true}), Some(&c.cid)).await.unwrap().value;
        assert!(up.public && up.enabled);
        assert_eq!(c2.get(&url).send().await.unwrap().status(), 200);
        let red = c2.get(&r).send().await.unwrap();
        assert_eq!(red.status(), 302);
        assert_eq!(red.headers().get("location").and_then(|v| v.to_str().ok()), Some("https://example.com/x"));
    });
}

#[test]
fn scrap_magic_links_expiry_and_minted_keys() {
    let f = Fleet::start(&["scrap", "keys"]);
    let admin = f.admin();
    let base = f.url("scrap");
    block(async {
        let body = format!("fn main() {{}} // {}", nonce());
        let made = scrap::create(&admin, &body, "Snippet", "rust", "public", "1d", true).await.unwrap();
        let token = made.token.clone().expect("token shown once");
        let c = raw();
        let raw_url = |t: Option<&str>| match t {
            Some(t) => format!("{base}/{}/raw?t={t}", made.id),
            None => format!("{base}/{}/raw", made.id),
        };

        // a token forces public → unlisted, and the admin read sees it as stored
        let p = scrap::get(&admin, &made.id).await.unwrap().value;
        assert_eq!(p.visibility, "unlisted");
        assert!(p.has_token);
        assert_eq!(p.body, body);
        assert!(!p.expires_at.is_empty());
        let listed = scrap::list(&admin, 1, 50).await.unwrap().value;
        assert!(listed.total >= 1);
        assert!(listed.pastes.iter().any(|x| x.id == made.id && x.body.is_empty()));

        // the share link carries the token right after creation
        let share = scrap::share_url(&admin, &made.id, Some(&token)).unwrap();
        assert!(share.contains(&made.id) && share.contains(&token));

        assert_eq!(c.get(raw_url(Some(&token))).send().await.unwrap().text().await.unwrap(), body);
        assert_eq!(c.get(raw_url(None)).send().await.unwrap().status(), 401, "no token → locked");

        // rotate: the old token stops working at once, the new one works
        let fresh = scrap::rotate_token(&admin, &made.id).await.unwrap();
        assert_ne!(fresh, token);
        assert_eq!(c.get(raw_url(Some(&token))).send().await.unwrap().status(), 403, "old token after rotate");
        assert_eq!(c.get(raw_url(Some(&fresh))).send().await.unwrap().status(), 200);

        // revoke: the paste serves per its visibility again — unlisted, so
        // anyone with the link reads it without a token
        scrap::revoke_token(&admin, &made.id).await.unwrap();
        assert!(!scrap::get(&admin, &made.id).await.unwrap().value.has_token);
        assert_eq!(
            c.get(raw_url(None)).send().await.unwrap().status(),
            200,
            "unlisted after revoke reads without a token"
        );
        // nothing to rotate or revoke now
        assert!(matches!(scrap::rotate_token(&admin, &made.id).await, Err(ApiError::Conflict { .. })));
        assert_eq!(scrap::revoke_token(&admin, &made.id).await.unwrap_err(), ApiError::NotFound);

        // a private paste without a token needs a session, which a raw
        // reader has none of
        let up = scrap::update(&admin, &made.id, &json!({"visibility": "private"})).await.unwrap().value;
        assert_eq!(up.visibility, "private");
        assert_eq!(c.get(raw_url(None)).send().await.unwrap().status(), 401);

        // expiry: set, clear, refuse unknown; an expired paste is "expired"
        let up = scrap::update(&admin, &made.id, &json!({"expires": "1h", "title": "  Renamed  ", "lang": " GO "}))
            .await
            .unwrap()
            .value;
        assert!(!up.expires_at.is_empty());
        assert!(!ext_lists::scrap::expired_now(&up.expires_at));
        assert_eq!(up.title, "Renamed");
        assert_eq!(up.lang, "go");
        let up = scrap::update(&admin, &made.id, &json!({"expires": "never"})).await.unwrap().value;
        assert_eq!(up.expires_at, "");
        assert!(matches!(
            scrap::update(&admin, &made.id, &json!({"expires": "forever"})).await,
            Err(ApiError::BadRequest(_))
        ));
        assert_eq!(
            scrap::update(&admin, "aaaaaaaaaaaaaaaa", &json!({"title": "x"})).await.unwrap_err(),
            ApiError::NotFound
        );

        // a minted write key works for writes and the admin API, then
        // revocation is immediate
        let (tok, id) = f.mint("lists test", "scrap", "write");
        let (minted, _) = f.session_with(&[("scrap", &tok)]);
        let b2 = format!("minted {}", nonce());
        let m = scrap::create(&minted, &b2, "", "", "unlisted", "never", false).await.unwrap();
        assert!(m.token.is_none());
        assert!(scrap::list(&minted, 1, 50).await.is_ok());
        f.revoke(&id);
        let (after, _) = f.session_with(&[("scrap", &tok)]);
        assert!(scrap::list(&after, 1, 50).await.unwrap_err().is_auth(), "revoked key on admin list");
        assert!(scrap::create(&after, &format!("x {}", nonce()), "", "", "unlisted", "never", false)
            .await
            .unwrap_err()
            .is_auth());
        // and a read-scoped minted key never reaches the admin API
        let (rtok, _) = f.mint("lists read", "scrap", "read");
        let (rs, _) = f.session_with(&[("scrap", &rtok)]);
        assert!(scrap::list(&rs, 1, 50).await.unwrap_err().is_auth());

        // delete, then it is gone
        scrap::delete(&admin, &made.id).await.unwrap();
        assert_eq!(scrap::get(&admin, &made.id).await.unwrap_err(), ApiError::NotFound);
    });
}

#[test]
fn scrap_list_pages() {
    let f = Fleet::start(&["scrap"]);
    let admin = f.admin();
    block(async {
        let n = nonce();
        for i in 0..5 {
            scrap::create(&admin, &format!("paste {i} {n}"), &format!("p{i}"), "", "unlisted", "never", false)
                .await
                .unwrap();
        }
        let p1 = scrap::list(&admin, 1, 2).await.unwrap().value;
        let p2 = scrap::list(&admin, 2, 2).await.unwrap().value;
        let p3 = scrap::list(&admin, 3, 2).await.unwrap().value;
        assert_eq!(p1.total, 5);
        assert_eq!(p1.pastes.len(), 2);
        assert_eq!(p3.pastes.len(), 1);
        // createdAt has one-second resolution, so order within a second is
        // the server's; the pages still partition the set
        let mut ids: Vec<String> = [p1, p2, p3].iter().flat_map(|p| p.pastes.iter().map(|x| x.id.clone())).collect();
        ids.sort();
        ids.dedup();
        assert_eq!(ids.len(), 5);
    });
}

#[test]
fn outage_serves_the_cached_list_then_recovers() {
    let f = Fleet::start(&["bookmarks"]);
    let admin = f.admin();
    block(async {
        bookmarks::create(
            &admin,
            &bookmarks::Bookmark { url: "http://127.0.0.1:9/c".into(), title: "Cached".into(), ..Default::default() },
        )
        .await
        .unwrap();
        let live = bookmarks::all(&admin).await.unwrap();
        assert_eq!(live.freshness, farfield_core::Freshness::Live);
    });
    f.stop("bookmarks");
    block(async {
        let stale = bookmarks::all(&admin).await.unwrap();
        assert!(matches!(stale.freshness, farfield_core::Freshness::Stale { .. }), "{:?}", stale.freshness);
        assert!(stale.value.iter().any(|b| b.title == "Cached"));
        // a write while down is offline, not a crash, and never retried
        let e = bookmarks::update(&admin, &stale.value[0].id, &json!({"title": "x"}), None).await.unwrap_err();
        assert!(e.is_offline(), "{e:?}");
    });
    f.restart("bookmarks");
    block(async {
        assert_eq!(bookmarks::all(&admin).await.unwrap().freshness, farfield_core::Freshness::Live);
    });
}
