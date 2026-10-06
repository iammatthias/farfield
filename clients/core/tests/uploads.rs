//! Integration tests for the uploads workspaces: library (resumable tus,
//! collections, delete, key scopes) and sideload (streamed IPA upload,
//! share links, deletes), against the real Go services.

mod fleet;

use farfield_core::api::ext_uploads::library_uploads::{self as lu};
use farfield_core::api::ext_uploads::sideload_links::{self as sl, Expiry};
use farfield_core::api::{library, sideload};
use farfield_core::upload::{Progress, TusOutcome, TusState};
use farfield_core::ApiError;
use farfield_core::Freshness;
use fleet::{block, key, read_key, Fleet};
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

// ── fixtures: a stored-method zip writer (no zip crate needed) ───────────

fn crc32(data: &[u8]) -> u32 {
    let mut c = !0u32;
    for &b in data {
        c ^= b as u32;
        for _ in 0..8 {
            c = if c & 1 != 0 { (c >> 1) ^ 0xEDB8_8320 } else { c >> 1 };
        }
    }
    !c
}

fn zip(entries: &[(&str, &[u8])]) -> Vec<u8> {
    let mut out = Vec::new();
    let mut central = Vec::new();
    for (name, data) in entries {
        let off = out.len() as u32;
        let crc = crc32(data);
        let n = data.len() as u32;
        let mut h = Vec::new();
        h.extend(0x0403_4b50u32.to_le_bytes());
        h.extend(20u16.to_le_bytes()); // version needed
        h.extend(0u16.to_le_bytes()); // flags
        h.extend(0u16.to_le_bytes()); // method: stored
        h.extend(0u16.to_le_bytes()); // time
        h.extend(0x21u16.to_le_bytes()); // date (1980-01-01)
        h.extend(crc.to_le_bytes());
        h.extend(n.to_le_bytes());
        h.extend(n.to_le_bytes());
        h.extend((name.len() as u16).to_le_bytes());
        h.extend(0u16.to_le_bytes());
        out.extend(&h);
        out.extend(name.as_bytes());
        out.extend(*data);

        central.extend(0x0201_4b50u32.to_le_bytes());
        central.extend(20u16.to_le_bytes()); // made by
        central.extend(20u16.to_le_bytes());
        central.extend(0u16.to_le_bytes());
        central.extend(0u16.to_le_bytes());
        central.extend(0u16.to_le_bytes());
        central.extend(0x21u16.to_le_bytes());
        central.extend(crc.to_le_bytes());
        central.extend(n.to_le_bytes());
        central.extend(n.to_le_bytes());
        central.extend((name.len() as u16).to_le_bytes());
        central.extend(0u16.to_le_bytes()); // extra
        central.extend(0u16.to_le_bytes()); // comment
        central.extend(0u16.to_le_bytes()); // disk
        central.extend(0u16.to_le_bytes()); // internal attrs
        central.extend(0u32.to_le_bytes()); // external attrs
        central.extend(off.to_le_bytes());
        central.extend(name.as_bytes());
    }
    let cd_off = out.len() as u32;
    let cd_len = central.len() as u32;
    out.extend(&central);
    out.extend(0x0605_4b50u32.to_le_bytes());
    out.extend(0u16.to_le_bytes());
    out.extend(0u16.to_le_bytes());
    out.extend((entries.len() as u16).to_le_bytes());
    out.extend((entries.len() as u16).to_le_bytes());
    out.extend(cd_len.to_le_bytes());
    out.extend(cd_off.to_le_bytes());
    out.extend(0u16.to_le_bytes());
    out
}

/// A 1×1 PNG, for the cover.
const PNG: &[u8] = &[
    0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00,
    0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00, 0x0d, 0x49,
    0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0, 0x1f, 0x00, 0x05, 0x00, 0x01, 0xff, 0x89, 0x99, 0x3d,
    0x1d, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
];

