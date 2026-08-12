# pulse — uptime + traffic analytics

pulse is farfield's observability service: it watches the fleet from the
inside (is every endpoint up?) and from the outside (who is visiting?). One
session-gated console at `https://pulse.farfield.systems` shows both. The only
public surface is `GET /status`, which exposes nothing beyond a target count.

It is two halves that meet in `pulse.sqlite`:

## The checker (uptime)

`apps/pulse` probes configured HTTP targets on their own cadences and records
every outcome in the `checks` table. Failure handling is two-stage so the
Cloudflare-tunnel hairpin path — which drops single probes regularly — does
not page as an outage:

1. A failed probe is retried once after a short pause; only the retry's
   outcome is recorded. A recorded fail means two consecutive misses seconds
   apart.
2. An incident opens only after `PULSE_FAIL_THRESHOLD` consecutive recorded
   fails (default 2). The fails themselves are always recorded — uptime
   percentages stay honest — only the incident transition debounces.
   Recovery closes on the first ok check, undebounced.

The console shows 24h / 7d / 30d uptime per target, latency sparklines, and
the incident log.

## The traffic pipeline (analytics)

Every other app wraps its handler in `lib/pulse`, which records one row per
request into a **telemetry sidecar** — `data/pulse/<app>.sqlite`, never the
app's own database. The sidecar location is derived from the app's database
handle (`PRAGMA database_list`), so apps configure nothing.

Privacy is by construction: no raw IP and no raw User-Agent are ever stored.
A request is attributed to a visitor key — 8 bytes of
`sha256(daySalt + clientIP + userAgent)` — where the day salt lives only in
memory and rotates at UTC midnight, so visitors cannot be linked across days
even with the database in hand. `/status` probes and `/static/` asset noise
are never recorded. Recording never blocks a request: rows go through a
bounded queue to a single writer goroutine, and overflow drops events rather
than slowing the page.

The **collector** in the pulse app sweeps the sidecar directory every
`PULSE_COLLECT_INTERVAL` (default 5m), reads each app's new rows by cursor
(read-only), and folds them into daily aggregates: `hits_daily` (hits +
exact uniques per day/app/path) and `referrers_daily`. A cursor-rewind guard
resets any app whose request log was rebuilt (ids restarting below the stored
cursor), so a wiped or migrated sidecar is re-collected rather than silently
ignored.

## Retention

Raw telemetry is rolling; aggregates and history are forever.

| data | table | keeps | pruned by |
|---|---|---|---|
| raw requests | sidecar `requests` | 14 days | lib/pulse, daily |
| uptime probes | `checks` | 45 days | checker, daily |
| unique-visitor scratch | `vkeys_seen` | today + yesterday | collector, each sweep |
| daily aggregates | `hits_daily`, `referrers_daily` | forever | — |
| incidents | `incidents` | forever | — |

The sidecars are deliberately **not** backed up — the backup app's `*.sqlite`
glob does not descend into `data/pulse/`. That is the point of the layout:
traffic alone no longer changes an app's database, so an app whose real data
is unchanged snapshots to the same CID and uploads nothing. `pulse.sqlite`
itself (aggregates, checks, incidents) is backed up like any other app
database and is the fleet's largest snapshot.

## Env

| var | default | meaning |
|---|---|---|
| `PULSE_PORT` | 8798 | HTTP port |
| `PULSE_DB_PATH` | `pulse.sqlite` | database; sidecars live in `pulse/` beside it |
| `PULSE_COLLECT_INTERVAL` | `5m` | collector sweep cadence; `0` disables |
| `PULSE_FAIL_THRESHOLD` | `2` | consecutive recorded fails that open an incident |
