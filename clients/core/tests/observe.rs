//! The observe group against the real Go services: daily (public, paged,
//! ETag-revalidated), pulse (read-key only), switchboard and backup (private
//! admin APIs), apex (the public profile document).

mod fleet;

use farfield_core::api::ext_observe::{apex, backup, daily as daily_x, pulse, switchboard};
use farfield_core::api::{daily, status};
use farfield_core::{ApiError, Freshness};
use fleet::{block, key, read_key, repo, Fleet};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

fn raw() -> reqwest::blocking::Client {
    reqwest::blocking::Client::builder().redirect(reqwest::redirect::Policy::none()).build().unwrap()
}

/// Fill daily's index with one photo per day from its start (2026-01-01)
/// through today (UTC, as SQLite and the server both compute it), so no
/// request ever needs NASA: every archive page is warm and today exists.
fn seed_daily(f: &Fleet) -> i64 {
    f.stop("daily");
    let db = f.data.path().join("daily.sqlite");
    let sql = "WITH RECURSIVE d(day) AS (SELECT '2026-01-01' UNION ALL SELECT date(day, '+1 day') FROM d WHERE day < date('now')) \
         INSERT OR REPLACE INTO photos (source, date, cid, title, explanation, image_url, thumb_url, media_type, credit, source_url, placeholder, fetched_at) \
         SELECT 'nasa', day, 'cid-' || day, 'Photo of ' || day, 'An explanation for ' || day || '.', \
         'https://example.invalid/' || day || '.jpg', '', 'image', 'Test credit', 'https://apod.nasa.gov/apod/', 0, '2026-01-01T00:00:00Z' FROM d; \
         SELECT count(*) FROM photos WHERE source = 'nasa';";
    let out = Command::new("sqlite3").arg(&db).arg(sql).output().expect("sqlite3");
    assert!(out.status.success(), "seed: {}", String::from_utf8_lossy(&out.stderr));
    let n: i64 = String::from_utf8_lossy(&out.stdout).trim().parse().unwrap();
    f.restart("daily");
    n
}

#[test]
fn daily_photo_navigation_archive_paging_and_revalidation() {
    let f = Fleet::start(&["daily"]);
    let total = seed_daily(&f);
    // public: no key at all
    let (s, _) = f.session_with(&[]);
    block(async {
        let today = daily::today(&s).await.unwrap();
        assert_eq!(today.freshness, Freshness::Live);
        let p = &today.value.photo;
        assert!(!p.title.is_empty() && !p.image_url.is_empty(), "{p:?}");
        assert_eq!(today.value.next, "", "today has no next day");
        assert!(!today.value.prev.is_empty());

        // prev/next walk: the previous day points forward to today
        let prev = daily::day(&s, &today.value.prev).await.unwrap();
        assert_eq!(prev.value.photo.date, today.value.prev);
        assert_eq!(prev.value.next, p.date);

        // the first day has no prev
        let first = daily::day(&s, "2026-01-01").await.unwrap();
        assert_eq!(first.value.prev, "");
        assert_eq!(first.value.next, "2026-01-02");

        // ETag revalidation: the second read is a 304 served from the cache
        let again = daily::day(&s, "2026-01-01").await.unwrap();
        assert_eq!(again.freshness, Freshness::Live);
        assert_eq!(again.etag, first.etag);
        assert_eq!(again.value.photo.title, first.value.photo.title);

        // malformed date: a 400, not a crash
        assert!(matches!(daily::day(&s, "not-a-date").await.unwrap_err(), ApiError::BadRequest(_)));

        // archive: 14 per page, newest first, contiguous across pages
        let pages = (total + 13) / 14;
        let a1 = daily::archive(&s, 1).await.unwrap().value;
        assert_eq!((a1.total, a1.pages, a1.page), (total, pages, 1));
        assert_eq!(a1.photos.len(), 14);
        assert_eq!(a1.photos[0].date, p.date);
        assert!(a1.photos.windows(2).all(|w| w[0].date > w[1].date));
        let a2 = daily::archive(&s, 2).await.unwrap().value;
        assert_eq!(a2.page, 2);
        assert!(a2.photos[0].date < a1.photos[13].date);
        let last = daily::archive(&s, pages as u32).await.unwrap().value;
        assert_eq!(last.photos.len() as i64, total - (pages - 1) * 14);
        assert_eq!(last.photos.last().unwrap().date, "2026-01-01");

        // the plate: descriptor + SVG agree on the date, and the SVG is SVG
        let art = daily_x::art(&s, Some("2026-01-01")).await.unwrap().value;
        assert_eq!(art.date, "2026-01-01");
        assert!(!art.biome.is_empty() && art.coord.len() == 4 && !art.zone.colors.is_empty(), "{art:?}");
        let svg = daily_x::art_svg(&s, Some("2026-01-01")).await.unwrap();
        assert!(svg.starts_with(b"<svg"), "{:?}", String::from_utf8_lossy(&svg[..40.min(svg.len())]));
        let today_svg = daily_x::art_svg(&s, None).await.unwrap();
        assert!(today_svg.starts_with(b"<svg"));
        // the misc.rs path agrees (it once requested the HTML page)
        let svg2 = daily::art_svg(&s, Some("2026-01-01")).await.unwrap();
        assert_eq!(svg, svg2);

        // remote images refuse anything but http(s)
        assert!(matches!(daily_x::remote_image("file:///etc/passwd").await.unwrap_err(), ApiError::BadRequest(_)));
    });

    // outage: the cached day is served, marked stale; then recovery
    f.stop("daily");
    block(async {
        let stale = daily::day(&s, "2026-01-01").await.unwrap();
        assert!(
            matches!(stale.freshness, Freshness::Stale { error: ApiError::Offline(_), .. }),
            "{:?}",
            stale.freshness
        );
        // never fetched before: nothing to fall back on
        assert!(daily::day(&s, "2026-01-05").await.unwrap_err().is_offline());
    });
    f.restart("daily");
    block(async {
        assert_eq!(daily::day(&s, "2026-01-01").await.unwrap().freshness, Freshness::Live);
    });
}

