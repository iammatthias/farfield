//! Extra client functions for the observe workspaces (see misc.rs for the base):
//! typed views of pulse, switchboard, backup, apex and daily, plus the
//! credential-less fetch daily's remote (NASA) images need.
//!
//! Go encodes a nil slice as `null`, and `#[serde(default)]` only covers a
//! missing key — so every list here goes through [`null_vec`].
use crate::session::{Loaded, Session};
use crate::transport::{ApiError, ServiceClient};
use reqwest::{header, Method};
use serde::{Deserialize, Deserializer, Serialize};
use std::collections::BTreeMap;
use std::sync::OnceLock;
use std::time::Duration;

fn enc(s: &str) -> String {
    url::form_urlencoded::byte_serialize(s.as_bytes()).collect()
}

/// A list that may arrive as `null` (a nil Go slice) or be absent.
pub fn null_vec<'de, D: Deserializer<'de>, T: Deserialize<'de>>(d: D) -> Result<Vec<T>, D::Error> {
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}

// ── pulse ────────────────────────────────────────────────────────────────

pub mod pulse {
    use super::*;
    pub const SERVICE: &str = "pulse";

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase", default)]
    pub struct Check {
        pub ts: String,
        pub status_code: i64,
        pub latency_ms: i64,
        pub ok: bool,
        pub err: String,
    }

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase", default)]
    pub struct Incident {
        pub id: i64,
        pub target_id: i64,
        pub target_name: String,
        pub opened_at: String,
        pub closed_at: String,
        pub last_err: String,
    }

    impl Incident {
        pub fn open(&self) -> bool {
            self.closed_at.is_empty()
        }
    }

    /// One monitored target with its latest check, uptime windows (already
    /// formatted by the server: "99.95%" or "—") and open incident.
    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase", default)]
    pub struct Target {
        pub id: i64,
        pub name: String,
        pub url: String,
        pub method: String,
        pub expected_status: i64,
        pub interval_s: i64,
        pub enabled: bool,
        pub created_at: String,
        pub last: Option<Check>,
        #[serde(rename = "up24h")]
        pub up_24h: String,
        #[serde(rename = "up7d")]
        pub up_7d: String,
        #[serde(rename = "up30d")]
        pub up_30d: String,
        pub incident: Option<Incident>,
    }

    impl Target {
        /// Up when the newest check passed; None before the first check.
        pub fn up(&self) -> Option<bool> {
            self.last.as_ref().map(|c| c.ok)
        }
    }

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    pub struct Overview {
        #[serde(default, deserialize_with = "null_vec")]
        pub targets: Vec<Target>,
        /// The most recent incidents, open and closed.
        #[serde(default, deserialize_with = "null_vec")]
        pub incidents: Vec<Incident>,
    }

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    pub struct DayCount {
        pub day: String,
        pub n: i64,
    }

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(default)]
    pub struct PathStat {
        pub app: String,
        pub path: String,
        pub hits: i64,
        pub uniques: i64,
    }

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(default)]
    pub struct BucketStat {
        pub bucket: String,
        pub hits: i64,
    }

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(default)]
    pub struct RefStat {
        pub host: String,
        pub hits: i64,
    }

    /// Traffic for one app (or all, `app == ""`) over `[from, to]`; the
    /// per-day series are contiguous (gaps filled with zero by the server).
    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase", default)]
    pub struct Traffic {
        pub app: String,
        pub from: String,
        pub to: String,
        #[serde(deserialize_with = "null_vec")]
        pub apps: Vec<String>,
        #[serde(deserialize_with = "null_vec")]
        pub hits_per_day: Vec<DayCount>,
        #[serde(deserialize_with = "null_vec")]
        pub uniques_per_day: Vec<DayCount>,
        #[serde(deserialize_with = "null_vec")]
        pub top_paths: Vec<PathStat>,
        #[serde(deserialize_with = "null_vec")]
        pub status_mix: Vec<BucketStat>,
        #[serde(deserialize_with = "null_vec")]
        pub top_referrers: Vec<RefStat>,
    }

    /// `GET /api/overview` — needs PULSE_READ_KEY (a wrong key is a 303 to
    /// the console login, which surfaces as `Unauthorized`).
    pub async fn overview(s: &Session) -> Result<Loaded<Overview>, ApiError> {
        s.load(SERVICE, "/api/overview").await
    }

    /// `GET /api/traffic`; empty `app` means every app, empty dates the
    /// server's default (the last 14 days).
    pub async fn traffic(s: &Session, app: &str, from: &str, to: &str) -> Result<Loaded<Traffic>, ApiError> {
        let mut q = Vec::new();
        if !app.is_empty() {
            q.push(format!("app={}", enc(app)));
        }
        if !from.is_empty() {
            q.push(format!("from={}", enc(from)));
        }
        if !to.is_empty() {
            q.push(format!("to={}", enc(to)));
        }
        let path = if q.is_empty() { "/api/traffic".to_string() } else { format!("/api/traffic?{}", q.join("&")) };
        s.load(SERVICE, &path).await
    }
}