/// A minimal valid EPUB (what apps/library's parser needs: container.xml →
/// OPF with Dublin Core metadata, a cover-image item), padded with `filler`
/// incompressible bytes so an upload spans many small chunks.
fn epub(title: &str, filler: usize) -> Vec<u8> {
    let container = r#"<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>"#;
    let opf = format!(
        r#"<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>{title}</dc:title>
    <dc:creator>Test Author</dc:creator>
    <dc:language>en</dc:language>
    <dc:identifier id="bookid">urn:uuid:{title}</dc:identifier>
    <dc:description>A test book.</dc:description>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="cover-img" href="cover.png" media-type="image/png" properties="cover-image"/>
  </manifest>
  <spine/>
</package>"#
    );
    // xorshift noise: incompressible, deterministic
    let mut x = 0x2545_f491_4f6c_dd1du64 ^ title.len() as u64;
    let noise: Vec<u8> = (0..filler)
        .map(|_| {
            x ^= x << 13;
            x ^= x >> 7;
            x ^= x << 17;
            x as u8
        })
        .collect();
    zip(&[
        ("mimetype", b"application/epub+zip"),
        ("META-INF/container.xml", container.as_bytes()),
        ("OEBPS/content.opf", opf.as_bytes()),
        ("OEBPS/cover.png", PNG),
        ("OEBPS/filler.bin", &noise),
    ])
}

/// A synthetic .ipa the sideload parser accepts: Payload/<Name>.app/Info.plist
/// (XML plist) plus an embedded.mobileprovision whose plist is wrapped in
/// filler, as a CMS envelope would.
fn ipa(bundle: &str, name: &str, version: &str, build: &str, expiry: &str) -> Vec<u8> {
    let info = format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>{bundle}</string>
<key>CFBundleName</key><string>{name}</string>
<key>CFBundleDisplayName</key><string>{name}</string>
<key>CFBundleShortVersionString</key><string>{version}</string>
<key>CFBundleVersion</key><string>{build}</string>
<key>CFBundlePackageType</key><string>APPL</string>
</dict></plist>"#
    );
    let prov = format!(
        "\x00\x01CMS-PREAMBLE\x02<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\"><dict>\
<key>Name</key><string>{name}</string><key>TeamName</key><string>Test Team</string>\
<key>ExpirationDate</key><date>{expiry}</date>\
<key>ProvisionedDevices</key><array><string>00008110-AAAA</string><string>00008110-BBBB</string></array>\
</dict></plist>CMS-TRAILER\x00"
    );
    zip(&[
        (&format!("Payload/{name}.app/Info.plist"), info.as_bytes()),
        (&format!("Payload/{name}.app/embedded.mobileprovision"), prov.as_bytes()),
    ])
}

fn write_file(f: &Fleet, name: &str, bytes: &[u8]) -> PathBuf {
    let p = f.data.path().join("files").join(name);
    std::fs::create_dir_all(p.parent().unwrap()).unwrap();
    std::fs::write(&p, bytes).unwrap();
    p
}

fn http() -> reqwest::blocking::Client {
    reqwest::blocking::Client::builder().redirect(reqwest::redirect::Policy::none()).build().unwrap()
}

/// Start an upload on the runtime and interrupt it with `stop` once at
/// least `after` bytes have gone out. Returns the upload's result.
fn interrupt(
    s: &std::sync::Arc<farfield_core::Session>,
    file: &Path,
    chunk: u64,
    after: u64,
    stop: impl FnOnce(&Progress),
) -> Result<TusOutcome, ApiError> {
    let progress = Progress::new(0);
    let (s2, pr) = (s.clone(), progress.clone());
    let st = TusState::new(file.to_path_buf(), "").unwrap();
    let h = farfield_core::spawn(async move {
        let mut st = st;
        lu::run_chunked(&s2, &mut st, &pr, chunk).await
    });
    let deadline = Instant::now() + Duration::from_secs(20);
    while progress.sent() < after && !h.is_finished() {
        assert!(Instant::now() < deadline, "upload never got going");
        std::thread::yield_now();
    }
    assert!(!h.is_finished(), "the upload finished before it could be interrupted");
    stop(&progress);
    block(h).unwrap()
}

// ── library ──────────────────────────────────────────────────────────────