#[test]
fn pulse_overview_and_traffic_need_the_read_key() {
    let f = Fleet::start(&["pulse"]);
    let (ok, _) = f.session_with(&[("pulse", &read_key("pulse"))]);
    let (wrong, _) = f.session_with(&[("pulse", "not-the-key")]);
    let (none, _) = f.session_with(&[]);
    // the write key is not a pulse credential: pulse configures a read key only
    let (write, _) = f.session_with(&[("pulse", &key("pulse"))]);
    block(async {
        let o = pulse::overview(&ok).await.unwrap();
        assert_eq!(o.freshness, Freshness::Live);
        for t in &o.value.targets {
            assert!(!t.name.is_empty() && !t.url.is_empty(), "{t:?}");
            assert!(!t.up_24h.is_empty());
        }
        let t = pulse::traffic(&ok, "", "", "").await.unwrap().value;
        // the server's default window: 14 contiguous days
        assert_eq!(t.hits_per_day.len(), 14, "{t:?}");
        assert_eq!(t.uniques_per_day.len(), 14);
        let t2 = pulse::traffic(&ok, "content", "2026-01-01", "2026-01-07").await.unwrap().value;
        assert_eq!((t2.app.as_str(), t2.from.as_str(), t2.to.as_str()), ("content", "2026-01-01", "2026-01-07"));
        assert_eq!(t2.hits_per_day.len(), 7);
        assert!(t2.hits_per_day.iter().all(|d| d.n == 0));
        // reversed range is normalised by the server
        let t3 = pulse::traffic(&ok, "", "2026-01-07", "2026-01-01").await.unwrap().value;
        assert_eq!((t3.from.as_str(), t3.to.as_str()), ("2026-01-01", "2026-01-07"));

        // a wrong key is a 303 to the console login → Unauthorized
        for (who, s) in [("wrong", &wrong), ("none", &none), ("write", &write)] {
            match pulse::overview(s).await.unwrap_err() {
                ApiError::Unauthorized(svc) => assert_eq!(svc, "pulse", "{who}"),
                e => panic!("{who}: {e:?}"),
            }
            assert!(pulse::traffic(s, "", "", "").await.unwrap_err().is_auth(), "{who}");
        }
    });
    let r = raw().get(format!("{}/api/overview", f.url("pulse"))).header("X-API-Key", "nope").send().unwrap();
    assert_eq!(r.status(), 303);
}

#[test]
fn switchboard_admin_reads_are_write_key_and_private_only() {
    let f = Fleet::start(&["switchboard"]);
    let admin = f.admin();
    let (reader, _) = f.session_with(&[("switchboard", &read_key("switchboard"))]);
    let (none, _) = f.session_with(&[]);
    block(async {
        let m = switchboard::messages(&admin, 50).await.unwrap();
        assert_eq!(m.freshness, Freshness::Live);
        assert!(m.value.is_empty(), "a fresh line has no messages");
        let j = switchboard::jobs(&admin, 50).await.unwrap();
        assert!(j.value.is_empty());
        // limits outside 1..=500 are clamped client-side, not refused
        switchboard::messages(&admin, 10_000).await.unwrap();
        switchboard::jobs(&admin, 0).await.unwrap();

        // health: public /status, no key needed
        let st = switchboard::status(&none).await.unwrap();
        assert_eq!(st.messages, 0);
        assert!(!st.hook, "the test fleet sets no webhook secret");
        assert!(status(&none, "switchboard").await.unwrap()["ok"].as_bool().unwrap());

        for (who, s) in [("read", &reader), ("none", &none)] {
            assert!(switchboard::messages(s, 10).await.unwrap_err().is_auth(), "{who}");
            assert!(switchboard::jobs(s, 10).await.unwrap_err().is_auth(), "{who}");
        }
    });
    let http = raw();
    for path in ["/api/admin/messages", "/api/admin/jobs"] {
        let url = format!("{}{path}", f.url("switchboard"));
        let ok = http.get(&url).header("X-API-Key", key("switchboard")).send().unwrap();
        assert_eq!(ok.status(), 200);
        assert_eq!(ok.headers()["cache-control"], "no-store");
        let tunnel = http.get(&url).header("X-API-Key", key("switchboard")).header("Cf-Ray", "x").send().unwrap();
        assert_eq!(tunnel.status(), 404, "{path} via the tunnel");
        assert_eq!(http.get(&url).header("X-API-Key", read_key("switchboard")).send().unwrap().status(), 401);
        assert_eq!(http.get(&url).send().unwrap().status(), 401);
    }
}

