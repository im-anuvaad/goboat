# goboat Architecture

Reference for whoever maintains goboat. The rules for hosted apps are in [app-contract.md](app-contract.md); installing and using it is in the [README](../README.md).

## 1. What and why

A single static Go binary that builds and hosts a few small server-rendered **Go + SQLite** web apps on one Ubuntu 24.04+ VPS behind Cloudflare, for **one user who doesn't do ops**.

- **One SSH command, then only the dashboard.** The installer prints a one-time password; the admin account, tokens, dashboard domain, bucket, apps, branches, env vars, deploys, logs, backups, restores and server moves are all in the browser. No admin CLI, no config file. The only other interface is a read-only status API (D30).
- **goboat builds.** Connect a repository and a branch; goboat downloads the source of a commit, builds it with the Go toolchain on the server and runs it (D20). No release workflow in the app's repository.
- **Cloudflare in front, Full (strict), origin reachable only through the user's own Cloudflare account** (IP filter + zone-level Authenticated Origin Pulls). Every Cloudflare object goboat creates is tracked until it is deleted (D24).
- **SQLite backups** to any S3-compatible bucket, RPO ≤ 10 min (D25).
- **Server moves** with one command and one click, downtime under a minute (D27).
- **Small:** < 30 MB RSS idle, no other daemons. Builds are the only heavy work and are bounded (§7.3).

Non-goals: several machines or users, containers or strong multi-tenant isolation (apps are trusted), zero-downtime deploys (stop-then-start is safe with one SQLite writer), deploys on push, languages other than Go, cgo, private Go module dependencies, git submodules, non-Cloudflare setups, self-update.

Assumptions: Ubuntu ≥ 24.04 (x86_64/arm64), systemd, cgroup v2, **2 vCPU / 2 GB RAM recommended** (1–2 production apps, or 3–4 small ones), a 2 GB swap file created by the installer, public ports 22 and 443 only; 1 dashboard user, 1 GitHub token, 1 Cloudflare API token, 1 bucket.

## 2. Stack and code

Go, `CGO_ENABLED=0`, static `linux/amd64` + `linux/arm64` binaries for goboat itself. stdlib for HTTP, TLS, reverse proxy, GitHub REST, tar/gzip; `modernc.org/sqlite` (pure Go) for goboat's own DB; `x/crypto/argon2` and `x/crypto/hkdf`; `minio-go` for S3/R2; `klauspost/compress/zstd`; `BurntSushi/toml` for `goboat.toml`. Dashboard: `html/template`, one CSS file, a little vanilla JS, all `go:embed`. Rule: add a module only when the stdlib would need ~300+ lines of risky code.

```
cmd/goboat/       serve (systemd), setup-host and move-in (run by the installer), version
contrib/systemd/  goboat.service, embedded in the binary
internal/
  store/          SQLite (WAL, single writer), embedded migrations
  secrets/        master.key, AES-256-GCM seal/open with purpose as associated data
  auth/           argon2id, sessions, CSRF, rate limit, one-time password
  web/            dashboard handlers, templates, static, SSE, SVG charts, read-only JSON API
  apps/           app registry (apps + sealed env), create/update/delete, restore on add
  edge/           listener, IP filter, setup address, certificates, AOP, router, proxy, Cloudflare sync + cleanup
  cloudflare/     minimal REST client
  github/         token check, repos, branches, commits, source archives
  source/         safe unpack of source archives and the Go toolchain, ELF check
  manifest/       goboat.toml parsing
  builder/        go build in a bounded cgroup, toolchain and cache management
  deployer/       queue + pipeline, builds, rollback, history
  supervisor/     process lifecycle, restart policy, health check
  cgroup/ users/  cgroup v2 tree and limits; per-app system users and the build user
  backup/ s3/     snapshots, change detection, archives, upload, retention, restore, bucket folders
  move/           server-to-server move: bundle, protocol, cutover
  metrics/ logs/  sampling + rollups; rotating logs + live tail
  layout/ hostinfo/ app/ buildinfo/
```

All components are goroutines in one process (the systemd unit), talking through Go interfaces. goboat runs as root (port 443, users, cgroups, chown); apps and builds never do.

## 3. On disk

```
/usr/local/bin/goboat
/usr/local/lib/goboat/go/                Go toolchain installed by the installer (root-owned, read-only)
/etc/goboat/master.key                   32 random bytes (hex), 0600, never overwritten
/etc/systemd/system/goboat.service
/etc/sysctl.d/90-goboat.conf             vm.swappiness=10
/goboat.swap                             2 GB swap file (only if the server had less than 2 GB of swap)
/var/lib/goboat/                         0711
  goboat.db                              all state and settings
  setup/{cert,key}.pem                   self-signed cert of the setup address
  certs/<host>/{cert,key}.pem            Origin CA certs (0600)
  aop/{ca,client}.{pem,key}              goboat's AOP CA (RSA 3072, 10 y) + client cert (RSA 2048, 5 y)
  cache/{mod,build,home}/                Go module cache, build cache, HOME of builds (goboatbuild, 0700)
  work/<app>/<deployment id>/            build workspace: source.tar.gz, src/ (root-owned), app, tmp/ (goboatbuild; emptied at start)
  apps/<app>/builds/                     root:goboat-<app> 0750: only the app can enter its builds
  apps/<app>/builds/<build id>/src/      source tree at the commit, root-owned, read-only for the app (cwd)
  apps/<app>/builds/<build id>/app       the built binary (root-owned)
  apps/<app>/current -> builds/<build id>
  apps/<app>/data/  tmp/                 DATA_DIR, TMPDIR (0700, app user)
  apps/<app>/logs/app.log[.1-3]          runtime log (10 MB × 3)
  apps/<app>/logs/deploys/<id>.log       build + deploy logs (last 20)
  backups/staging/  backups/local/<app>/ temp snapshots; newest archives
  move/                                  bundle and data staging during a server move
  deleted/<app>-<time>/data/             data kept when an app is deleted
```