#[test]
fn library_tus_upload_end_to_end() {
    let f = Fleet::start(&["library"]);
    let s = f.admin();
    let file = write_file(&f, "first-light.epub", &epub("First Light", 2048));
    block(async {
        let progress = Progress::new(0);
        let st = TusState::new(file.clone(), "Field Guides").unwrap();
        let out = lu::run(&s, st, &progress).await.unwrap();
        let TusOutcome::Done { cid } = out else { panic!("{out:?}") };
        assert_eq!(progress.sent(), progress.total());
        assert_eq!(progress.total(), std::fs::metadata(&file).unwrap().len());
        // settled uploads are forgotten
        assert!(lu::interrupted(&s).is_empty());

        let cat = library::catalog(&s).await.unwrap().value;
        let b = cat.books.iter().find(|b| b.cid == cid).expect("book in /api/admin/books");
        assert_eq!(b.title, "First Light");
        assert_eq!(b.author, "Test Author");
        assert_eq!(b.collection, "Field Guides");
        assert_eq!(b.filename, "first-light.epub");
        assert_eq!(b.size as u64, progress.total());
        assert_eq!(cat.collections.iter().find(|c| c.name == "Field Guides").map(|c| c.count), Some(1));
        assert_eq!(cat.uncategorized, 0);

        // the cover comes over OPDS Basic auth
        let cover_cid = if b.thumb_cid.is_empty() { b.cover_cid.clone() } else { b.thumb_cid.clone() };
        assert!(!cover_cid.is_empty(), "the EPUB's cover was extracted");
        let png = library::cover(&s, &cover_cid).await.unwrap();
        assert!(png.starts_with(&[0x89, b'P', b'N', b'G']) || png.starts_with(&[0xff, 0xd8]), "an image");

        // move to a new collection, then to uncategorized
        let moved = library::set_collection(&s, &cid, "Shelf").await.unwrap();
        assert_eq!(moved.value.collection, "Shelf");
        let cat = library::catalog(&s).await.unwrap().value;
        assert_eq!(cat.collections.iter().find(|c| c.name == "Shelf").map(|c| c.count), Some(1));
        assert!(!cat.collections.iter().any(|c| c.name == "Field Guides"));
        library::set_collection(&s, &cid, "").await.unwrap();
        let cat = library::catalog(&s).await.unwrap().value;
        assert_eq!(cat.uncategorized, 1);
        assert!(cat.collections.is_empty());

        // the same bytes again: the same book, not a second one
        let again = lu::run(&s, TusState::new(file.clone(), "").unwrap(), &Progress::new(0)).await.unwrap();
        assert_eq!(again, TusOutcome::Done { cid: cid.clone() });
        assert_eq!(library::catalog(&s).await.unwrap().value.books.len(), 1);

        // delete
        library::delete(&s, &cid).await.unwrap();
        let cat = library::catalog(&s).await.unwrap().value;
        assert!(cat.books.is_empty());
        assert_eq!(library::delete(&s, &cid).await.unwrap_err(), ApiError::NotFound);
    });
}

#[test]
fn library_tus_resumes_after_pause_and_relaunch() {
    let f = Fleet::start(&["library"]);
    let file = write_file(&f, "long-book.epub", &epub("Long Book", 400 << 10));
    let size = std::fs::metadata(&file).unwrap().len();
    let s = std::sync::Arc::new(f.admin());

    // pause (cancel) part-way through
    let r = interrupt(&s, &file, 4096, 4 * 4096, |p| p.cancel());
    assert_eq!(r, Err(ApiError::Cancelled));

    // "relaunch": a new session over the same data directory finds it
    let s2 = f.admin();
    block(async {
        let pending = lu::interrupted(&s2);
        assert_eq!(pending.len(), 1, "{pending:?}");
        let mut st = pending[0].state.clone();
        assert_eq!(st.file, file);
        let loc = st.location.clone().expect("the server location was persisted");
        let server = lu::server_state(&s2, &loc).await.unwrap().expect("server still has it");
        assert!(server.offset >= 4096 && server.offset < size, "partial on the server: {server:?}");
        assert_eq!(server.length, size);
        assert_eq!(server.status, "open");

        // resume: picks up at the server's offset, same upload
        let progress = Progress::new(0);
        let resumed_from = server.offset;
        let out = lu::run_chunked(&s2, &mut st, &progress, 4096).await.unwrap();
        let TusOutcome::Done { cid } = out else { panic!("{out:?}") };
        assert_eq!(st.location.as_deref(), Some(loc.as_str()), "resumed the same upload, did not start over");
        assert!(resumed_from > 0);
        assert!(lu::interrupted(&s2).is_empty());
        let done = lu::server_state(&s2, &loc).await.unwrap().unwrap();
        assert_eq!((done.status.as_str(), done.cid.as_deref(), done.offset), ("done", Some(cid.as_str()), size));

        // the whole file arrived intact: a fresh upload of the same bytes
        // lands on the same content address
        let fresh = lu::run(&s2, TusState::new(file.clone(), "").unwrap(), &Progress::new(0)).await.unwrap();
        assert_eq!(fresh, TusOutcome::Done { cid: cid.clone() });
        let cat = library::catalog(&s2).await.unwrap().value;
        assert_eq!(cat.books.len(), 1);
        assert_eq!(cat.books[0].size as u64, size);
    });
}

