//! Integration tests: the client against the real Go services.

mod fleet;

use farfield_core::api::{blobs, content, feed};
use farfield_core::store::SaveState;
use farfield_core::sync::{self, ContentEntry, Resolution, SaveOutcome};
use farfield_core::upload::Progress;
use farfield_core::{ApiError, Freshness};
use fleet::{block, key, read_key, Fleet};
use serde_json::json;

fn entry(title: &str, body: &str, published: bool) -> content::Entry {
    content::Entry {
        collection: "notes".into(),
        title: title.into(),
        body: body.into(),
        published,
        tags: vec!["t".into()],
        ..Default::default()
    }
}

/// The content app ships no collection API; the session console creates
/// them. Use it the way a person would.
fn make_collection(f: &Fleet, slug: &str) {
    let url = f.url("content");
    let jar = f.data.path().join("content-cookies.txt");
    let st = std::process::Command::new("curl")
        .args(["-s", "-o", "/dev/null", "-c"])
        .arg(&jar)
        .args(["-d", "password=demo", &format!("{url}/login")])
        .status()
        .unwrap();
    assert!(st.success());
    let out = std::process::Command::new("curl")
        .args(["-s", "-o", "/dev/null", "-w", "%{http_code}", "-b"])
        .arg(&jar)
        .args(["-d", &format!("name={slug}&slug={slug}&description=test"), &format!("{url}/collections")])
        .output()
        .unwrap();
    let code = String::from_utf8_lossy(&out.stdout).to_string();
    assert!(code.starts_with('2') || code.starts_with('3'), "create collection: {code}");
}

#[test]
fn auth_and_visibility() {
    let f = Fleet::start(&["content", "keys"]);
    make_collection(&f, "notes");
    let admin = f.admin();
    block(async {
        let draft = content::create(&admin, &entry("Draft one", "secret draft", false)).await.unwrap();
        let public = content::create(&admin, &entry("Public one", "hello", true)).await.unwrap();

        // no key: the read API refuses (a read key is configured)
        let (anon, _) = f.session_with(&[]);
        let err = content::entries(&anon, None, content::Status::Published, 1, 50).await.unwrap_err();
        assert!(err.is_auth(), "{err:?}");

        // read key: published only; drafts need the write key
        let (reader, _) = f.session_with(&[("content", &read_key("content"))]);
        let p = content::entries(&reader, None, content::Status::Published, 1, 50).await.unwrap();
        assert!(p.value.items.iter().any(|e| e.slug == public.value.slug));
        assert!(!p.value.items.iter().any(|e| e.slug == draft.value.slug));
        assert!(matches!(content::entries(&reader, None, content::Status::Drafts, 1, 50).await, Err(ApiError::Forbidden(_))));
        assert_eq!(content::entry(&reader, &draft.value.slug).await.unwrap_err(), ApiError::NotFound);

        // write key sees drafts
        let d = content::entries(&admin, None, content::Status::Drafts, 1, 50).await.unwrap();
        assert!(d.value.items.iter().any(|e| e.slug == draft.value.slug));

        // a wrong key is "not signed in", never a crash or a redirect followed
        let (wrong, _) = f.session_with(&[("content", "nope-not-a-key")]);
        assert!(content::entries(&wrong, None, content::Status::Published, 1, 5).await.unwrap_err().is_auth());
    });
}

#[test]
fn minted_keys_and_revocation() {
    let f = Fleet::start(&["content", "keys"]);
    make_collection(&f, "notes");
    let (token, id) = f.mint("desktop test", "content", "write");
    let (s, _) = f.session_with(&[("content", &token)]);
    block(async {
        content::create(&s, &entry("Via ffk", "x", false)).await.unwrap();
    });
    f.revoke(&id);
    // a fresh session (no cache) with the revoked key is refused at once
    let (s2, _) = f.session_with(&[("content", &token)]);
    block(async {
        let e = content::entries(&s2, None, content::Status::Drafts, 1, 5).await.unwrap_err();
        assert!(e.is_auth(), "revoked key still accepted: {e:?}");
        // and the cached session cannot keep using cached private data either
        let e = content::entries(&s, None, content::Status::Drafts, 1, 5).await.unwrap_err();
        assert!(e.is_auth(), "{e:?}");
    });
}

#[test]
fn pagination_and_etag_revalidation() {
    let f = Fleet::start(&["content", "feed"]);
    make_collection(&f, "notes");
    let s = f.admin();
    block(async {
        for i in 0..7 {
            content::create(&s, &entry(&format!("E{i}"), "b", true)).await.unwrap();
            feed::create(&s, &format!("post {i}"), &[]).await.unwrap();
        }
        // content: page/limit, a short page is the end
        let p1 = content::entries(&s, Some("notes"), content::Status::All, 1, 3).await.unwrap();
        let p3 = content::entries(&s, Some("notes"), content::Status::All, 3, 3).await.unwrap();
        assert_eq!(p1.value.items.len(), 3);
        assert!(p1.value.has_more);
        assert_eq!(p3.value.items.len(), 1);
        assert!(!p3.value.has_more);
        // second read revalidates: the server answers 304 and the cache serves
        let again = content::entries(&s, Some("notes"), content::Status::All, 1, 3).await.unwrap();
        assert_eq!(again.freshness, Freshness::Live);
        assert_eq!(again.etag, p1.etag);
        assert_eq!(again.value.items.len(), 3);

        // feed: keyset cursor from the last post
        let (a, more) = feed::posts(&s, None, 4).await.unwrap().value;
        assert!(more);
        let (b, more2) = feed::posts(&s, Some(&a.last().unwrap().cursor()), 4).await.unwrap().value;
        assert!(!more2);
        assert_eq!(a.len() + b.len(), 7);
        assert!(a.iter().all(|p| !b.iter().any(|q| q.slug == p.slug)), "pages overlap");
    });
}

