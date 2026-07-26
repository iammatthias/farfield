# Hosting

What runs on the GNAR homelab, and how a request reaches it. This lives in the
farfield repo because farfield is most of what the box runs and this repo is
version-controlled — but it describes the whole host, including projects that
live elsewhere.

Keep it current when a service is added, removed, or re-pointed.

## The box

| | |
|---|---|
| Host | `homelab.local` (Linux x86_64) |
| SSH | `ssh iam@homelab.local`, passwordless sudo |
| Projects | `~/projects/` — one git checkout per project |
| Ingress stack | `/srv/stack` — Caddy, cloudflared, Tailscale (not in this repo) |

## How a request arrives

```
public DNS (*.farfield.systems)
  → Cloudflare edge          bot filtering, ~100 MB body cap
  → gnar-cloudflared         outbound tunnel, no inbound ports open
  → gnar-caddy               host → port routing
  → host.docker.internal:PORT ( = 172.17.0.1, the docker0 gateway )
  → the app container
```

Two consequences worth remembering:

- **Caddy is a container** sharing `gnar-tailscale`'s network namespace, so it
  reaches apps through `host.docker.internal`, not `localhost`. Containers
  therefore publish on `FARFIELD_BIND_IP=172.17.0.1` — binding loopback breaks
  ingress, and binding `0.0.0.0` would expose every admin UI to the LAN.
- The app sees Caddy as the peer, so the real client address arrives in
  `CF-Connecting-IP`. That header is only believed from a trusted peer — see
  `FARFIELD_TRUSTED_PROXIES` in `.env.example`.

## Hosted projects

### farfield — `~/projects/farfield` (this repo)

Fifteen Go services, one compose project, one shared `./data` bind mount.
Deploy with the `farfield-deploy` skill. All run as uid 65532 with every
Linux capability dropped.

| Host | Port | App |
|---|---|---|
| `farfield.systems` | 8790 | apex — landing page + docs |
| `content.farfield.systems` | 8787 | content — entries, series, fleet search |
| `feed.farfield.systems` | 8788 | feed — short posts |
| `blobs.farfield.systems` | 8789 | blobs — content-addressed media (R2) |
| `library.farfield.systems`, `opds.farfield.systems` | 8797 | library — EPUBs + OPDS catalog |
| `bookmarks.farfield.systems` | 8793 | bookmarks |
| `daily.farfield.systems` | 8792 | daily — photo, art, sudoku, wordle |
| `calendar.farfield.systems` | — | 301 → `daily` (legacy name) |
| `qr.farfield.systems` | 8794 | qr |
| `scrap.farfield.systems` | 8799 | scrap — pastes |
| `sideload.farfield.systems` | 8800 | sideload — iOS builds |
| `keys.farfield.systems` | 8801 | keys — scoped API keys |
| `pulse.farfield.systems` | 8798 | pulse — traffic telemetry |
| `bard.farfield.systems` | 8795 | bard |
| `dead-presidents.farfield.systems` | 8796 | dead-presidents |
| — (tailnet only) | 8791 | backup — snapshots every app's DB to R2 |

`backup` is deliberately not in Caddy: it is reachable over Tailscale only.

### frame-skylight — `~/projects/frame-skylight`

Custom backend for the Skylight digital photo frame. One container,
`skylight-icloud-sync`, from
`~/projects/frame-skylight/docker/icloud-frame-sync`.

| | |
|---|---|
| Port | `8780` — JSON status endpoint, **`network_mode: host`** |
| Runs as | root (unlike farfield) |
| Not public | no Caddy entry; LAN-reachable only |
| What it does | polls an iCloud shared album every 300 s and reconciles it into the frame's local slideshow database over network-adb |
| Frame | `192.168.50.72:5555` (adb over TCP) |

It binds `0.0.0.0:8780`, so its status endpoint — which reports the frame's
address and album contents — is readable by anything on the LAN. Not a
credential leak, but it is the one service on this box still exposed that way.

## Operational notes

- **Data.** Every farfield app bind-mounts `./data`. It is host-side,
  gitignored, and owned by `65532:65532`. If a container is ever run as a
  different uid, chown accordingly or every app fails to open its database.
- **Secrets** live in `~/projects/farfield/.env` on the host only. When
  inspecting it, match narrowly — a broad grep prints values.
- **Backups.** The `backup` app snapshots every `*.sqlite` in `./data` to R2
  every 6 hours and shortly after boot. Snapshots are content-addressed, so an
  unchanged database uploads nothing.
- **Logs** use the json-file driver, capped at 10 MB × 3 per container.
  Recreating a container discards its logs.