#[test]
fn library_tus_resumes_after_outage() {
    let f = Fleet::start(&["library"]);
    let file = write_file(&f, "outage.epub", &epub("Outage", 400 << 10));
    let size = std::fs::metadata(&file).unwrap().len();
    let s = std::sync::Arc::new(f.admin());

    // the service dies mid-upload
    let r = interrupt(&s, &file, 4096, 4 * 4096, |_| f.stop("library"));
    let e = r.expect_err("an outage mid-upload is an error");
    assert!(matches!(e, ApiError::Offline(_) | ApiError::Uncertain(_)), "{e:?}");
    let pending = lu::interrupted(&s);
    assert_eq!(pending.len(), 1);

    f.restart("library");
    block(async {
        let mut st = pending[0].state.clone();
        let loc = st.location.clone().unwrap();
        let server = lu::server_state(&s, &loc).await.unwrap().expect("staging survived the restart");
        assert!(server.offset > 0 && server.offset < size, "{server:?}");
        let out = lu::run_chunked(&s, &mut st, &Progress::new(0), 4096).await.unwrap();
        let TusOutcome::Done { cid } = out else { panic!("{out:?}") };
        assert_eq!(st.location.as_deref(), Some(loc.as_str()));
        let cat = library::catalog(&s).await.unwrap().value;
        let b = cat.books.iter().find(|b| b.cid == cid).expect("book");
        assert_eq!(b.size as u64, size);
        assert_eq!(b.title, "Outage");
        assert!(lu::interrupted(&s).is_empty());
    });
}

#[test]
fn library_discard_drops_server_partial() {
    let f = Fleet::start(&["library"]);
    let file = write_file(&f, "discard.epub", &epub("Discard", 200 << 10));
    let s = std::sync::Arc::new(f.admin());
    let r = interrupt(&s, &file, 4096, 2 * 4096, |p| p.cancel());
    assert_eq!(r, Err(ApiError::Cancelled));
    block(async {
        let st = lu::interrupted(&s).remove(0).state;
        let loc = st.location.clone().unwrap();
        lu::discard(&s, &st).await.unwrap();
        assert!(lu::interrupted(&s).is_empty());
        assert_eq!(lu::server_state(&s, &loc).await.unwrap(), None);
    });
}

#[test]
fn library_upload_scope_cannot_read() {
    let f = Fleet::start(&["library", "keys"]);
    // an upload-scoped key (what LIBRARY_UPLOAD_KEY grants; the harness
    // mints the revocable ffk_ equivalent through the keys console)
    let (token, key_id) = f.mint("intern", "library", "upload");
    let (up, _) = f.session_with(&[("library", &token)]);
    let admin = f.admin();
    let file = write_file(&f, "scoped.epub", &epub("Scoped", 1024));
    block(async {
        let out = lu::run(&up, TusState::new(file.clone(), "Inbox").unwrap(), &Progress::new(0)).await.unwrap();
        let TusOutcome::Done { cid } = out else { panic!("{out:?}") };
        // it can regroup...
        library::set_collection(&up, &cid, "Read Later").await.unwrap();
        // ...but never read the catalog, a cover, or delete
        assert!(library::catalog(&up).await.unwrap_err().is_auth());
        let b = library::catalog(&admin).await.unwrap().value.books.into_iter().find(|b| b.cid == cid).unwrap();
        assert_eq!(b.collection, "Read Later");
        assert!(library::cover(&up, &b.cover_cid).await.unwrap_err().is_auth());
        assert!(library::delete(&up, &cid).await.unwrap_err().is_auth());
        // no key at all
        let (anon, _) = f.session_with(&[]);
        assert!(library::catalog(&anon).await.unwrap_err().is_auth());
        let (wrong, _) = f.session_with(&[("library", "not-a-key")]);
        assert!(library::catalog(&wrong).await.unwrap_err().is_auth());

        // a revoked upload key uploads nothing more
        f.revoke(&key_id);
        let file2 = write_file(&f, "after-revoke.epub", &epub("After Revoke", 512));
        let e = lu::run(&up, TusState::new(file2, "").unwrap(), &Progress::new(0)).await.unwrap_err();
        assert!(e.is_auth(), "{e:?}");
    });
}

// ── sideload ─────────────────────────────────────────────────────────────