| Table | Content |
|---|---|
| `settings` | `dashboard_domain`, `dashboard_reached`, `setup_password` (argon2id hash, expiry, used), `api_token` (SHA-256, created, last used), GitHub token*, Cloudflare token*, `s3_config`*, Cloudflare IP ranges, `move_state`*, `moved_to` |
| `users` | id 1 only: email, argon2id hash |
| `sessions` | SHA-256 of the token, csrf, created/last seen, ip, user agent, `setup` flag |
| `apps`, `app_env` | name, repo, branch, domain (required, unique), port, memory limit, CPU limit; env values* |
| `app_state` | desired state (running/stopped) |
| `builds` | the kept builds (at most 3 per app): id (first 12 hex digits of the commit), commit SHA, branch, commit subject, Go version used, `goboat.toml` values (package, args, health path), binary size, build time |
| `deployments` | history: kind (deploy, rollback, move), build id, commit, branch, status (queued, building, deploying, succeeded, failed), error, times; a failed build is a failed deployment with the compiler output in its log |
| `backups` | kind, status, object key, local path, size, sha256, per-database fingerprint, commit, error |
| `edge_certs` | per served host: zone id, Origin CA cert id, expiry |
| `edge_zones` | per zone: SSL mode, AOP cert id/serial (saved right after the upload), AOP enforced |
| `cf_cleanup` | Cloudflare objects to delete (§5.6): kind (`host`, `aop_cert`, `zone`), zone, host, object id, the server's addresses at removal, reason, attempts, last error, created, last try |
| `metrics_1m` | 7 days of 1-minute rollups per scope (`_host`, `_goboat`, app) |

`*` sealed with AES-256-GCM (`master.key`), purpose-bound (`settings:cloudflare_token`, `app_env:<app>:<NAME>` …), never shown in the UI. Migrations are numbered, one transaction each; a failing one rolls back and goboat refuses to start; a schema newer than the binary is refused naming both versions. goboat refuses to start if `master.key` is group/other-accessible or malformed.

## 4. Install, setup address, updates

`install.sh` (the only SSH step) asks nothing:

1. Checks Ubuntu ≥ 24.04, systemd, cgroup v2, the CPU, at least 1.8 GB RAM and 10 GB free disk.
2. Downloads `goboat-linux-<arch>` + `checksums.txt` from the latest release (or `GOBOAT_VERSION=vX.Y.Z`), verifies SHA-256 and that the binary runs.
3. Stops goboat, installs the binary and runs `goboat setup-host`, which:
   - refuses to continue if another program holds port 443 (naming it);
   - creates the directories, `master.key` and the `goboatbuild` user;
   - installs the latest stable **Go toolchain**: reads `https://go.dev/dl/?mode=json`, downloads `go<ver>.linux-<arch>.tar.gz` from `dl.google.com`, verifies its SHA-256 from that index, unpacks it into `/usr/local/lib/goboat/go.new` and swaps it in. A Go installed elsewhere on the server is never touched. Re-running the installer updates it; if go.dev can't be reached, an installed toolchain is kept;
   - **swap:** if the server has less than 2 GB of swap, creates `/goboat.swap` (2 GB, 0600, `mkswap`, `swapon`, an `/etc/fstab` line) and sets `vm.swappiness=10`. An existing swap setup of 2 GB or more is left as it is;
   - writes and enables the embedded systemd unit (D10), runs migrations, stores a **new one-time password** (argon2id, 24 h), starts goboat and prints the setup address `https://<public IPv4>`, the password and the dashboard domain if any.

   Measured on a 2 vCPU / 2 GB VPS: an update including the Go download and the swap file takes about 15 s.

When the installer is given a move code (§10) it runs `goboat move-in` instead of printing a password. Re-running the installer is the **update** (keeps everything; apps restart) and the **recovery** path (forgotten password, broken domain). goboat never updates itself.

Unit essentials: `ExecStart=goboat serve`, `Restart=always` with growing delay (2 s → 60 s, never gives up), `Delegate=yes`, `KillMode=mixed`, `TimeoutStopSec=45s`, `LimitNOFILE=65536`.

**Setup address** (D11): before a dashboard domain works, the dashboard is served on `https://<server IP>` with the self-signed cert.
- Setup is **open** while the dashboard domain is not *ready*, or while an unexpired one-time password exists that has not been used to set up the account. The domain is *ready* when its Origin CA cert is installed, AOP for its zone is enforced or rolling out (§5.3), and either a request for it arrived through Cloudflare with goboat's AOP client cert (remembered per domain) or the zone is active and its SSL mode is known to be Full/Full (strict).
- While open, non-Cloudflare connections pass the IP filter as *setup connections*: self-signed cert whatever the SNI, no client cert, **only the dashboard** (never apps), client IP = TCP peer (`CF-Connecting-IP` ignored), CSRF origin = `https://<Host>`.
- While the dashboard domain is ready, the setup address accepts only the one-time password, never the admin password.
- When setup closes, non-Cloudflare connections are refused before TLS again.
- Signing in with the one-time password (email empty) gives a `setup` session that can only set the admin email and a password (≥ 12 chars); saving consumes the password and revokes all sessions. First-run order: Cloudflare token → dashboard domain → GitHub token, bucket → apps; the home page lists what is missing.