#[test]
fn outage_serves_stale_and_says_so() {
    let f = Fleet::start(&["feed"]);
    let s = f.admin();
    block(async {
        feed::create(&s, "before the outage", &[]).await.unwrap();
        let live = feed::posts(&s, None, 10).await.unwrap();
        assert_eq!(live.freshness, Freshness::Live);
    });
    f.stop("feed");
    block(async {
        let stale = feed::posts(&s, None, 10).await.unwrap();
        assert!(matches!(stale.freshness, Freshness::Stale { error: ApiError::Offline(_), .. }), "{:?}", stale.freshness);
        assert_eq!(stale.value.0.len(), 1);
        // a write during the outage fails as offline, never as "saved"
        let e = feed::create(&s, "during", &[]).await.unwrap_err();
        assert!(e.is_offline(), "{e:?}");
        // unaffected: another service's client still constructs and works
        // independently (covered by the fleet tests per service)
    });
    f.restart("feed");
    block(async {
        let back = feed::posts(&s, None, 10).await.unwrap();
        assert_eq!(back.freshness, Freshness::Live);
    });
}

#[test]
fn blob_upload_progress_cancel_and_media_post() {
    let f = Fleet::start(&["feed"]);
    let s = f.admin();
    let dir = tempfile::tempdir().unwrap();
    // a real PNG so the server's image path runs
    let png = dir.path().join("pixel.png");
    std::fs::write(&png, PNG_1X1).unwrap();
    block(async {
        let p = Progress::new(0);
        let m = blobs::upload(&s, &png, &p).await.unwrap();
        assert_eq!(p.sent(), PNG_1X1.len() as u64);
        assert!(m.value.cid.starts_with('b'));
        assert_eq!(m.value.mime, "image/png");
        // idempotent by content
        let again = blobs::upload(&s, &png, &Progress::new(0)).await.unwrap();
        assert_eq!(again.value.cid, m.value.cid);
        let page = blobs::list(&s, 1).await.unwrap();
        assert!(page.value.blobs.iter().any(|b| b.cid == m.value.cid));
        let bytes = blobs::bytes(&s, &m.value.cid, 1 << 20).await.unwrap();
        assert!(!bytes.is_empty());

        // cancellation stops a large upload and reports Cancelled
        let big = dir.path().join("big.bin");
        std::fs::write(&big, vec![7u8; 24 << 20]).unwrap();
        let p = Progress::new(0);
        let p2 = p.clone();
        let canceller = tokio::spawn(async move {
            while p2.sent() == 0 {
                tokio::time::sleep(std::time::Duration::from_millis(5)).await;
            }
            p2.cancel();
        });
        let r = blobs::upload(&s, &big, &p).await;
        canceller.await.unwrap();
        assert_eq!(r.unwrap_err(), ApiError::Cancelled);
        assert!(p.sent() < 24 << 20);

        // feed media post through the shared multipart path
        let post = feed::create_with_media(&s, "a photo", &["pics".into()], &[png.clone()], &Progress::new(0)).await.unwrap();
        assert!(post.value.body.contains(&format!("blob://{}", m.value.cid)), "{}", post.value.body);
        feed::delete(&s, &post.value.slug, true, None).await.unwrap();
    });
}

#[test]
fn draft_recovery_across_relaunch() {
    let f = Fleet::start(&["content"]);
    make_collection(&f, "notes");
    let s = f.admin();
    let local = serde_json::to_value(entry("Unsaved", "typed but not sent ![](blob://bafkexample)", false)).unwrap();
    let key = {
        let mut d = sync::new_draft("content", local.clone());
        sync::edit(&s, &mut d, local.clone()).unwrap();
        s.drafts("content").unwrap().save(&d).unwrap();
        d.key.clone()
    };
    drop(s); // "quit"
    let s = f.admin(); // relaunch
    let found = s.drafts("content").unwrap().list("content");
    assert_eq!(found.len(), 1);
    assert_eq!(found[0].key, key);
    assert_eq!(found[0].state, SaveState::Local);
    assert_eq!(found[0].local, local);
    // and it saves: the local key is replaced by the server's slug
    let mut d = found[0].clone();
    block(async {
        assert_eq!(sync::save::<ContentEntry>(&s, &mut d).await.unwrap(), SaveOutcome::Saved);
    });
    assert_eq!(d.state, SaveState::Saved);
    assert!(!d.key.starts_with("new-"));
    assert!(s.drafts("content").unwrap().load("content", &key).is_none());
    let _ = Resolution::Merge;
    let _ = json!(null);
    let _ = key;
}

const PNG_1X1: &[u8] = &[
    0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00,
    0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00, 0x0D, 0x49,
    0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0xF8, 0xCF, 0xC0, 0xF0, 0x1F, 0x00, 0x05, 0x00, 0x01, 0xFF, 0x89, 0x99, 0x3D,
    0x1D, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
];

#[allow(dead_code)]
fn unused() {
    let _ = key("x");
}
