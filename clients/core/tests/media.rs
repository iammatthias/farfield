//! Integration tests for the media workspaces (feed and blobs): the client
//! against the real Go services.
//!
//! Note on guarded deletes: blobs decides "unreferenced" by scanning content
//! and feed. The fleet harness wires CONTENT_URL for it but not FEED_URL /
//! FEED_READ_KEY, so its feed scan goes to the default 127.0.0.1:8788. That
//! makes "it really was an orphan, so it went" unprovable here; these tests
//! assert the parts that do not depend on it (a referenced blob is always
//! kept, with its count when the server can give one).

mod fleet;

use farfield_core::api::ext_media::{self, Release};
use farfield_core::api::{blobs, content, feed};
use farfield_core::store::SaveState;
use farfield_core::sync::{self, FeedPost, SaveOutcome};
use farfield_core::upload::Progress;
use farfield_core::{ApiError, Freshness};
use fleet::{block, read_key, Fleet};
use std::path::{Path, PathBuf};
use std::time::Duration;

const PNG_1X1: &[u8] = &[
    0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00,
    0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00, 0x0D, 0x49,
    0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0xF8, 0xCF, 0xC0, 0xF0, 0x1F, 0x00, 0x05, 0x00, 0x01, 0xFF, 0x89, 0x99, 0x3D,
    0x1D, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
];

fn png(dir: &Path) -> PathBuf {
    let p = dir.join("pixel.png");
    std::fs::write(&p, PNG_1X1).unwrap();
    p
}