## 5. Edge

### 5.1 Listener and IP filter
Only `:443` (no `:80`; the user enables Cloudflare's *Always Use HTTPS*). A custom listener closes connections from outside Cloudflare's ranges **before TLS** (except setup connections). Ranges come from `/client/v4/ips` at start and every 24 h, the last good list is saved, a compiled-in list is the fallback, and implausible lists are refused. Rejections are counted and logged at most once a minute. Failed handshakes are logged: the first at once, then a count per minute. Timeouts: read header 10 s, idle 120 s, no write timeout.

### 5.2 Origin certificates (Full strict)
Per hostname (dashboard + each app domain): local ECDSA P-256 key + CSR → Cloudflare Origin CA (`origin-ecc`, 5475 days). Chosen by SNI; **unknown SNI fails the handshake**; TLS 1.2+, h2 and http/1.1. Sync (at start, every 6 h, after changes, *Sync now*) re-issues certs with < 30 days left. A replaced cert goes into `cf_cleanup` in the same transaction that records the new one (§5.6). The zone SSL mode is checked, never changed automatically; the dashboard warns and offers *Set Full (strict)*.

### 5.3 Authenticated Origin Pulls
The IP filter only proves "some Cloudflare account"; zone-level AOP with goboat's own CA proves "yours" (D5). Per zone: upload the client cert + key (`origin_tls_client_auth`), **save the returned id at once**, turn zone AOP on, wait until Cloudflare reports the cert `active` (≤ 3 min per sync), then **wait 10 more minutes** for Cloudflare's edge to roll it out (enforcing earlier gave 2–5 % 520s for ~5 minutes) before requiring `RequireAndVerifyClientCert` for hosts in that zone. The same wait follows any re-upload or re-enable; a timer syncs when it ends; during it no client cert is requested. The enforced state is saved in `edge_zones`, applies at once after a restart, and stays on if the API is merely failing. If someone deletes the cert or disables AOP in Cloudflare, the next sync restores it. The client cert is renewed 60 days before expiry (upload, wait active, then the old one goes to `cf_cleanup`).

**Zone-level AOP in a zone goboat serves belongs to goboat** (D24): every other zone-level AOP certificate in that zone is deleted (§5.6). Other origins in such a zone must use per-hostname AOP or none.

### 5.4 Router and proxy
`Host` (lowercased, port removed) → dashboard, an app's `httputil.ReverseProxy` to `127.0.0.1:<port>`, or 404; `Host` must equal the SNI or 421. The proxy replaces `X-Forwarded-For`/`X-Real-IP` with `CF-Connecting-IP`, sets `X-Forwarded-Proto: https` and `X-Forwarded-Host`, keeps `Host`, flushes immediately (`FlushInterval: -1`; SSE, long-poll, WebSockets work), dials with a 5 s timeout. Switching builds → 503 (`Retry-After: 3`), starting → 503, stopped/crashed/never deployed → 503, proxy error → 502; goboat pages carry `X-Goboat-Page` and `no-store`. Per-app counters (requests, status classes, latency histogram) feed monitoring.

### 5.5 Cloudflare token, DNS, domains
Token permissions: Zone Read, DNS Edit, SSL and Certificates Edit, Zone Settings Edit; saving it verifies it, lists readable zones and names missing permissions. For each domain: zone by longest suffix (a domain in no zone is refused when saved), proxied `A` (+ `AAAA` if the server has IPv6) created or updated in place; conflicting CNAMEs or duplicates are reported, never touched. Edge sync is best effort per host. Public IPv4/IPv6 are detected via `https://www.cloudflare.com/cdn-cgi/trace`. The dashboard domain (needs the token, can't be an app's domain) is routed at once and set up in the background; sessions are per domain.

**Every app has a domain** (D21): Add app refuses to create an app without one ("Enter the domain the app will be served on"). The app's Settings can change the domain but not clear it.

### 5.6 Cloudflare cleanup
Every object goboat creates at Cloudflare is either in use (a served hostname's DNS records and certificate, a zone's current AOP certificate) or queued in `cf_cleanup` until Cloudflare confirms it is gone. Nothing is forgotten when an API call fails or goboat restarts half-way.

- **Removing a hostname** (app deleted, domain changed, dashboard domain changed) queues a `host` item and forgets its certificate record in **one transaction**, deletes the local certificate files and starts a background sync. It never calls Cloudflare and never waits for a running sync.
- **Every sync works through the queue first**, then syncs the served hostnames and zones:
  - `host`: delete the hostname's A/AAAA records **whose content is one of this server's addresses** (at removal or now; after a server move they point elsewhere and are left alone), and revoke every Origin CA certificate for exactly that hostname. Skipped (and dropped) if the hostname is served again.
  - `aop_cert`: delete a replaced zone-level AOP certificate, but only once the current one is active.
  - `zone`: when no configured hostname is in a zone any more: turn zone-level AOP off, delete the AOP certificates there and forget the zone.
  - Success or 404 removes the item; any other error is stored on it with an attempt count, and the whole sync is retried after 1, 5 and 15 minutes, then every 6 hours.
- **Comparison with Cloudflare per zone goboat serves**, on every sync:
  - *Origin CA* (`GET /certificates?zone_id=`, which lists non-revoked certificates with their PEM): a certificate for exactly one served hostname whose serial differs from the installed one is revoked. This catches certificates whose id was never saved and those of earlier installs.
  - *AOP* (`GET /zones/<z>/origin_tls_client_auth`): once goboat's certificate is active, every other zone-level certificate is deleted (D24).
  - Certificates for hostnames goboat doesn't serve are listed under **Settings → Cloudflare → Unused certificates** with a **Revoke** button each. goboat never revokes them by itself: they may belong to another origin.
- **Configured hostnames are never cleaned up**, even when not routed: the list comes from the database (every app's domain and the dashboard domain), so an app that fails to load keeps its DNS record and certificate. If that list can't be read, the sync does no cleanup at all; zones are released only when every configured hostname's zone is known.
- A certificate record for a hostname that is not configured (a sync racing a removal, or a removal by an older goboat) is turned into a `host` item.
- goboat never orders edge certificates. The Universal SSL certificate packs Cloudflare shows under Edge Certificates (an active one and a backup) are Cloudflare's own.
- **Settings → Cloudflare → Cleanup pending** shows queued items with their last error; the home page warns when one has failed for over 24 hours.

## 6. Apps and processes

- **Names** come from the repository name: lowercased, `_`/`.` → `-`, 2–24 chars `a-z0-9-`, starting with a letter. One repo = one app. Ports are assigned once from 20001 up.
- **Add app** form: repository (with suggestions from the GitHub token), branch (the repository's branches, filled in by `app.js`; empty = the default branch), domain (required: "the domain is missing: enter the domain the app will be served on"), memory and CPU (drop-downs, §6.1), the backup folder to start from (§8). The form shows how much memory all apps' limits add up to against the server's RAM; creating an app or changing a limit that takes the total above 75 % adds a note to the confirmation.
- **Users:** `goboat-<app>` (system, no home, `nologin`); `data/` and `tmp/` 0700 owned by it; builds root-owned; other apps' and goboat's files unreadable. Builds run as `goboatbuild` (§7.3), which no app name can collide with.
- **cgroups:** goboat owns `system.slice/goboat.service/` (`Delegate=yes`), moves itself into `supervisor/`, apps run in `apps/<app>/`, builds in `builds/`. Apps get `memory.max`, `memory.swap.max=0`, `memory.zswap.max=0`, `cpu.max`, `pids.max` 256; no `memory.high`; `RLIMIT_NOFILE` 4096 via prlimit. Processes start inside their cgroup (`CLONE_INTO_CGROUP`) with the app's uid/gid, `Setpgid`, `Pdeathsig=SIGKILL`, cwd = the build's `src/`, a clean env (`PATH`, `LANG`, contract vars, user env, and `GOMEMLIMIT` = 90 % of `memory.max` unless the user sets one). Killing = SIGKILL every PID in the cgroup until empty, **never `cgroup.kill`** (D8). `memory.events` OOM kills are recorded.
- **Supervisor:** `stopped → starting → running → stopping`, plus `crashed`, `deploying`. Running = TCP connect then `GET health_path` 2xx/3xx within 30 s. Stop = SIGTERM, 10 s, kill the cgroup. Restarts with backoff 1 s → 60 s; 5 exits in 5 min → `crashed` until the user starts it. At start, apps with desired state `running` start one at a time.
- **Logs:** stdout/stderr lines get a timestamp and stream tag → `app.log` (10 MB × 3) + a 1000-line ring buffer streamed to the dashboard over SSE; lines > 16 KB are split.
- **Settings** (app page): branch (applies on the next deploy), domain (re-routed at once; can't be cleared), memory and CPU (same drop-downs; apply on restart, the page says "restart needed" and offers **Restart**; D22). Env names `PORT`, `HOST`, `DATA_DIR`, `TMPDIR`, `GOBOAT_*` are reserved.
- **Delete** (type the name): refused while building or deploying; stop, remove cgroup, rows, builds, logs, user; the domain's Cloudflare objects go to `cf_cleanup` (§5.6); `DATA_DIR` is kept in `deleted/` unless unticked; backups are kept unless "also delete its backups" is ticked.

### 6.1 Limits

| | Choices | Default |
|---|---|---|
| Memory | 128 MB, 256 MB, 512 MB, 1 GB | 256 MB |
| CPU | 0.5, 1, 2 cores | 1 |
| Processes | fixed 256 | |
| Open files | fixed 4096 | |

The dashboard is the only source of limits (D23): `goboat.toml` has no limit keys and rejects them with a message pointing to the app's Settings. Only the values above are accepted from the forms; an app whose limit was set before the choices existed keeps it (shown as "(current)") until it is changed. A running app keeps its old limits until it restarts: saving says so, and the app page shows "Restart to apply" until then.

## 7. Builds and deploys

### 7.1 What is deployed
The app page lists the 10 newest commits of the app's branch (`GET /repos/<o>/<r>/commits?sha=<branch>&per_page=10`, cached 1 min, newest preselected) with their short SHA, subject, author and date. goboat deploys **only on click**. One build or deployment runs at a time host-wide; others queue and the page shows their position. A build is identified by app + commit SHA (its id is the first 12 hex digits); the current build and the newest others, 3 in all, are kept for rollback. Deploying a commit that is still kept switches to it without rebuilding.

### 7.2 Pipeline

```
1. Commit already built and kept → use that build (skip 2–6)
2. GitHub API: GET /repos/<o>/<r>/commits/<sha>, then /tarball/<sha> (stored token; the redirect to codeload is followed without it; ≤ 100 MB)   [app keeps serving]
3. Unpack into work/<app>/<id>/ (no absolute paths, "..", devices, escaping symlinks; ≤ 500 MB, ≤ 50 000 files)
4. Read goboat.toml (contract, package, args, health_path); refuse unknown keys
5. go build as goboatbuild in the build cgroup (§7.3), output to the build log
6. Check the output is an ELF executable for this CPU; move src/ + app to apps/<app>/builds/<id>/ (root-owned)
── downtime (stop old + start new + its health check) ──
7. Router: deploying (503)   8. Stop old   9. Pre-deploy backup (snapshot; upload in background)
10. Atomic swap of `current`   11. Empty tmp/, start, health check
12a. Healthy → serve, succeeded, prune to 3 kept builds
12b. Unhealthy → stop, point `current` back, start the old build if it was running, keep the failed build out of the kept list, failed
     (the DB is not restored; the UI offers the pre-deploy backup)
```

A failure in 2–6 never touches the running app, and errors name the cause (commit not found, archive too large, missing or invalid `goboat.toml`, the `go build` error with the log, out of memory, timeout). **Rollback** switches to a kept build without rebuilding. After a crash or restart, unfinished builds and deployments are marked failed and `work/` is emptied; a switch that started (7–12) always completes. Build metadata lives in `builds`, so restarts don't re-read `goboat.toml`.

### 7.3 The build sandbox

| | |
|---|---|
| User | `goboatbuild` (system, no login); owns `work/<app>/<id>/` and `cache/` only |
| cgroup | one per build, `goboat.service/builds/<app>-<deployment id>/`, removed afterwards (and at start): `memory.max` 1.5 GB or 3/4 of the RAM if that is less (1.4 GB on a "2 GB" VPS), **swap allowed** (`memory.swap.max` left at `max`), `cpu.weight` 20 (apps: 100), `io.weight` 20, `pids.max` 1024. `memory.peak` and `memory.swap.peak` are written to the deploy log |
| Time | 15 minutes, then the cgroup is killed |
| Command | `/usr/local/lib/goboat/go/bin/go build -trimpath -buildvcs=false -ldflags="-s -w" -o <workspace>/app <package>` in the source root |
| Env | `PATH` (toolchain first), `HOME`/`GOPATH` under `cache/home`, `GOMODCACHE=cache/mod`, `GOCACHE=cache/build`, `TMPDIR=<workspace>/tmp`, `GOENV=off`, `GOTELEMETRY=off`, `CGO_ENABLED=0`, `GOTOOLCHAIN=auto`, `GOFLAGS=` (empty); never the app's env vars |
| Network | needed for `proxy.golang.org`, `sum.golang.org` and toolchain downloads |

- `go build` runs no code from the module or its dependencies (no cgo, no build scripts in Go), so the build sandbox mainly bounds resources; `goboat.toml` has no way to run commands.
- `GOCACHE` trims itself (entries unused for 5 days). Before a build, if `cache/mod` is over 5 GB, it is emptied (`go clean -modcache`); the next build downloads what it needs again.
- The Go version used (read from the built binary, so it is the toolchain after `GOTOOLCHAIN` switching) is stored with the build and shown on the app page.
- A build killed by the memory limit (an `oom_kill` in its cgroup's `memory.events`) fails with "the build ran out of memory"; a build over 15 minutes, with "took longer than 15 minutes". Leftover processes are killed with the cgroup either way.
- The deployment records the build's wall time, CPU time (`cpu.stat`), peak memory and peak swap; the deployment page shows them, the swap in a warning colour when it was used.

Measured on a 2 vCPU / 1.9 GB VPS with two apps running (deploy log figures):

| Build | Time | Peak memory | Peak swap | Result |
|---|---|---|---|---|
| Minimal app with `modernc.org/sqlite`, cold caches | 63 s | 1.2 GB | 0 | ok |
| Same app after a one-line change | 1.2 s | 82 MB | 0 | ok |
| App with SQLite, the AWS S3 SDK and the Prometheus client, cold | 32 s | 0.76 GB | 0 | ok |
| Generated 63 MB map literal | 113 s | 1.4 GB | 1.1 GB | ok, using swap |
| Generated 150 MB of map literals | 314 s | 1.4 GB | 2.0 GB | "ran out of memory" |

During every one of these builds a running app answered its health check through Cloudflare without a failure (average 0.4–0.9 s through the proxy from outside, unchanged from idle).

## 8. Backups and restore

| Policy (fixed) | |
|---|---|
| Schedule | every 10 min aligned to UTC (:00, :10 …), **skipped for an app whose databases did not change**; before every deploy/rollback; *Back up now* |
| Retention | 48 h, the newest backup of each app always kept (done by goboat, not bucket rules) |
| Local copies | newest 2 per app; copies not uploaded yet are kept until uploaded or 48 h old, thinned to one per hour after the newest 6 |
| Bucket | `<app>/<UTC time>-<kind>-<id>.tar.zst`; S3 settings sealed, saved only after a test upload/read/delete; empty secret keeps the old one; path-style SigV4 |

- **Scope:** SQLite databases only. Other files in `DATA_DIR` survive deploys (only `TMPDIR` is emptied) but are the app's own business to protect.
- **What:** every file under `DATA_DIR` starting with `SQLite format 3\0` (sub-folders included), skipping `-wal`/`-shm`/`-journal` and `.pre-restore-*`.
- **Change detection** (D26): the fingerprint is a SHA-256 over each database's path and the size and mtime (nanoseconds) of the file and of its `-wal`. It is taken **before** the snapshot and stored on the backup, so a write during the snapshot makes the next check take another backup. A scheduled run compares the current fingerprint with that of the app's latest successful backup and takes none when they are equal (no row, no upload); the app page shows "Last check <time>: unchanged since the backup of <time>". Taking a backup doesn't change the fingerprint itself (checked by a unit test). Pre-deploy and manual backups always run.
- **Snapshot:** `VACUUM INTO` with `busy_timeout=10000` while the app runs, then `PRAGMA quick_check`; sidecar files goboat creates are chowned back to the app user.
- **Archive:** tar (first `manifest.json`: app, kind, time, commit, goboat version, path/size/SHA-256 per DB) + zstd; its SHA-256 goes into `backups`.
- **Upload:** 3 attempts with backoff; scheduled/manual uploads finish before returning, pre-deploy ones in the background; failed uploads are retried after every scheduled run and when S3 settings are saved.
- **Retention** runs once an hour per app (one bucket listing), not after every upload. With every slot used, an app has at most 288 scheduled archives in the bucket; on R2 that is at most 144 uploads per app per day, inside the free tier for a few apps.
- **Overruns:** apps are backed up one at a time; slots that pass while a run is still going are skipped, counted and logged, and the home page warns if one was skipped in the last hour.
- **Measured** (VPS 1, Garage on VPS 2 as the bucket): an idle app made no backup at the next slot ("unchanged since…"); one visit (a SQLite insert) made one at the next slot. With the bucket stopped, 22 backups in a row left 6 local copies and the "only on this server" warning; when the bucket came back, the next slot uploaded all 6 and the warning cleared. Archives older than 48 h were deleted at the first retention pass after a restart, the newest kept.
- **Restore:** refuse while a build or deployment is queued/running; take the local copy if its SHA-256 matches, else download and check; unpack to staging and verify every file (size, SHA-256, safe paths, `quick_check`) — **any failure stops here, the app untouched**; stop the app, move the current DBs and sidecars to `DATA_DIR/.pre-restore-<time>/` (last 2 kept), copy the restored files in (fsync, rename, chown), start again if it was running.
- **Add app** lists the bucket's folders, preselects the app's own name, and restores the newest archive of the chosen folder into the empty `DATA_DIR` before the first deploy (a failure leaves the app created with an error shown). This is the path when the old server is gone; a working server is moved with §10.
- **App page:** the 20 newest backups, then **Show all** (grouped by day). **Badges:** app and home pages show *backup failed* when the latest backup failed or isn't uploaded and warn when no bucket is set.

## 9. Monitoring

Every 10 s: host (`/proc/stat`, `/proc/meminfo` including swap, `/proc/loadavg`, `statfs`), goboat itself (`/proc/self`), each app (`cpu.stat`, `memory.current`, `pids.current`, `io.stat`, `memory.events`) and proxy counters (requests, 5xx, p50/p95 from a fixed histogram 5 ms … 10 s). CPU and I/O are counter deltas. 1 h of samples in memory (360 per scope); each minute is rolled up into `metrics_1m`, pruned after 7 days. Charts are SVGs rendered by goboat (`/charts?scope=&chart=&range=1h|24h|7d`). Warnings (dashboard only): disk > 85 %, memory > 90 %, 64 MB or more written to swap (`pswpout` in `/proc/vmstat`) in the last hour while no build was running or had ended within a minute (swap-ins are not counted: they are pages coming back after a build), OOM kill in the last hour, app > 90 % of its memory limit, crashed, backup failed or local only, backup slot skipped, Cloudflare cleanup failing, cert expiring < 14 days, restart needed, setup step missing.

## 10. Moving to another server

Moves a **working** installation to a new server: all settings, the account, apps, env vars, certificates and the AOP CA, and the apps' latest data. The old server keeps serving until the user switches (D27).

### 10.1 What the user does
1. Old dashboard: **Settings → Move to another server → Start**. It shows a command with a one-time **move code**, valid 1 hour:
   `curl -fsSL https://github.com/im-anuvaad/goboat/releases/latest/download/install.sh | sudo bash -s -- move https://<dashboard domain> <code>`
2. Run it on the new server. The old dashboard shows each step: connected, settings copied, each app building, ready.
3. Press **Switch now**. The page follows the switch and then links to the dashboard domain, now served by the new server.

### 10.2 Protocol
The old server serves `/move/v1/*` on its dashboard domain (through Cloudflare, so the IP filter and AOP stay as they are). The new server is an HTTPS client of that URL. From the code, both derive with HKDF-SHA256 an **auth token** (sent as `Authorization: Bearer`) and an **encryption key** (never sent); every payload is AES-256-GCM-encrypted with that key, so Cloudflare, which terminates TLS, can't read it. The old server keeps the code sealed in `move_state`; wrong tokens count towards the sign-in rate limit; the code expires after 1 hour unless a move is in progress, and one move runs at a time.

| Step | New server | Old server |
|---|---|---|
| 1 | `install.sh move <url> <code>`: installs goboat, Go and swap as usual (§4); `goboat move-in` checks the new server is empty | |
| 2 | `GET /move/v1/hello`: versions must match, else it stops: "update the old server first: run the install command there" | **Freezes configuration**: settings, apps, env vars and limits can't be changed until the move ends or is cancelled. Apps keep running, deploys are refused |
| 3 | `GET /move/v1/bundle`: `goboat.db` snapshot (without sessions, setup password, metrics; backup rows keep only bucket copies), `master.key`, `aop/`, `certs/`. Installed while goboat is stopped; goboat starts in **incoming** mode: no edge sync, no backups, no apps started, setup address closed | Logs the copy |
| 4 | Builds every app at the commit the old server runs (`GET /move/v1/apps`), reporting progress (`POST /move/v1/progress`). Meanwhile it **pre-copies each app's whole `DATA_DIR`** (`GET /move/v1/files/<app>`: a file list with size, mtime, SHA-256, then the files; databases as consistent `VACUUM INTO` snapshots). A failed build stops the move here | Shows progress; **Cancel** possible |
| 5 | Waits (`GET /move/v1/switch`, long poll) | User presses **Switch now** |
| 6 | | Router: every app 503 "moving"; stops apps; stops scheduled backups |
| 7 | Asks for the file list again and fetches only files that are new or changed since the pre-copy (with the apps stopped, the files are final), deletes files that are gone, runs `quick_check` on every database, starts apps, waits for health checks | Serves the changed files |
| 8 | Leaves incoming mode: edge sync points every DNS record (apps and dashboard domain) at the new server; Cloudflare objects are reused unchanged (same Origin CA certs, same AOP cert, so no 10-minute rollout). Backups start. `POST /move/v1/done` | Records `moved_to` = new IP and time, enters **moved** mode |

- **Downtime** is steps 6–8: stopping the apps, sending the files that changed since the pre-copy (usually just the databases), starting the apps and the DNS update (proxied records change at Cloudflare within seconds). For apps with databases of a few MB this is well under a minute.
- **Failure before step 8**: the new server reports the error (`POST /move/v1/failed`); the old server restarts its apps, unfreezes, and shows the error; the user can retry with a new code. The new server deletes everything it received.
- **Moved mode** (old server, persists across restarts and re-installs): no apps, no backups, no edge sync, no cleanup; the dashboard domain and setup address show only "This server was moved to <IP> on <date>. You can delete it." Nothing on the old server writes to the bucket or Cloudflare any more, so the two servers never compete.
- Builds are redone on the new server, so it may have a different CPU architecture.
- Moved: everything in each app's `DATA_DIR`, not only its databases. Not moved: logs, metrics, deploy history (the builds of the current commits are new), local backup copies (the bucket copies stay usable). The Start page says so.

## 11. Dashboard and security

- Server-rendered pages; every action is a form POST + redirect (works without JS); `app.js` only does live logs (SSE), 5 s refresh of status sections, branch lists on Add app and confirmations. Pages: Apps (home), App, Add app, Deployment, Logs, Monitoring, Settings, Move, Account, Set up your account.
- **Passwords:** argon2id (64 MiB, t=3, p=1), ≥ 12 chars, at most 2 hashes at a time; unknown emails hashed against a dummy. Email and password changes need the current password; a password change signs out other sessions; a forgotten password → re-run the installer.
- **Sessions:** 32-byte token in `__Host-goboat` (`Secure; HttpOnly; SameSite=Strict; Path=/`), only its SHA-256 stored; 7 days idle, 30 days total; *sign out all others*.
- **Rate limit** (in memory, reset by a restart): 5 failures per client IP per 15 min → 429, even for the right password; above 50 failures overall, one attempt per 2 s. Failed current-password checks, API tokens and move tokens count too.
- **CSRF:** every POST needs `Origin` (or `Referer`) = `https://<Host>` and, when signed in, the session's token. Bodies ≤ 256 KB.
- **Headers:** strict CSP (`default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`), `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: same-origin`, HSTS, `no-store`. Flash messages use a 60 s cookie.
- Times are stored and scheduled in UTC and shown in the server's timezone with its name.
- **Status API** ([api.md](api.md)): `GET /api/v1/status`, `/apps`, `/apps/{name}`, JSON only. One token `gbt_` + 32 random bytes, created/rotated on the Account page with the current password, shown once, only its SHA-256 stored. `Authorization: Bearer` only; refused on setup connections. It never exposes env values or secrets.

## 12. Budget (to be measured on a 2 vCPU / 2 GB VPS)

| | Target |
|---|---|
| Idle RSS of goboat | < 30 MB (soft heap limit 12 MB, `GOGC=50`, memory returned every 2 min) |
| Idle CPU | < 1 % |
| Proxy overhead | < 1 ms p50 |
| goboat binary | < 30 MB |
| Rebuild of a small app after a one-line change (warm caches) | < 20 s (measured: 1.2 s build, 2.8 s deploy) |
| First build of a small app (cold caches) | < 2 min (measured: 63 s with SQLite) |
| Deploy downtime (switch only) | < 1 s plus the app's start |
| Server move downtime (small databases) | < 1 min |
| Reboot to serving | seconds |

## 13. Changing goboat

- `make lint` (gofmt, vet, staticcheck) and `make test` (`-race`) run in CI on every PR with amd64/arm64 builds. Unit tests cover the risky pure parts: secrets, store and migrations, the one-time password, `goboat.toml`, source archive unpacking, backup archives and change detection, the cleanup queue and reconciliation decisions, the move bundle and its encryption, app names, the health check, the setup address, the GitHub client, page rendering and the status API.
- There is **no automated end-to-end suite**. Anything touching processes, cgroups, builds, deploys, backups, moves or the edge must be tried on a real Ubuntu VPS with a real Cloudflare zone: tag a pre-release (`vX.Y.Z-rc1`), install it with `GOBOAT_VERSION=vX.Y.Z-rc1` in front of `bash`, and go through the affected flows. Useful checks: install/update (toolchain, swap), setup address closes, build + deploy + rollback + a broken build + a build that runs out of memory, restart/stop leaves no processes, backup (changed and unchanged) + restore, app delete and recreate with the same domain leaves exactly one Origin CA cert and one AOP cert in the zone, a move between two servers, direct IP access refused, no 520/521 through Cloudflare.
- Release: push a `vX.Y.Z` tag; the release workflow lints, tests, builds both binaries, writes `checksums.txt` and publishes them with `install.sh`. Tags with `-` are pre-releases.
- A schema change is a new numbered migration. An older binary then refuses the database.

## 14. Decisions

| # | Decision | Reason |
|---|---|---|
| D1 | Go for goboat | stdlib TLS/proxy/HTTP, pure-Go SQLite and S3 |
| D2 | No containers: users + cgroups | trusted single-owner apps; light |
| D3 | Stop-then-start deploys | safe with one SQLite writer; sub-second downtime |
| D4 | Cloudflare Origin CA, no ACME | all domains on Cloudflare; 15-year certs; no port 80 |
| D5 | Zone AOP with own CA + IP filter | the IP filter alone lets any Cloudflare customer in |
| D6 | Server-rendered HTML, tiny JS, no frontend build | strict CSP, nothing to vendor |
| D7 | Deploy only on click | the user decides when an app changes; fewer moving parts |
| D8 | Kill cgroups by PID, not `cgroup.kill` | `cgroup.kill` made later `CLONE_INTO_CGROUP` children die on Ubuntu kernels |
| D9 | No `memory.high`, no swap for apps | clean OOM + restart instead of a stalled app |
| D10 | systemd unit embedded, installed by `setup-host` | one copy; the binary can repair it |
| D11 | First sign-in with an installer-printed one-time password on a self-signed setup address that closes | no first-come public claim page; admin password never used outside Cloudflare |
| D12 | Update = re-run the installer | one command for install, update, recovery |
| D13 | App name = repository name; `goboat.toml` at the root | predictable names and backup folders |
| D14 | Enforce AOP per zone only once rolled out; persist it | never break a site during setup; never fail open after a restart |
| D15 | `Host` must equal SNI (421) | stops cross-zone requests bypassing a zone's AOP |
| D16 | Edge sync best effort per host | one broken domain doesn't block the others |
| D17 | Verify a backup fully before stopping the app | a bad backup never costs uptime or data |
| D18 | Deleting an app keeps its data by default | deletion is one click; data loss shouldn't be |
| D19 | `GOMEMLIMIT` = 90 % of the memory limit unless set | Go ignores cgroup limits; the GC should work before the kernel kills |
| D20 | **Build on the server** from a GitHub source archive at a commit | the user only connects a repo; no release workflow to write or maintain. Costs RAM (hence 2 GB + swap) and a toolchain on the server |
| D21 | **Domain required** at creation; can be changed, never cleared | an app without a domain can't be reached, and there would be nothing to fix it with |
| D22 | Limit changes apply on restart; "restart needed" + **Restart** shown | predictable; no surprise restarts |
| D23 | **Limits only in the dashboard**, from fixed choices; `goboat.toml` rejects limit keys | one place, and what the form shows is what applies |
| D24 | **Track every Cloudflare object until deleted**; reconcile served zones against Cloudflare's lists; goboat owns zone-level AOP in its zones | failed or interrupted deletes, crashes and reinstalls had left piles of certs |
| D25 | **Backups every 10 min**, skipped when unchanged, 48 h retention | an RPO of 3 h lost real data; skipping keeps cost and bucket clutter down |
| D26 | Change detection by file size + mtime of the DB and its WAL | cheap, no SQLite internals; any write changes one of them |
| D27 | **Server move by direct transfer** from the old server, encrypted with a key derived from the move code; old server fenced in moved mode | one command + one click for the user; certs and AOP reused, so no Cloudflare waits; never two servers writing to the bucket |
| D28 | Source archives instead of `git clone` | no git credentials or history on the server; reuses the safe unpacker; same token as the API |
| D29 | Go toolchain owned by goboat in `/usr/local/lib/goboat/go`, `GOTOOLCHAIN=auto` | never clashes with a system Go; apps can require newer Go via `go.mod` |
| D30 | A read-only status API with one rotatable bearer token | agents and scripts can answer "is it up?" without a browser; read-only keeps the dashboard the only way to change things |