// ── switchboard ──────────────────────────────────────────────────────────

pub mod switchboard {
    use super::*;
    pub const SERVICE: &str = "switchboard";

    /// One logged exchange: the inbound text and the reply sent on it.
    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase", default)]
    pub struct Message {
        pub id: String,
        pub direction: String,
        pub sender: String,
        pub body: String,
        pub route: String,
        #[serde(rename = "ref")]
        pub reference: String,
        pub reply: String,
        pub status: String,
        pub received_at: String,
    }

    /// One agent turn.
    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase", default)]
    pub struct Job {
        pub id: String,
        pub message_id: String,
        pub sender: String,
        pub prompt: String,
        pub status: String,
        pub result: String,
        pub error: String,
        pub started_at: String,
        pub finished_at: String,
    }

    /// `/status`: how many messages are logged, whether the webhook secret
    /// is configured, whether the Photon line is connected.
    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(default)]
    pub struct Status {
        pub messages: i64,
        pub hook: bool,
        pub line: bool,
    }

    #[derive(Deserialize)]
    struct Messages {
        #[serde(default, deserialize_with = "null_vec")]
        messages: Vec<Message>,
    }

    #[derive(Deserialize)]
    struct Jobs {
        #[serde(default, deserialize_with = "null_vec")]
        jobs: Vec<Job>,
    }

    /// Newest first. Admin API: write key only, private ingress only.
    pub async fn messages(s: &Session, limit: u32) -> Result<Loaded<Vec<Message>>, ApiError> {
        let l: Loaded<Messages> =
            s.load(SERVICE, &format!("/api/admin/messages?limit={}", limit.clamp(1, 500))).await?;
        Ok(Loaded { value: l.value.messages, etag: l.etag, freshness: l.freshness })
    }

    pub async fn jobs(s: &Session, limit: u32) -> Result<Loaded<Vec<Job>>, ApiError> {
        let l: Loaded<Jobs> = s.load(SERVICE, &format!("/api/admin/jobs?limit={}", limit.clamp(1, 500))).await?;
        Ok(Loaded { value: l.value.jobs, etag: l.etag, freshness: l.freshness })
    }

    pub async fn status(s: &Session) -> Result<Status, ApiError> {
        let v = crate::api::status(s, SERVICE).await?;
        serde_json::from_value(v).map_err(|e| ApiError::Decode(e.to_string()))
    }
}

// ── backup ───────────────────────────────────────────────────────────────

pub mod backup {
    use super::*;
    pub const SERVICE: &str = "backup";

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase", default)]
    pub struct Snapshot {
        pub app: String,
        pub cid: String,
        pub size: i64,
        pub created_at: String,
    }

    #[derive(Deserialize)]
    struct Snapshots {
        #[serde(default, deserialize_with = "null_vec")]
        snapshots: Vec<Snapshot>,
    }