/// The content app ships no collection API; its console creates them.
fn make_collection(f: &Fleet, slug: &str) {
    let url = f.url("content");
    let jar = f.data.path().join("media-content-cookies.txt");
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
fn feed_paging_cursor_and_revalidation() {
    let f = Fleet::start(&["feed"]);
    let s = f.admin();
    block(async {
        for i in 0..8 {
            feed::create(&s, &format!("post {i}"), &[]).await.unwrap();
        }
        // keyset pages of 4: 4, 4, then an empty page ends it — a full last
        // page cannot know it is the last, so the client must treat an empty
        // append as the end
        let p1 = feed::posts(&s, None, 4).await.unwrap();
        let (a, more) = p1.value.clone();
        assert_eq!(a.len(), 4);
        assert!(more);
        // newest first (createdAt is to the second; ties order by slug)
        assert!(a.windows(2).all(|w| w[0].created_at >= w[1].created_at), "newest first");
        let (b, more) = feed::posts(&s, Some(&a.last().unwrap().cursor()), 4).await.unwrap().value;
        assert_eq!(b.len(), 4);
        assert!(more);
        let (c, more) = feed::posts(&s, Some(&b.last().unwrap().cursor()), 4).await.unwrap().value;
        assert!(c.is_empty());
        assert!(!more);
        let all: Vec<&str> = a.iter().chain(b.iter()).map(|p| p.slug.as_str()).collect();
        let uniq: std::collections::HashSet<&&str> = all.iter().collect();
        assert_eq!(uniq.len(), 8, "pages overlap: {all:?}");

        // revalidation: same version → 304 → served from cache, Live
        let again = feed::posts(&s, None, 4).await.unwrap();
        assert_eq!(again.freshness, Freshness::Live);
        assert_eq!(again.etag, p1.etag);
        assert_eq!(again.value.0, a);
        // a new post changes the version
        feed::create(&s, "post 8", &[]).await.unwrap();
        let fresh = feed::posts(&s, None, 4).await.unwrap();
        assert_ne!(fresh.etag, p1.etag);

        // trailing #hashtags become tags
        let (body, tags) = feed::split_hashtags("sunset over the bay\n#photo #SF");
        let p = feed::create(&s, &body, &tags).await.unwrap();
        assert_eq!(p.value.body, "sunset over the bay");
        assert_eq!(p.value.tags, vec!["photo".to_string(), "sf".to_string()]);
    });
}

#[test]
fn media_auth_read_vs_write() {
    let f = Fleet::start(&["feed"]);
    let dir = tempfile::tempdir().unwrap();
    let file = png(dir.path());
    block(async {
        let admin = f.admin();
        feed::create(&admin, "hello", &[]).await.unwrap();
        blobs::upload(&admin, &file, &Progress::new(PNG_1X1.len() as u64)).await.unwrap();

        // no key: both read APIs refuse
        let (anon, _) = f.session_with(&[]);
        assert!(feed::posts(&anon, None, 10).await.unwrap_err().is_auth());
        assert!(blobs::list(&anon, 1).await.unwrap_err().is_auth());
        // read key: reads, cannot write
        let (reader, _) = f.session_with(&[("feed", &read_key("feed")), ("blobs", &read_key("blobs"))]);
        assert_eq!(feed::posts(&reader, None, 10).await.unwrap().value.0.len(), 1);
        assert!(blobs::list(&reader, 1).await.unwrap().value.total >= 1);
        assert!(feed::create(&reader, "nope", &[]).await.unwrap_err().is_auth());
        assert!(blobs::upload(&reader, &file, &Progress::new(0)).await.unwrap_err().is_auth());
        // wrong key: "not signed in"
        let (wrong, _) = f.session_with(&[("feed", "not-a-key"), ("blobs", "not-a-key")]);
        assert!(feed::posts(&wrong, None, 10).await.unwrap_err().is_auth());
        assert!(blobs::list(&wrong, 1).await.unwrap_err().is_auth());
    });
}

#[test]
fn media_post_with_progress() {
    let f = Fleet::start(&["feed"]);
    let s = f.admin();
    let dir = tempfile::tempdir().unwrap();
    let a = png(dir.path());
    let b = dir.path().join("notes.txt");
    std::fs::write(&b, b"not an image, still media").unwrap();
    let total = std::fs::metadata(&a).unwrap().len() + std::fs::metadata(&b).unwrap().len();
    block(async {
        let p = Progress::new(total);
        let post = feed::create_with_media(&s, "two attachments", &["pics".into()], &[a.clone(), b.clone()], &p)
            .await
            .unwrap();
        assert_eq!(p.sent(), total, "every byte counted");
        assert!((p.fraction() - 1.0).abs() < f32::EPSILON);
        let refs = ext_media::blob_refs(&post.value.body);
        assert_eq!(refs.len(), 2, "{}", post.value.body);
        assert_eq!(ext_media::strip_embeds(&post.value.body), "two attachments");
        assert_eq!(post.value.tags, vec!["pics".to_string()]);
        assert!(post.etag.is_some(), "a created post carries its version");
        // the stored media is in blobs, in order
        let m = blobs::meta(&s, &refs[0]).await.unwrap().value;
        assert_eq!(m.mime, "image/png");
        assert_eq!(ext_media::MediaKind::of(&m.mime), ext_media::MediaKind::Image);
        assert_eq!(ext_media::thumb_source(&m).as_deref(), Some(refs[0].as_str()), "a 1px image is its own thumbnail");
        let bytes = blobs::bytes(&s, &refs[0], 1 << 20).await.unwrap();
        assert_eq!(&bytes[..], PNG_1X1);
        // and it shows in the timeline
        let (posts, _) = feed::posts(&s, None, 10).await.unwrap().value;
        assert_eq!(posts[0].slug, post.value.slug);
    });
}

#[test]
fn post_edit_conflict_between_two_sessions() {
    let f = Fleet::start(&["feed"]);
    let a = f.admin();
    let b = f.admin();
    block(async {
        let created = feed::create(&a, "original", &["one".into()]).await.unwrap();
        let slug = created.value.slug.clone();
        let v0 = created.etag.clone().expect("etag");

        // both open the post as a draft (what DraftDoc<FeedPost> does)
        let mut da = sync::open::<FeedPost>(&a, &slug).await.unwrap();
        let mut db = sync::open::<FeedPost>(&b, &slug).await.unwrap();
        assert_eq!(da.base_etag.as_deref(), Some(v0.as_str()));

        // B saves first
        db.local["body"] = "edited by B".into();
        db.state = SaveState::Local;
        assert_eq!(sync::save::<FeedPost>(&b, &mut db).await.unwrap(), SaveOutcome::Saved);
        assert_ne!(db.base_etag.as_deref(), Some(v0.as_str()));

        // A's write is conditional on v0: refused with the current post
        let e = feed::update(&a, &slug, "edited by A", &[], Some(&v0)).await.unwrap_err();
        match &e {
            ApiError::Precondition { current, etag } => {
                assert_eq!(current["body"], "edited by B");
                assert_eq!(etag.as_deref(), db.base_etag.as_deref());
            }
            other => panic!("want 412, got {other:?}"),
        }
        // through the sync layer: a conflict with B's copy beside A's
        da.local["body"] = "edited by A".into();
        da.state = SaveState::Local;
        assert_eq!(sync::save::<FeedPost>(&a, &mut da).await.unwrap(), SaveOutcome::Conflict);
        assert_eq!(da.state, SaveState::Conflict);
        assert_eq!(da.remote.as_ref().unwrap()["body"], "edited by B");
        // the server still has B's version: nothing was overwritten
        assert_eq!(feed::post(&a, &slug).await.unwrap().value.body, "edited by B");

        // keep mine, explicitly → saved over B's version
        assert_eq!(
            sync::resolve::<FeedPost>(&a, &mut da, sync::Resolution::KeepMine).await.unwrap(),
            SaveOutcome::Saved
        );
        assert_eq!(feed::post(&a, &slug).await.unwrap().value.body, "edited by A");

        // a delete conditional on a stale version is refused too
        let e = feed::delete(&b, &slug, false, db.base_etag.as_deref()).await.unwrap_err();
        assert!(matches!(e, ApiError::Precondition { .. }), "{e:?}");
        feed::delete(&a, &slug, false, da.base_etag.as_deref()).await.unwrap();
        assert_eq!(feed::post(&a, &slug).await.unwrap_err(), ApiError::NotFound);
    });
}

#[test]
fn delete_with_media_release_keeps_a_blob_referenced_elsewhere() {
    let f = Fleet::start(&["feed", "content"]);
    make_collection(&f, "notes");
    let s = f.admin();
    let dir = tempfile::tempdir().unwrap();
    let file = png(dir.path());
    block(async {
        let post = feed::create_with_media(
            &s,
            "shared photo",
            &[],
            std::slice::from_ref(&file),
            &Progress::new(PNG_1X1.len() as u64),
        )
        .await
        .unwrap();
        let cid = ext_media::blob_refs(&post.value.body).remove(0);
        // the same photo also sits in an essay
        let entry = content::Entry {
            collection: "notes".into(),
            title: "Essay".into(),
            body: format!("words\n\n![](blob://{cid})"),
            published: true,
            ..Default::default()
        };
        content::create(&s, &entry).await.unwrap();

        // taking the post back with its media: the post goes, the photo stays
        feed::delete(&s, &post.value.slug, true, post.etag.as_deref()).await.unwrap();
        assert_eq!(feed::post(&s, &post.value.slug).await.unwrap_err(), ApiError::NotFound);
        // release runs in the background on the server: give it time to act
        tokio::time::sleep(Duration::from_millis(1500)).await;
        let m = blobs::meta(&s, &cid).await.expect("a referenced blob must survive a media release");
        assert_eq!(m.value.cid, cid);

        // asking blobs directly: kept, with the reason (and the count when
        // the server can see every source)
        match ext_media::delete_unless_referenced(&s, &cid).await.unwrap() {
            Release::Kept { references, message } => {
                assert!(message.contains("kept"), "{message}");
                if let Some(n) = references {
                    assert!(n >= 1, "{n}");
                }
                eprintln!("kept: references={references:?} message={message:?}");
            }
            Release::Deleted => panic!("a referenced blob was deleted"),
        }
        // the raw error carries the 409
        let e = blobs::delete(&s, &cid, true).await.unwrap_err();
        assert!(matches!(e, ApiError::Conflict { .. }), "{e:?}");
        assert!(blobs::meta(&s, &cid).await.is_ok());
    });
}

#[test]
fn blobs_list_paging_and_total() {
    let f = Fleet::start(&["blobs"]);
    let s = f.admin();
    let dir = tempfile::tempdir().unwrap();
    block(async {
        let mut cids = std::collections::HashSet::new();
        for i in 0..50 {
            let p = dir.path().join(format!("f{i}.txt"));
            std::fs::write(&p, format!("blob number {i}")).unwrap();
            let len = std::fs::metadata(&p).unwrap().len();
            let pr = Progress::new(len);
            let m = blobs::upload(&s, &p, &pr).await.unwrap();
            assert_eq!(pr.sent(), len);
            cids.insert(m.value.cid);
        }
        assert_eq!(cids.len(), 50);
        let p1 = blobs::list(&s, 1).await.unwrap().value;
        assert_eq!(p1.total, 50);
        assert_eq!(p1.page, 1);
        assert_eq!(p1.pages, (50 + ext_media::BLOBS_PAGE - 1) / ext_media::BLOBS_PAGE);
        assert_eq!(p1.blobs.len() as i64, ext_media::BLOBS_PAGE);
        let p2 = blobs::list(&s, 2).await.unwrap().value;
        assert_eq!(p2.page, 2);
        assert_eq!(p2.blobs.len(), 2);
        let seen: std::collections::HashSet<String> =
            p1.blobs.iter().chain(p2.blobs.iter()).map(|b| b.cid.clone()).collect();
        assert_eq!(seen, cids, "the two pages are every blob, once");
        // past the end: empty, not an error
        assert!(blobs::list(&s, 3).await.unwrap().value.blobs.is_empty());
        // non-images have no thumbnail source
        assert!(p1.blobs.iter().all(|b| ext_media::thumb_source(b).is_none()));
    });
}

#[test]
fn blobs_delete_unless_referenced() {
    let f = Fleet::start(&["content"]);
    make_collection(&f, "notes");
    let s = f.admin();
    let dir = tempfile::tempdir().unwrap();
    block(async {
        let a = dir.path().join("a.txt");
        std::fs::write(&a, b"referenced by an essay").unwrap();
        let b = dir.path().join("b.txt");
        std::fs::write(&b, b"referenced by nothing").unwrap();
        let ma = blobs::upload(&s, &a, &Progress::new(0)).await.unwrap().value;
        let mb = blobs::upload(&s, &b, &Progress::new(0)).await.unwrap().value;
        let mut entry = content::Entry { collection: "notes".into(), title: "Refs".into(), ..Default::default() };
        entry.body = format!("{}\n", ma.markdown("a"));
        content::create(&s, &entry).await.unwrap();

        // referenced → 409, kept, never deleted
        let r = ext_media::delete_unless_referenced(&s, &ma.cid).await.unwrap();
        assert!(matches!(r, Release::Kept { .. }), "{r:?}");
        assert!(blobs::meta(&s, &ma.cid).await.is_ok());

        // unreferenced: deleted when blobs can see every source; kept (and
        // says why) when it cannot — never a guess
        match ext_media::delete_unless_referenced(&s, &mb.cid).await.unwrap() {
            Release::Deleted => assert_eq!(blobs::meta(&s, &mb.cid).await.unwrap_err(), ApiError::NotFound),
            Release::Kept { references, message } => {
                assert_eq!(references, None, "nothing references it");
                assert!(message.contains("cannot confirm"), "{message}");
                // the explicit, unguarded delete takes it
                blobs::delete(&s, &mb.cid, false).await.unwrap();
                assert_eq!(blobs::meta(&s, &mb.cid).await.unwrap_err(), ApiError::NotFound);
            }
        }
        assert_eq!(blobs::bytes(&s, &mb.cid, 1 << 20).await.unwrap_err(), ApiError::NotFound);
        // deleting it again: not found, not a crash
        assert_eq!(blobs::delete(&s, &mb.cid, false).await.unwrap_err(), ApiError::NotFound);
        // a read key cannot delete
        let (reader, _) = f.session_with(&[("blobs", &read_key("blobs"))]);
        assert!(blobs::delete(&reader, &ma.cid, true).await.unwrap_err().is_auth());
    });
}

#[test]
fn upload_cancel_blob_and_media_post() {
    let f = Fleet::start(&["feed"]);
    let s = f.admin();
    let dir = tempfile::tempdir().unwrap();
    let big = dir.path().join("big.bin");
    std::fs::write(&big, vec![7u8; 24 << 20]).unwrap();
    let cancel_when_started = |p: &Progress| {
        let p2 = p.clone();
        tokio::spawn(async move {
            while p2.sent() == 0 {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
            p2.cancel();
        })
    };
    block(async {
        let before_total = blobs::list(&s, 1).await.unwrap().value.total;

        // a blob upload
        let p = Progress::new(24 << 20);
        let c = cancel_when_started(&p);
        let r = blobs::upload(&s, &big, &p).await;
        c.await.unwrap();
        assert_eq!(r.unwrap_err(), ApiError::Cancelled);
        assert!(p.sent() < 24 << 20);

        // a media post
        let p = Progress::new(24 << 20);
        let c = cancel_when_started(&p);
        let r = feed::create_with_media(&s, "never posted", &[], std::slice::from_ref(&big), &p).await;
        c.await.unwrap();
        assert_eq!(r.unwrap_err(), ApiError::Cancelled);
        assert!(p.sent() < 24 << 20);

        // neither left anything behind
        let (posts, _) = feed::posts(&s, None, 10).await.unwrap().value;
        assert!(posts.is_empty(), "{posts:?}");
        assert_eq!(blobs::list(&s, 1).await.unwrap().value.total, before_total);
    });
}

#[test]
fn outage_mid_list_serves_stale_then_recovers() {
    let f = Fleet::start(&["feed"]);
    let s = f.admin();
    let dir = tempfile::tempdir().unwrap();
    let file = png(dir.path());
    let first = block(async {
        for i in 0..6 {
            feed::create(&s, &format!("p{i}"), &[]).await.unwrap();
        }
        blobs::upload(&s, &file, &Progress::new(0)).await.unwrap();
        assert_eq!(blobs::list(&s, 1).await.unwrap().freshness, Freshness::Live);
        let p1 = feed::posts(&s, None, 3).await.unwrap();
        assert_eq!(p1.freshness, Freshness::Live);
        p1.value.0
    });
    // feed goes down between page 1 and page 2
    f.stop("feed");
    block(async {
        let stale = feed::posts(&s, None, 3).await.unwrap();
        assert!(
            matches!(stale.freshness, Freshness::Stale { error: ApiError::Offline(_), .. }),
            "{:?}",
            stale.freshness
        );
        assert_eq!(stale.value.0, first, "the cached page, unchanged");
        // the next page was never loaded: nothing to show, offline
        let e = feed::posts(&s, Some(&first.last().unwrap().cursor()), 3).await.unwrap_err();
        assert!(e.is_offline(), "{e:?}");
        // a write during the outage fails as offline, never as "saved"
        assert!(feed::create(&s, "during", &[]).await.unwrap_err().is_offline());
        // blobs is unaffected by feed's outage
        assert_eq!(blobs::list(&s, 1).await.unwrap().freshness, Freshness::Live);
    });
    // and a blobs outage is stale in the same way
    f.stop("blobs");
    block(async {
        let stale = blobs::list(&s, 1).await.unwrap();
        assert!(matches!(stale.freshness, Freshness::Stale { .. }));
        assert_eq!(stale.value.total, 1);
    });
    f.restart("feed");
    f.restart("blobs");
    block(async {
        let back = feed::posts(&s, None, 3).await.unwrap();
        assert_eq!(back.freshness, Freshness::Live);
        let (p2, _) = feed::posts(&s, Some(&first.last().unwrap().cursor()), 3).await.unwrap().value;
        assert_eq!(p2.len(), 3);
        assert_eq!(blobs::list(&s, 1).await.unwrap().freshness, Freshness::Live);
    });
}