fn rfc3339_in(days: i64) -> String {
    let t = time::OffsetDateTime::now_utc() + time::Duration::days(days);
    t.replace_nanosecond(0).unwrap().format(&time::format_description::well_known::Rfc3339).unwrap()
}

#[test]
fn sideload_upload_share_revoke_delete() {
    let f = Fleet::start(&["sideload"]);
    let s = f.admin();
    let a1 =
        write_file(&f, "fieldnotes-31.ipa", &ipa("com.test.fieldnotes", "Fieldnotes", "0.9.1", "31", &rfc3339_in(3)));
    let a2 =
        write_file(&f, "fieldnotes-32.ipa", &ipa("com.test.fieldnotes", "Fieldnotes", "0.9.2", "32", &rfc3339_in(90)));
    let b1 = write_file(&f, "aperture.ipa", &ipa("com.test.aperture", "Aperture", "1.5.0", "150", &rfc3339_in(-1)));
    block(async {
        // a cancelled upload sends nothing
        let p = Progress::new(0);
        p.cancel();
        assert_eq!(sideload::upload(&s, &a1, "", &p).await.unwrap_err(), ApiError::Cancelled);
        assert!(sideload::builds(&s).await.unwrap().value.is_empty());

        let p = Progress::new(0);
        let up = sideload::upload(&s, &a1, "first build", &p).await.unwrap().value;
        assert_eq!(p.sent(), std::fs::metadata(&a1).unwrap().len());
        assert_eq!(
            (up.bundle_id.as_str(), up.version.as_str(), up.build_number.as_str()),
            ("com.test.fieldnotes", "0.9.1", "31")
        );
        assert_eq!(up.device_count, 2);
        // idempotent by content
        let again = sideload::upload(&s, &a1, "first build", &Progress::new(0)).await.unwrap().value;
        assert_eq!(again.id, up.id);
        tokio::time::sleep(Duration::from_millis(1100)).await; // distinct createdAt seconds
        let up2 = sideload::upload(&s, &a2, "", &Progress::new(0)).await.unwrap().value;
        let ap = sideload::upload(&s, &b1, "", &Progress::new(0)).await.unwrap().value;

        let builds = sideload::builds(&s).await.unwrap().value;
        assert_eq!(builds.len(), 3);
        let apps = sideload::apps(&builds);
        let fieldnotes = apps.iter().find(|(b, _)| b == "com.test.fieldnotes").unwrap();
        assert_eq!(fieldnotes.1.len(), 2);
        assert_eq!(fieldnotes.1[0].id, up2.id, "newest build first");
        let b31 = builds.iter().find(|b| b.id == up.id).unwrap();
        assert_eq!(b31.notes, "first build");
        assert!(matches!(sl::expiry(&b31.profile_expiry), Expiry::Soon(2 | 3)), "{}", b31.profile_expiry);
        assert_eq!(sl::expiry(&builds.iter().find(|b| b.id == ap.id).unwrap().profile_expiry), Expiry::Expired);
        assert_eq!(sideload::install_url(&s, b31).unwrap(), format!("{}/b/{}", f.url("sideload"), up.id));

        // mint → list → revoke
        let sh = sideload::share(&s, &up.id, "2h", "3", "for QA").await.unwrap().value;
        assert!(!sh.token.is_empty());
        assert_eq!(sh.max_installs, 3);
        assert!(sl::seconds_until(&sh.expires_at).is_some_and(|n| (7000..=7200).contains(&n)), "{}", sh.expires_at);
        assert_eq!(sl::share_url(&s, &sh), format!("{}/s/{}", f.url("sideload"), sh.token));
        let unl = sideload::share(&s, &up2.id, "30m", "unlimited", "").await.unwrap().value;
        assert_eq!(unl.max_installs, 0);

        let shares = sideload::shares(&s).await.unwrap().value;
        let got = shares.iter().find(|x| x.token == sh.token).expect("listed");
        assert_eq!((got.state.as_str(), got.installs, got.max_installs, got.live), ("active", 0, 3, true));
        assert_eq!(
            (got.label.as_str(), got.build_id.as_str(), got.app_name.as_str(), got.version.as_str()),
            ("for QA", up.id.as_str(), "Fieldnotes", "0.9.1")
        );

        sideload::revoke_share(&s, &sh.token).await.unwrap();
        let shares = sideload::shares(&s).await.unwrap().value;
        let got = shares.iter().find(|x| x.token == sh.token).unwrap();
        assert_eq!((got.state.as_str(), got.revoked, got.live), ("revoked", true, false));
        assert_eq!(shares.iter().find(|x| x.token == unl.token).unwrap().state, "active");
        // revoking again is harmless; an unknown token is not found
        sideload::revoke_share(&s, &sh.token).await.unwrap();
        assert_eq!(sideload::revoke_share(&s, "nope").await.unwrap_err(), ApiError::NotFound);

        // delete a build, then a whole app
        sideload::delete_build(&s, &ap.id).await.unwrap();
        assert_eq!(sideload::builds(&s).await.unwrap().value.len(), 2);
        sideload::delete_app(&s, "com.test.fieldnotes").await.unwrap();
        assert!(sideload::builds(&s).await.unwrap().value.is_empty());
        assert_eq!(sideload::delete_app(&s, "com.test.fieldnotes").await.unwrap_err(), ApiError::NotFound);

        // auth: no key, wrong key
        let (anon, _) = f.session_with(&[]);
        assert!(sideload::builds(&anon).await.unwrap_err().is_auth());
        assert!(sideload::shares(&anon).await.unwrap_err().is_auth());
        let (wrong, _) = f.session_with(&[("sideload", "nope")]);
        assert!(sideload::shares(&wrong).await.unwrap_err().is_auth());
        // the read key reads builds at most, never the admin share list
        let (reader, _) = f.session_with(&[("sideload", &read_key("sideload"))]);
        assert!(sideload::shares(&reader).await.unwrap_err().is_auth());
    });
}