    /// Every snapshot in the registry. BACKUP_API_KEY only (no minted keys);
    /// 503 when the server has none configured.
    pub async fn snapshots(s: &Session) -> Result<Loaded<Vec<Snapshot>>, ApiError> {
        let l: Loaded<Snapshots> = s.load(SERVICE, "/api/admin/snapshots").await?;
        Ok(Loaded { value: l.value.snapshots, etag: l.etag, freshness: l.freshness })
    }

    /// One app's snapshots, newest first, with their total size.
    #[derive(Clone, Debug, PartialEq, Default)]
    pub struct Group {
        pub app: String,
        pub total: i64,
        pub snapshots: Vec<Snapshot>,
    }

    /// Group by app (apps alphabetical), newest first within each.
    pub fn group(list: &[Snapshot]) -> Vec<Group> {
        let mut by: BTreeMap<String, Vec<Snapshot>> = BTreeMap::new();
        for s in list {
            by.entry(s.app.clone()).or_default().push(s.clone());
        }
        by.into_iter()
            .map(|(app, mut snapshots)| {
                snapshots.sort_by(|a, b| b.created_at.cmp(&a.created_at));
                let total = snapshots.iter().map(|s| s.size).sum();
                Group { app, total, snapshots }
            })
            .collect()
    }
}

// ── apex ─────────────────────────────────────────────────────────────────

pub mod apex {
    use super::*;
    pub const SERVICE: &str = "apex";

    /// The public profile document: GitHub-flavoured markdown per section
    /// (feed, writing, daily). A section whose upstream failed is absent.
    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase", default)]
    pub struct Profile {
        pub sections: BTreeMap<String, String>,
        pub updated_at: String,
    }

    pub async fn profile(s: &Session) -> Result<Loaded<Profile>, ApiError> {
        s.load(SERVICE, "/api/profile").await
    }
}

// ── daily ────────────────────────────────────────────────────────────────