/// A backup process started by hand on the fleet's port, without
/// BACKUP_API_KEY — the harness always sets one, so this is the only way to
/// see the unconfigured admin API.
struct Bare(Child);

impl Drop for Bare {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn backup_without_key(f: &Fleet) -> Bare {
    f.stop("backup");
    let d = f.data.path();
    let port = f.ports["backup"];
    let log = std::fs::File::create(d.join("backup-bare.log")).unwrap();
    let child = Command::new(repo().join("clients/target/fleet-bin/backup"))
        .arg("serve")
        .current_dir(d) // away from the repo's .env
        .env_clear()
        .env("PATH", std::env::var("PATH").unwrap_or_default())
        .env("HOME", d)
        .envs([
            ("HOST", "127.0.0.1".to_string()),
            ("PASSWORD", fleet::PASSWORD.to_string()),
            ("COOKIE_SECURE", "false".into()),
            ("SESSION_SECRET", "test-fleet-secret".into()),
            ("BACKUP_PORT", port.to_string()),
            ("BACKUP_DB_PATH", d.join("backup.sqlite").display().to_string()),
            ("BACKUP_INTERVAL", "0".into()),
        ])
        .stdout(Stdio::from(log.try_clone().unwrap()))
        .stderr(Stdio::from(log))
        .spawn()
        .expect("start bare backup");
    let bare = Bare(child);
    let deadline = Instant::now() + Duration::from_secs(20);
    let url = format!("{}/status", f.url("backup"));
    while Instant::now() < deadline {
        if raw().get(&url).send().map(|r| r.status() == 200).unwrap_or(false) {
            return bare;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    panic!("bare backup did not come up: {}", std::fs::read_to_string(d.join("backup-bare.log")).unwrap_or_default());
}

#[test]
fn backup_snapshots_key_gate_and_unconfigured_503() {
    let f = Fleet::start(&["backup"]);
    let admin = f.admin();
    let (reader, _) = f.session_with(&[("backup", &read_key("backup"))]);
    block(async {
        let l = backup::snapshots(&admin).await.unwrap();
        assert_eq!(l.freshness, Freshness::Live);
        let groups = backup::group(&l.value);
        assert_eq!(
            groups.len(),
            groups.iter().map(|g| g.app.as_str()).collect::<std::collections::BTreeSet<_>>().len()
        );
        assert!(backup::snapshots(&reader).await.unwrap_err().is_auth(), "backup has no read scope");
        assert!(status(&admin, "backup").await.unwrap()["ok"].as_bool().unwrap());
    });
    let http = raw();
    let url = format!("{}/api/admin/snapshots", f.url("backup"));
    assert_eq!(http.get(&url).header("X-API-Key", key("backup")).header("Cf-Ray", "x").send().unwrap().status(), 404);

    // no BACKUP_API_KEY on the server: the admin API is off (503), whatever
    // key the client holds — and it says so instead of looking like a bad key
    let _bare = backup_without_key(&f);
    block(async {
        match backup::snapshots(&admin).await.unwrap_err() {
            ApiError::Unavailable(m) => assert!(m.contains("no key configured"), "{m}"),
            e => panic!("{e:?}"),
        }
    });
}

#[test]
fn apex_profile_document() {
    let f = Fleet::start(&["apex"]);
    let (s, _) = f.session_with(&[]);
    block(async {
        let p = apex::profile(&s).await.unwrap();
        assert_eq!(p.freshness, Freshness::Live);
        // sections are GFM strings keyed by name; an upstream that failed is
        // absent rather than empty
        for (k, v) in &p.value.sections {
            assert!(matches!(k.as_str(), "feed" | "writing" | "daily"), "{k}");
            assert!(!v.is_empty(), "{k}");
        }
        // the profile carries an ETag: revalidation is a 304 served from cache
        let again = apex::profile(&s).await.unwrap();
        assert_eq!(again.freshness, Freshness::Live);
        assert_eq!(again.value, p.value);
        assert!(status(&s, "apex").await.unwrap()["ok"].as_bool().unwrap());
    });
}