// ── private-route isolation ──────────────────────────────────────────────

#[test]
fn admin_routes_hidden_from_the_edge() {
    let f = Fleet::start(&["library", "sideload"]);
    let http = http();
    for (app, path) in [("library", "/api/admin/books"), ("sideload", "/api/admin/shares")] {
        let url = format!("{}{path}", f.url(app));
        let ok = http.get(&url).header("X-API-Key", key(app)).send().unwrap();
        assert_eq!(ok.status(), 200, "{app}");
        assert_eq!(ok.headers()["cache-control"], "no-store");
        for h in ["Cf-Ray", "Cf-Connecting-IP"] {
            let r = http.get(&url).header("X-API-Key", key(app)).header(h, "x").send().unwrap();
            assert_eq!(r.status(), 404, "{app} with {h}");
        }
        assert_eq!(http.get(&url).send().unwrap().status(), 401, "{app} no key");
    }
    // a revoke through the edge is just as absent
    let r = http
        .post(format!("{}/api/admin/shares/whatever/revoke", f.url("sideload")))
        .header("X-API-Key", key("sideload"))
        .header("Cf-Ray", "x")
        .send()
        .unwrap();
    assert_eq!(r.status(), 404);
}

// ── outages on reads ─────────────────────────────────────────────────────

#[test]
fn catalog_and_builds_fall_back_to_cache_when_down() {
    let f = Fleet::start(&["library", "sideload"]);
    let s = f.admin();
    let file = write_file(&f, "cached.epub", &epub("Cached", 512));
    let ipa_file = write_file(&f, "cached.ipa", &ipa("com.test.cached", "Cached", "1.0", "1", &rfc3339_in(30)));
    block(async {
        lu::run(&s, TusState::new(file, "").unwrap(), &Progress::new(0)).await.unwrap();
        sideload::upload(&s, &ipa_file, "", &Progress::new(0)).await.unwrap();
        assert_eq!(library::catalog(&s).await.unwrap().freshness, Freshness::Live);
        assert_eq!(sideload::builds(&s).await.unwrap().freshness, Freshness::Live);
    });
    f.stop("library");
    f.stop("sideload");
    block(async {
        let l = library::catalog(&s).await.unwrap();
        assert!(matches!(l.freshness, Freshness::Stale { .. }), "{:?}", l.freshness);
        assert_eq!(l.value.books.len(), 1);
        let b = sideload::builds(&s).await.unwrap();
        assert!(matches!(b.freshness, Freshness::Stale { .. }));
        assert_eq!(b.value.len(), 1);
        // a mutation while down is an error, never a silent success
        assert!(sideload::share(&s, &b.value[0].id, "30m", "1", "").await.is_err());
    });
    f.restart("library");
    f.restart("sideload");
    block(async {
        assert_eq!(library::catalog(&s).await.unwrap().freshness, Freshness::Live);
        assert_eq!(sideload::builds(&s).await.unwrap().freshness, Freshness::Live);
    });
}