pub mod daily {
    use super::*;
    pub const SERVICE: &str = "daily";

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(default)]
    pub struct Zone {
        pub name: String,
        #[serde(deserialize_with = "null_vec")]
        pub colors: Vec<String>,
        pub wash: String,
    }

    /// One day's generative plate: where it sits in the 4-D walk, its biome
    /// and zone (with the palette), and the CID of its SVG.
    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(default)]
    pub struct Art {
        pub date: String,
        #[serde(deserialize_with = "null_vec")]
        pub coord: Vec<i64>,
        pub biome: String,
        pub zone: Zone,
        pub cid: String,
    }

    pub async fn art(s: &Session, date: Option<&str>) -> Result<Loaded<Art>, ApiError> {
        match date {
            Some(d) => s.load(SERVICE, &format!("/api/art/{}", enc(d))).await,
            None => s.load(SERVICE, "/api/art").await,
        }
    }

    /// Cap for a remote image (APOD's full-size images run to ~10 MB).
    pub const REMOTE_CAP: usize = 40 << 20;

    fn remote_client() -> &'static reqwest::Client {
        static C: OnceLock<reqwest::Client> = OnceLock::new();
        C.get_or_init(|| {
            reqwest::Client::builder()
                .user_agent(concat!("farfield-desktop/", env!("CARGO_PKG_VERSION")))
                // public images on a CDN: a short redirect chain is normal,
                // and nothing secret rides on these requests
                .redirect(reqwest::redirect::Policy::limited(3))
                .connect_timeout(Duration::from_secs(10))
                .timeout(Duration::from_secs(60))
                .build()
                .expect("remote image client")
        })
    }

    fn limiter() -> &'static tokio::sync::Semaphore {
        static S: OnceLock<tokio::sync::Semaphore> = OnceLock::new();
        S.get_or_init(|| tokio::sync::Semaphore::new(6))
    }

    /// Fetch a remote (third-party, http/https) image the daily index points
    /// at — never with a credential, never through a service client, at most
    /// six at a time, bounded in size.
    pub async fn remote_image(url: &str) -> Result<bytes::Bytes, ApiError> {
        let u = url::Url::parse(url).map_err(|e| ApiError::BadRequest(e.to_string()))?;
        if !matches!(u.scheme(), "http" | "https") {
            return Err(ApiError::BadRequest(format!("not an http(s) URL: {url}")));
        }
        let _permit = limiter().acquire().await.map_err(|_| ApiError::Cancelled)?;
        let r = remote_client()
            .request(Method::GET, u)
            .header(header::ACCEPT, "image/*")
            .send()
            .await
            .map_err(|e| ApiError::Offline(e.to_string()))?;
        let st = r.status();
        if !st.is_success() {
            return Err(match st.as_u16() {
                404 | 410 => ApiError::NotFound,
                s => ApiError::Server { status: s, message: format!("image fetch: {st}") },
            });
        }
        // NASA retired many old APOD image paths: they now redirect to an
        // HTML page, which must not reach the image decoder as "bytes"
        let ct = r.headers().get(header::CONTENT_TYPE).and_then(|v| v.to_str().ok()).unwrap_or("").to_ascii_lowercase();
        if ct.starts_with("text/") || ct.contains("html") {
            return Err(ApiError::Decode("the source no longer serves this image (it answers with a web page)".into()));
        }
        ServiceClient::body(r, REMOTE_CAP).await
    }

    /// The raw plate SVG for a date (or today) — `/art/{date}.svg`; the bare
    /// `/art/{date}` is the HTML page.
    pub async fn art_svg(s: &Session, date: Option<&str>) -> Result<bytes::Bytes, ApiError> {
        let c = s.client(SERVICE)?;
        let path = match date {
            Some(d) => format!("/art/{}.svg", enc(d)),
            None => "/art.svg".into(),
        };
        let r = c.send(c.request(Method::GET, &path)?, false).await?;
        ServiceClient::body(r, 8 << 20).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn go_nil_slices_decode_as_empty() {
        let t: pulse::Traffic = serde_json::from_str(
            r#"{"app":"","from":"2026-01-01","to":"2026-01-02","apps":null,"hitsPerDay":null,"uniquesPerDay":[{"day":"2026-01-01","n":3}],"topPaths":null,"statusMix":null,"topReferrers":null}"#,
        )
        .unwrap();
        assert!(t.apps.is_empty() && t.top_paths.is_empty());
        assert_eq!(t.uniques_per_day[0].n, 3);
        let o: pulse::Overview = serde_json::from_str(r#"{"targets":null,"incidents":null}"#).unwrap();
        assert!(o.targets.is_empty() && o.incidents.is_empty());
        let p: apex::Profile = serde_json::from_str(r#"{"sections":{}}"#).unwrap();
        assert!(p.sections.is_empty() && p.updated_at.is_empty());
    }

    #[test]
    fn overview_row_decodes() {
        let o: pulse::Overview = serde_json::from_str(
            r#"{"targets":[{"id":1,"name":"content","url":"http://x/status","method":"GET","expectedStatus":200,"intervalS":60,"enabled":true,"createdAt":"t","last":{"ts":"t","statusCode":200,"latencyMs":12,"ok":true},"up24h":"100.00%","up7d":"—","up30d":"—","incident":{"id":4,"targetId":1,"openedAt":"t","closedAt":"","lastErr":"boom"}}],"incidents":[]}"#,
        )
        .unwrap();
        let t = &o.targets[0];
        assert_eq!(t.up(), Some(true));
        assert_eq!(t.last.as_ref().unwrap().latency_ms, 12);
        assert_eq!(t.up_24h, "100.00%");
        assert!(t.incident.as_ref().unwrap().open());
    }

    #[test]
    fn snapshots_group_newest_first() {
        use backup::Snapshot;
        let s = |app: &str, at: &str, size| Snapshot { app: app.into(), cid: at.into(), size, created_at: at.into() };
        let g = backup::group(&[s("pulse", "1", 10), s("content", "2", 5), s("pulse", "3", 7)]);
        assert_eq!(g.len(), 2);
        assert_eq!(g[0].app, "content");
        assert_eq!(g[1].total, 17);
        assert_eq!(g[1].snapshots[0].created_at, "3");
    }
}
