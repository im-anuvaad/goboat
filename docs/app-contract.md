# goboat App Contract

What a Go app must do to be hosted by goboat. goboat downloads the app's source at a commit, builds it on the server and runs it behind Cloudflare. Version: **contract 1**.

## 1. Checklist

- [ ] The repository has `go.mod` and a `goboat.toml` at its **root** (§2).
- [ ] The repository name is the app name: lowercase letters, digits and `-`, 2–24 characters, starting with a letter (`My_Blog` becomes `my-blog`).
- [ ] It builds with `CGO_ENABLED=0`: use a pure-Go SQLite driver such as `modernc.org/sqlite`, not `mattn/go-sqlite3` (§3).
- [ ] Every dependency is public, or vendored with `go mod vendor` (§3).
- [ ] It listens on `HOST:PORT` and answers `health_path` with 2xx or 3xx within 30 seconds of starting (§4, §8).
- [ ] It keeps its data in SQLite files under `$DATA_DIR` (§6, §7).
- [ ] It exits cleanly within 10 seconds of `SIGTERM` (§8).

---

## 2. `goboat.toml`

The file tells goboat **what to build and how to check it is up**. It is required; a commit without it is not deployed.

```toml
contract    = 1
package     = "./cmd/server"   # optional, default "."
args        = ["-mode", "prod"] # optional, default none
health_path = "/healthz"        # optional, default "/"
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `contract` | yes | — | Must be `1`. |
| `package` | no | `"."` | The `main` package to build, relative to the repository root, starting with `.` (e.g. `"."`, `"./cmd/web"`). |
| `args` | no | `[]` | Command-line arguments for the binary. They are passed as they are; there is no shell, so `$VAR`, pipes and `&&` don't work. Use env vars (§5) for configuration. |
| `health_path` | no | `"/"` | A path starting with `/` that must return 2xx or 3xx when the app is ready. |

- Unknown keys are rejected with an error naming them, so typos fail the deploy instead of being ignored.
- **Resource limits, the domain, the branch and env vars are not in this file.** They are set in the dashboard (the Add app form and the app's Settings), and what the dashboard shows is what applies. A `memory`, `cpu` or similar key is rejected with a message saying so.
- The file is read at every deploy, from the commit being deployed.

---

## 3. How goboat builds your app

When you press **Deploy** on a commit, goboat:

1. Downloads the source of that commit from GitHub (the repository archive, with the stored token; no `git` history, no submodules).
2. Reads and checks `goboat.toml`.
3. Runs, in the repository root:

   ```
   CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o <build dir>/app <package>
   ```

4. Checks that the result is a Linux executable for the server's CPU and keeps it next to the source tree (§7).

Details you can rely on:

- **Go version.** goboat installs the latest stable Go. If your `go.mod` asks for a newer one (its `go` or `toolchain` line), that version is downloaded automatically (`GOTOOLCHAIN=auto`) and verified against the Go checksum database. To pin an exact version, add a `toolchain go1.x.y` line to `go.mod`.
- **Dependencies** come from `proxy.golang.org` and are verified against `go.sum`, which must be committed. Private modules can't be downloaded: goboat's GitHub token is used only for the app's own repository. Vendor them (`go mod vendor`, commit `vendor/`); Go then uses `vendor/` automatically.
- **No cgo.** There is no C compiler on the server. Use pure-Go packages.
- **Nothing of yours runs during the build.** `go build` doesn't execute code from your module or its dependencies; `go generate` and tests are **not** run. Commit generated files, and keep running tests in your own CI if you have any.
- **Your env vars are not passed to the build**, so secrets never reach the build. The build can't see `$DATA_DIR`.
- **Builds share caches** (module cache and build cache), so a rebuild after a small change takes seconds. The first build of an app downloads its modules.
- **No version control info** is stamped into the binary (`-buildvcs=false`: the archive has no `.git`). Read the commit from `GOBOAT_COMMIT` (§5) instead of `debug.ReadBuildInfo`.
- **Limits per build:** 1.5 GB of memory (1.4 GB on a 2 GB server) plus the server's swap, 15 minutes, lower CPU priority than the running apps. One build runs at a time on the server; the app keeps serving its current version while its new one builds. A build that needs more fails with "the build ran out of memory"; that takes a lot (on a 2 GB server, a minimal app with `modernc.org/sqlite` peaks at 1.2 GB on its first build and about 80 MB on later ones).
- **Source size:** the downloaded archive may be at most 100 MB, the unpacked tree 500 MB.

Embedding templates and static files with `//go:embed` is the simplest way to ship them. Files read at runtime with relative paths also work, because the app runs in its source tree (§7).

