# goboat Implementation Plan

How the code gets from the imported codebase to what [architecture.md](architecture.md) describes. Each phase ends with `make lint test` green, a check on a real VPS where it applies, and its own PR. This file is deleted when the last phase is merged.

## Test environment

| | Use |
|---|---|
| VPS 1 (2 vCPU, 1.9 GB, Ubuntu 24.04) | main goboat install |
| VPS 2 (same) | MinIO as the S3-compatible bucket; the target of the server move in phase 6 |
| Zone `doot.run` | the only Cloudflare zone used; every action is allowed there |
| Test repos `im-anuvaad/goboat-test-*` | `hello` (minimal app from the contract), `heavy` (many dependencies, to test build memory and swap), `broken` (fails to build / fails its health check) |

## Phase 1: import, rename, Go only

Behaviour stays as it is (release-based deploys still work), so the rename can be checked on its own.

- Import the code without its git history: module `github.com/im-anuvaad/goboat`, `cmd/goboat`, `goboat.service`, `/var/lib/goboat`, `/etc/goboat`, `goboat.db`, `goboat.toml`, users `goboat-<app>`, env prefix `GOBOAT_`, header `X-Goboat-Page`, cookies `__Host-goboat*`, token prefix `gbt_`, metrics scope `_goboat`, logo and texts.
- Remove the docs site (`site/`) and the dashboard screenshot; the Markdown docs in `docs/` are the documentation.
- Remove everything for other languages: Zig/C/musl texts, the `zig_version` column, test fixtures.
- Replace the migrations with one fresh schema without unused columns. No data to migrate: there is no installed base.
- Done when: `grep -ri` for the old name finds nothing (files, paths, binary strings), lint and tests pass, a pre-release installs on VPS 1 and serves the dashboard on `goboat.doot.run`.

## Phase 2: build on the server (done)

- `install.sh`: Go toolchain into `/usr/local/lib/goboat/go` (checksum from go.dev), 2 GB swap file, `vm.swappiness`, RAM/disk checks, `goboatbuild` user.
- `github`: branches, commits of a branch, source archive download.
- `source`: safe unpack of the archive (adapted from the release unpacker), size and file-count limits.
- `manifest`: `goboat.toml` contract 1 (`package`, `args`, `health_path`), limit keys rejected with a pointer to the dashboard.
- `builder`: build cgroup (memory 1.5 GB + swap, low CPU/IO weight, 15 min), env, cache trimming, Go version recorded, OOM reported.
- `deployer`: pipeline of architecture §7.2, `builds` table, keep 3, rollback to a build, crash recovery.
- Dashboard: branch on Add app and Settings, commit list and **Deploy**, build log streamed on the deployment page, `GOBOAT_BRANCH`/`GOBOAT_COMMIT`.
- Remove the release download path.
- Done when, on VPS 1: `hello` builds and deploys from a branch, a second deploy after a one-line change uses warm caches, rollback works without a rebuild, `broken` fails without touching the running build, `heavy` builds using swap while `hello` keeps serving, a build over the limit fails with an out-of-memory message.
- Result: all of the above checked on VPS 1 with `v0.0.2-rc1` (figures in architecture §7.3). Also checked: an update by re-running the installer migrates the database and installs Go and swap; restarting goboat during a build marks it failed and leaves no build processes, cgroups or workspaces; an app cannot read another app's builds, and the build user cannot read any app's.
- Moved to phase 3: the build's CPU/memory/swap as a chart (the deploy log has its peaks), and the "swap in use outside builds" warning.

## Phase 3: app form and limits (quirks A, B, C)

- Domain required on Add app ("Enter the domain the app will be served on"), cannot be cleared in Settings; DB column `NOT NULL` with a unique index.
- Memory and CPU drop-downs on Add app and Settings (architecture §6.1); total memory against RAM shown, warning above 75 %.
- Settings save shows "restart needed" with a **Restart** button.
- Monitoring: a "swap in use outside builds" warning, and the last build's CPU, memory and swap on the deployment page.
- Contract docs and form texts say limits come only from the dashboard.
- Done when: creating an app without a domain is refused with that message; limits picked in the form are the values in the app's cgroup files after a restart.

## Phase 4: Cloudflare cleanup (quirk D)

- `edge_hosts` with DNS record ids, `cf_cleanup` queue, `RemoveHost` as one transaction plus background sync, context-aware locking.
- Cleanup worked first in every sync; reconciliation of Origin CA and AOP certs per served zone; zone release when its last host goes; AOP cert id saved right after upload.
- Settings: pending cleanup with errors, unused Origin CA certs with **Revoke**; home warning after 24 h of failures.
- Done when, on `doot.run`: create → delete → recreate an app with the same domain 3 times leaves exactly one Origin CA cert for it and one AOP cert in the zone; a delete while the token is temporarily invalid is cleaned up after the token is fixed; killing goboat during an AOP upload leaves no extra cert after the next sync; the leftover certs from earlier tests in `doot.run` are cleaned (AOP automatically, Origin CA via **Revoke** or reconciliation).

## Phase 5: backups every 10 minutes (quirk E)

- Interval 10 min, fingerprint per database stored with the backup, skip when unchanged.
- Hourly retention, local copy thinning, overrun detection and warning, app page shows 20 + **Show all**.
- Done when, against MinIO: an idle app makes no new backups after its first; a write makes one at the next slot; objects older than 48 h are deleted except the newest; an outage of the bucket loses nothing and catches up.

## Phase 6: server move

- `move` package: code, HKDF-derived token and key, encrypted bundle and file transfer, `/move/v1/*` endpoints, configuration freeze, incoming and moved modes.
- `install.sh move <url> <code>` and `goboat move-in`.
- Old dashboard: Start, progress, Cancel, **Switch now**, moved page.
- Done when: a VPS 1 install with 2 apps (one with uploads in `DATA_DIR`) moves to VPS 2; measured downtime per app is recorded in architecture §12; after the move the zone has the same Origin CA and AOP certs as before; VPS 1 stays in moved mode after a reboot; a move cancelled at step 4 and one that fails at step 7 both leave VPS 1 serving as before.

## Phase 7: release

- Measure the budget table (architecture §12) on VPS 1 and fill it in, take a new dashboard screenshot.
- Tag `v0.1.0`.