---

## 4. Branches and deploys

- When you add an app you pick its **branch** (the repository's default branch is preselected). You can change it later in the app's Settings; it applies on the next deploy.
- The app page lists the 10 newest commits of that branch (newest preselected). **Deploy** builds and deploys the one you pick. goboat never deploys on push.
- The last 3 builds of an app are kept; **Rollback** switches to one of them without rebuilding.
- `goboat.toml` is read from the commit being deployed, so a change to `package` or `health_path` takes effect with that deploy.

---

## 5. Environment variables goboat provides

| Variable | Example | Meaning |
|---|---|---|
| `PORT` | `20001` | The port to listen on. It stays the same for an app across deploys. |
| `HOST` | `127.0.0.1` | The address to bind to. Always bind here, never to `0.0.0.0`. |
| `DATA_DIR` | `/var/lib/goboat/apps/blog/data` | Persistent and survives every deploy. Its SQLite files are backed up; nothing else in it is (§6). |
| `TMPDIR` | `/var/lib/goboat/apps/blog/tmp` | Scratch space, emptied on every deploy. |
| `GOBOAT_APP` | `blog` | The app's name (from the repository name). |
| `GOBOAT_DOMAIN` | `blog.example.com` | The public domain. |
| `GOBOAT_BRANCH` | `main` | The branch the running build came from. |
| `GOBOAT_COMMIT` | `3f9c2a1e…` | The full commit SHA of the running build. |
| `GOBOAT_CONTRACT` | `1` | The contract version. |
| `GOMEMLIMIT` | `241591910` | 90% of the app's memory limit, unless you set it yourself (§9). |

goboat also sets `PATH` and `LANG` and adds your own env vars from the dashboard; nothing else is inherited (no `HOME`). Names starting with `GOBOAT_`, and the names in the table above except `GOMEMLIMIT`, are reserved. Changing env vars needs a **Restart**, not a new deploy.

---

## 6. HTTP rules

- Serve **plain HTTP/1.1**. goboat handles TLS in front of your app.
- Headers you can rely on:
  - `Host`: your domain.
  - `X-Forwarded-Proto: https` and `X-Forwarded-Host` (your domain).
  - `X-Forwarded-For` and `X-Real-IP`: the **real visitor IP**, taken from Cloudflare's `CF-Connecting-IP`. Whatever the visitor sent in these headers is replaced, so they are safe to trust.
  - `CF-Connecting-IP` and `CF-IPCountry`, passed through from Cloudflare.
- WebSockets, long-polling and streamed responses (SSE) work; responses are flushed to the visitor immediately.
- Cloudflare limits request bodies to 100 MB on the free plan.
- During the switch to a new build, visitors see a short goboat "deploying" 503 page (well under a second, plus your app's start time). The build itself happens before that and causes no downtime.
- While your app is stopped or crashed, visitors get a goboat 503 page; if it stops answering unexpectedly, a 502.

---

## 7. Filesystem rules

| Path | Access | Survives deploy? | Backed up? |
|---|---|---|---|
| Build dir: your source tree at the commit plus the built binary (current working dir) | read-only | replaced | no |
| `$DATA_DIR` | read/write | ✅ yes | ✅ SQLite files |
| `$TMPDIR` | read/write | ❌ emptied | no |
| Other apps' folders, goboat's files, the build caches | no access | — | — |

- **Only SQLite databases in `$DATA_DIR` are backed up** (sub-folders included). goboat finds them by their file header, whatever their name.
- **Everything else in `$DATA_DIR` is yours.** Uploads, caches and generated files are kept across deploys, restarts, rollbacks and server moves, but they are not copied to the bucket or restored from it, and they are lost if the server is lost. An app that needs such files protected must do it itself (an object store, or storing them in SQLite).
- Don't create folders named `.pre-restore-*` in `$DATA_DIR`: goboat uses them to keep the previous databases after a restore.

---

## 8. SQLite rules

1. Open databases **only** from `$DATA_DIR`, for example `$DATA_DIR/app.db`.
2. Use these settings on every connection:
   ```sql
   PRAGMA journal_mode = WAL;
   PRAGMA busy_timeout = 5000;
   PRAGMA foreign_keys = ON;
   PRAGMA synchronous = NORMAL;
   ```
   Backups read the database while the app runs (every 10 minutes when it changed). `busy_timeout` makes sure a backup never causes a "database is locked" error in your app.
3. **Run migrations at startup**, before you start listening. goboat takes a backup right before every deploy, so a bad migration can be undone with **Restore**.
4. Migrations should only move forward, and ideally stay compatible with the previous build. Rolling back the code does **not** roll back the database.

---

## 9. Lifecycle and limits

```
start ──► migrations ──► listen on HOST:PORT ──► health_path returns 2xx/3xx ──► receives traffic
                                                                                     │
SIGTERM (deploy / restart / stop / server move) ──► finish requests, close DB ──► exit 0
                                                └─► after 10 s: SIGKILL
```

- goboat starts your binary directly and supervises it. If it exits, goboat restarts it with backoff (1 s, 2 s, 4 s … up to 60 s). After 5 exits in 5 minutes the app is marked **crashed** and left stopped until you start it.
- Don't daemonize, fork into the background or write PID files. Log to stdout or stderr (Go's `log` and `log/slog` do); lines longer than 16 KB are split.
- When the main process exits, goboat kills anything else still running in the app's cgroup.

Limits are chosen in the dashboard from a list when you add the app, and can be changed in the app's Settings (they apply on the next restart):

| Limit | Choices | Default |
|---|---|---|
| Memory | 128 MB, 256 MB, 512 MB, 1 GB | 256 MB |
| CPU | 0.5, 1 or 2 cores | 1 core |
| Processes/threads | fixed | 256 |
| Open files | fixed | 4096 |

- Apps **never swap**. An app that goes above its memory limit is killed by the kernel and restarted. Because the Go runtime doesn't read cgroup limits, goboat sets `GOMEMLIMIT` to 90% of the limit so the garbage collector works harder before that happens; set `GOMEMLIMIT` yourself in the env vars to override it.
- `GOMAXPROCS` follows the CPU limit automatically (Go 1.25 and newer).

---

## 10. Minimal app

```go
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // pure Go, no cgo
)

func main() {
	dsn := "file:" + filepath.Join(os.Getenv("DATA_DIR"), "app.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err == nil {
		err = db.Ping() // opens the file; fails fast on a bad DATA_DIR
	}
	if err != nil {
		log.Fatal(err)
	}
	// run migrations here, before listening

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: os.Getenv("HOST") + ":" + os.Getenv("PORT"), Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 8*time.Second) // killed after 10 s
		defer cancel()
		srv.Shutdown(sctx)
		close(done)
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-done
	db.Close()
}
```

Repository layout:

```
myapp/
├── goboat.toml      # contract = 1, health_path = "/healthz"
├── go.mod
├── go.sum
├── main.go
├── templates/       # embedded with //go:embed, or read at runtime by relative path
└── static/
```
