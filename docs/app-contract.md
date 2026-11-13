# goboat App Contract

Everything a **Go + SQLite web app** must do to be built and run by goboat. It is written for the people and the AI agents who write such apps: give this file to an agent together with "write a Go + SQLite app whose deployment target is goboat", and it has what it needs.

Contract version: **1**. How to run goboat itself is in [deployment.md](deployment.md).

**Contents:**
1. [The short version](#1-the-short-version)
2. [How goboat runs your app](#2-how-goboat-runs-your-app)
3. [Repository and goboat.toml](#3-repository-and-goboattoml)
4. [The build](#4-the-build)
5. [Environment variables](#5-environment-variables)
6. [HTTP](#6-http)
7. [Process lifecycle](#7-process-lifecycle)
8. [Filesystem](#8-filesystem)
9. [SQLite](#9-sqlite)
10. [Migrations, deploys and rollbacks](#10-migrations-deploys-and-rollbacks)
11. [Backups and restores](#11-backups-and-restores)
12. [Resource limits](#12-resource-limits)
13. [Server moves](#13-server-moves)
14. [Security](#14-security)
15. [Complete example](#15-complete-example)
16. [Checklist before the first deploy](#16-checklist-before-the-first-deploy)

---

## 1. The short version

An app runs on goboat when **all** of these hold. Each rule links to its section.

| # | Rule | § |
|---|---|---|
| 1 | The repository root has `go.mod`, `go.sum` and a `goboat.toml` with `contract = 1`. | 3 |
| 2 | One `main` package; the app is **one process** serving HTTP. | 3, 7 |
| 3 | It builds with plain `CGO_ENABLED=0 go build`: pure-Go dependencies only (`modernc.org/sqlite`, not `mattn/go-sqlite3`), all of them public or vendored, generated code committed. | 4 |
| 4 | It listens on `$HOST:$PORT` (plain HTTP) and answers `health_path` with 2xx/3xx within **30 seconds** of starting. | 6, 7 |
| 5 | Its persistent data is in **SQLite databases under `$DATA_DIR`**. Everything else on disk is read-only or temporary. | 8, 9 |
| 6 | It runs its **migrations at startup**, before listening; they are forward-only, idempotent and leave the schema usable by the **previous** build. | 10 |
| 7 | It stops cleanly within **10 seconds** of `SIGTERM`. | 7 |
| 8 | It fits its memory limit (128 MB – 1 GB, chosen in the dashboard) and logs to stdout/stderr. | 12, 7 |
| 9 | Configuration and secrets come from environment variables set in the dashboard, never from files in the repository. | 5, 14 |

The repository name becomes the app name (`My_Blog` → `my-blog`): lowercase letters, digits and `-`, 2–24 characters, starting with a letter.

---

## 2. How goboat runs your app

goboat is a single-server PaaS behind Cloudflare. The owner adds an app in the dashboard (repository, branch, domain, memory, CPU) and presses **Deploy** on a commit. goboat never deploys on its own, never on push.

```
Deploy pressed on commit X of the app's branch
  │
  ├─ download the source of X from GitHub, read goboat.toml       ┐
  ├─ go build (on the server, in a sandbox)                       │ the running version
  ├─ check the binary                                             ┘ keeps serving
  │
  ├─ visitors get a short 503 "deploying" page                    ┐
  ├─ stop the running version (SIGTERM, then SIGKILL after 10 s)  │ downtime:
  ├─ back up every SQLite database in DATA_DIR                    │ ~0.2 s plus your
  ├─ empty TMPDIR, start the new version                          │ app's startup
  └─ wait for health_path → traffic goes to the new version       ┘
       └─ not healthy within 30 s → stop it, start the previous build again
```

- Only **one version runs at a time**: the old one is stopped before the new one starts. Never two processes on the same database.
- The last **3 builds** are kept on the server. **Rollback** switches to one of them without rebuilding.
- If a build or start fails, the previous build keeps or resumes serving. **The database is not rolled back** (§10).

---

## 3. Repository and goboat.toml

```
myapp/
├── goboat.toml      # required, at the root
├── go.mod
├── go.sum           # committed
├── main.go          # or cmd/web/main.go, see "package"
├── migrations/      # embedded SQL files (§10)
├── templates/       # embedded or read at runtime by relative path
└── static/
```

`goboat.toml` says **what to build and how to check it is up**, nothing else:

```toml
contract    = 1
package     = "./cmd/web"        # optional, default "."
args        = ["-mode", "prod"]  # optional, default none
health_path = "/healthz"         # optional, default "/"
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `contract` | yes | — | Must be `1`. |
| `package` | no | `"."` | The `main` package to build, relative to the repository root, starting with `.`. One package, no `...` patterns, no `..`. |
| `args` | no | `[]` | Command-line arguments, passed as they are (no shell: no `$VAR`, pipes or `&&`). |
| `health_path` | no | `"/"` | A path that returns 2xx/3xx when the app is ready (§7). |

- Unknown keys fail the deploy with their names, so typos don't pass silently.
- **Memory, CPU, domain, branch and env vars are not in this file.** They are set only in the dashboard; keys like `memory`, `cpu`, `domain`, `branch` or `env` are rejected with a message saying so.
- The file is read from the commit being deployed, so a change to it takes effect with that deploy.

---

## 4. The build

goboat runs, in the repository root of the downloaded commit:

```
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o <build dir>/app <package>
```

| | |
|---|---|
| **Source** | GitHub's archive of the commit: no `.git`, no submodules, at most 100 MB (500 MB unpacked, 50 000 files). |
| **Go version** | The latest stable Go, installed by goboat. A newer `go` or `toolchain` line in `go.mod` is downloaded automatically (`GOTOOLCHAIN=auto`). Pin an exact version with `toolchain go1.x.y`. |
| **Dependencies** | From `proxy.golang.org`, checked against `go.sum`. **Private modules can't be fetched**: vendor them (`go mod vendor`, commit `vendor/`). |
| **cgo** | Not available: no C compiler. Use pure-Go packages. |
| **Env during the build** | Not your app's env vars. Secrets never reach the build. |
| **Caches** | Module and build caches are shared, so a rebuild after a small change takes seconds; a first build with `modernc.org/sqlite` takes about a minute. |
| **Limits** | 1.5 GB memory (1.4 GB on a 2 GB server) plus swap, 15 minutes, lower CPU priority than the running apps. Too much memory fails with "the build ran out of memory". |
| **Version info** | None stamped (`-buildvcs=false`). Read the commit from `GOBOAT_COMMIT` (§5). |

**Nothing of yours runs during the build**: no `go generate`, no tests, no scripts, no npm. Generated code must be committed:

| Tool | What to commit |
|---|---|
| [templ](https://templ.guide) | The `*_templ.go` files from `templ generate`. The templ runtime is pure Go. |
| sqlc, stringer, other generators | Their generated `.go` files. |
| Tailwind CSS, esbuild, any frontend build | The built CSS/JS, e.g. `static/app.css`. |
| htmx and other JS libraries | The file itself, e.g. `static/htmx.min.js` (or load it from a CDN). |

A stale generated file builds without complaint, so regenerate before committing (a `make gen` target, or a CI job that fails when `git diff --exit-code` shows changes after generating).

Ship templates and static files with `//go:embed`. Files read at runtime by **relative path** also work: the working directory is the source tree of the commit (§8).

---

## 5. Environment variables

The process starts with **only** these, plus the env vars set on the app's page in the dashboard. Nothing else is inherited: no `HOME`, no `USER`.

| Variable | Example | Meaning |
|---|---|---|
| `PORT` | `20001` | Port to listen on. Stays the same for the app. |
| `HOST` | `127.0.0.1` | Address to bind. Always bind here, never `0.0.0.0`. |
| `DATA_DIR` | `/var/lib/goboat/apps/notes/data` | Persistent directory for your SQLite files (§8). |
| `TMPDIR` | `/var/lib/goboat/apps/notes/tmp` | Scratch space, emptied on every deploy. |
| `GOBOAT_APP` | `notes` | App name. |
| `GOBOAT_DOMAIN` | `notes.example.com` | Public domain (use it to build absolute URLs). |
| `GOBOAT_BRANCH` | `main` | Branch of the running build. |
| `GOBOAT_COMMIT` | `3f9c2a1e…` | Full commit SHA of the running build. |
| `GOBOAT_CONTRACT` | `1` | Contract version. |
| `GOMEMLIMIT` | `241591905` | 90 % of the memory limit (§12), unless you set it yourself. |
| `PATH`, `LANG` | `/usr/local/bin:/usr/bin:/bin`, `C.UTF-8` | |

- Names starting with `GOBOAT_`, and `PORT`, `HOST`, `DATA_DIR`, `TMPDIR`, are reserved.
- Env vars are stored encrypted and never shown again in the dashboard. A change applies on the next **Restart** or deploy, not to the running process.
- Fail at startup with a clear message when a required variable is missing: the deploy then fails and the previous build keeps serving.

---

## 6. HTTP

- Serve **plain HTTP/1.1** on `$HOST:$PORT`. Cloudflare and goboat handle TLS, HTTP/2 and HTTP/3 in front.
- Request headers you can rely on (goboat replaces whatever the visitor sent):

| Header | Value |
|---|---|
| `Host` | Your domain. |
| `X-Forwarded-For`, `X-Real-IP` | The visitor's real IP (from Cloudflare's `CF-Connecting-IP`). Safe to trust. |
| `X-Forwarded-Proto` | `https` (always; plain HTTP never reaches you). |
| `X-Forwarded-Host` | Your domain. |
| `CF-Connecting-IP`, `CF-IPCountry` | Passed through from Cloudflare. |

- Set `Secure` on cookies: every visitor is on HTTPS.
- WebSockets, Server-Sent Events and long-polling work; responses are flushed immediately. Cloudflare closes a connection that sends nothing for 100 seconds, so send keep-alive messages.
- Request bodies are limited to 100 MB by Cloudflare (free plan).
- Visitors see goboat's own 503 page while the app is deploying, starting, stopped or crashed, and a 502 when it doesn't answer. Pages with an `X-Goboat-Page` header come from goboat, not your app.
- **Health check:** at start, goboat connects to `127.0.0.1:$PORT` and sends `GET <health_path>` with `Host: <your domain>` and `User-Agent: goboat-health` every 200 ms until it gets 2xx/3xx, for at most 30 seconds. Only after that does traffic arrive. It is checked **only at start**; afterwards goboat watches the process (§7). Make it cheap and dependent on what the app really needs (e.g. `db.Ping`), not on outside services.

---

## 7. Process lifecycle

```
start ──► read env ──► open DB ──► migrate ──► listen ──► health_path 2xx ──► traffic
                                                                                 │
SIGTERM (deploy, restart, stop, server move) ──► stop accepting, finish requests, close DB ──► exit
                                             └─► still running after 10 s: SIGKILL
```

- goboat runs your binary directly as an unprivileged user, in its own cgroup, with the source tree as working directory.
- **One process.** Don't daemonize, fork into the background or write PID files. Background work (jobs, schedules, queues) runs in goroutines of the same process; goboat has no cron. Use a `time.Ticker` and keep job state in SQLite.
- **Exits are restarted** with backoff (1 s, 2 s, 4 s … up to 60 s). After **5 exits in 5 minutes** the app is marked *crashed* and left stopped until the owner presses Start. A run of 60 s resets the backoff. Exit with an error and a log line on fatal problems instead of hanging.
- When the main process exits, everything else in its cgroup is killed.
- **Logs:** write to stdout or stderr (Go's `log` and `log/slog` do). goboat timestamps each line, keeps 3 × 10 MB per app and streams them live in the dashboard. Lines over 16 KB are split. Don't write log files.
- **Shutdown:** handle `SIGTERM` with `http.Server.Shutdown` and a deadline under 10 s (8 s is a good value), then close the database.

---

## 8. Filesystem

| Path | Access | Survives a deploy | Backed up | Moved with the server |
|---|---|---|---|---|
| Working directory: the source tree of the commit, plus the binary | read-only | replaced | no | rebuilt |
| `$DATA_DIR` | read/write | ✅ | SQLite files only | ✅ everything |
| `$TMPDIR` | read/write | ❌ emptied | no | no |
| Anything else (other apps, goboat, `/home`, `/etc` …) | none | | | |

- **Persistent data belongs in SQLite** under `$DATA_DIR` (sub-folders allowed). goboat finds databases by their file header, whatever their name.
- Other files in `$DATA_DIR` (uploads, caches) survive deploys, restarts, rollbacks and server moves, but **are not backed up**. Keep data you can't lose in SQLite (small blobs fit) or in an object store you manage.
- Don't create folders named `.pre-restore-*` in `$DATA_DIR`: goboat uses them (§11).
- Don't store absolute paths or the server's hostname or IP: they change when the installation moves (§13).

---

## 9. SQLite

- Driver: **`modernc.org/sqlite`** (pure Go; `database/sql` driver name `"sqlite"`).
- Database files only under `$DATA_DIR`, e.g. `$DATA_DIR/app.db`.
- Open every connection with:

```go
dsn := "file:" + filepath.Join(os.Getenv("DATA_DIR"), "app.db") +
	"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
db, err := sql.Open("sqlite", dsn)
```

| Setting | Why |
|---|---|
| `journal_mode(WAL)` | Readers don't block the writer; backups read while you write. |
| `busy_timeout(5000)` | Waits instead of failing with "database is locked" while a backup or another connection holds a lock. |
| `foreign_keys(1)` | SQLite leaves foreign keys off by default. |
| `synchronous(NORMAL)` | Safe with WAL, much faster than FULL. |
| `_txlock=immediate` | Write transactions take the write lock at `BEGIN`, which avoids deadlocks between two transactions that both start reading and then write. |

- Keep transactions short. goboat backs up with `VACUUM INTO` while the app runs, every 10 minutes when something changed.
- Only one process uses the database, always yours (§2). Inside it, `database/sql` connection pooling with the settings above is fine.

---

## 10. Migrations, deploys and rollbacks

goboat doesn't know your schema. It runs your binary, and **your binary migrates the database at startup**. What makes this safe is knowing what goboat does around it.

### 10.1 What can happen to the database

| Event | Code that runs | Database it finds |
|---|---|---|
| **Deploy succeeds** | new build | migrated by the new build at start |
| **Deploy, new build fails its health check** (crash, slow start, bug) | the **previous build** is started again | already migrated by the new build, **not rolled back** |
| **Rollback** (dashboard) | an older build | migrated by the newer build, **not rolled back** |
| **Restore** of a backup (dashboard) | the current build, restarted | the backup's: possibly an **older schema** |
| **Crash loop** | the same build, again and again | partly migrated if a migration failed half-way outside a transaction |
| **Server move** | the same commit on another server | the same database, copied |

Two consequences follow, and every rule below comes from them:

- **The previous build must work on the new schema.** After a failed deploy or a rollback, older code runs on a database the newer code migrated.
- **The current build must migrate from any older schema.** After a restore, the database can be days old.

### 10.2 Rules

1. **Migrate at startup, before listening.** The health check only passes once the server listens, so traffic never reaches a half-migrated database.
2. **Forward-only.** goboat never runs down-migrations. Don't write any; undo a change with a new forward migration.
3. **Numbered and recorded.** Files `0001_init.sql`, `0002_….sql` applied in order, each recorded in a `schema_migrations` table. Apply only the ones not recorded.
4. **One transaction per migration**, together with its `schema_migrations` row. A failed migration then leaves nothing behind and is retried at the next start. (SQLite runs `CREATE`, `ALTER` and `DROP` inside transactions.)
5. **Ignore unknown versions.** Older code finds rows for migrations it doesn't have: that is normal after a rollback, not an error. Never refuse to start because the schema is newer.
6. **Expand, then contract.** Make each release's schema change compatible with the previous release's code:

| Change | Release N (expand) | Release N+1 or later (contract) |
|---|---|---|
| Add a column | `ADD COLUMN … DEFAULT …` (nullable or with a default); new code uses it | — |
| Add a table or index | create it | — |
| Rename a column | add the new column, write both, backfill | stop using the old one; drop it a release later |
| Drop a column or table | stop using it in code | drop it |
| Change a column's type or constraint | new column (or new table + copy), write both | switch reads, then drop the old one |
| Make a column `NOT NULL` | add with a default, backfill | rebuild the table (SQLite's 12-step `ALTER`) |

7. **Explicit column lists.** `SELECT a, b FROM t` and `INSERT INTO t (a, b) VALUES …`, never `SELECT *` or `INSERT INTO t VALUES (…)`, so older code keeps working when columns are added.
8. **Fast migrations.** Everything before listening counts towards the 30-second health check. Schema changes on SQLite are fast; **large data backfills** (millions of rows) belong in a goroutine after listening, in small batches, resumable after a restart.
9. **No destructive surprises.** A migration that deletes or rewrites data can't be undone by goboat's rollback: the owner must restore a backup (§10.3).

### 10.3 Undoing a bad deploy

- **Code bug, schema fine:** press **Rollback** to a previous build (seconds, no rebuild), then deploy a fix.
- **Bad migration (wrong data change):** goboat took a **backup right before the deploy** (kind `pre-deploy`). **Restore** it from the app's Backups list, then deploy fixed code. Everything written after that backup is lost, so prefer fixing forward when the data is mostly right.
- **The new build crashes on start:** goboat already went back to the previous build; check the deploy log and app log, fix, deploy again.

---

## 11. Backups and restores

| | |
|---|---|
| **What** | Every SQLite database under `$DATA_DIR` (found by its header; `-wal`, `-shm`, `-journal` skipped). Nothing else. |
| **When** | Every 10 minutes when a database changed, before every deploy and rollback, and on **Back up now**. |
| **How** | `VACUUM INTO` a copy while the app runs, then `PRAGMA quick_check`. Consistent, no downtime. |
| **Kept** | 48 hours in an S3-compatible bucket, the newest always; the last 2 also on the server. |

**Restore** (by the owner, from the dashboard): goboat verifies the backup completely first, then stops the app, moves the current database files to `$DATA_DIR/.pre-restore-<time>/` (the last 2 kept), puts the backup's files in place and starts the app. Your migrations then run on that older schema (§10.1).

---

## 12. Resource limits

| Limit | Choices (dashboard) | Default |
|---|---|---|
| Memory | 128 MB, 256 MB, 512 MB, 1 GB | 256 MB |
| CPU | 0.5, 1, 2 cores | 1 core |
| Processes and threads | fixed | 256 |
| Open files | fixed | 4096 |

- **No swap for apps.** Above the memory limit the kernel kills the process and goboat restarts it; the dashboard shows the out-of-memory kill.
- `GOMEMLIMIT` is set to 90 % of the limit so Go's garbage collector works harder before that happens. `GOMAXPROCS` follows the CPU limit (Go 1.25+).
- Stream large responses and uploads instead of reading them into memory; limit request body sizes with `http.MaxBytesReader`.

---

## 13. Server moves

The owner can move the whole installation to another server. For your app that is a stop on one server and a start on another, about two seconds apart:

- It receives `SIGTERM` as for a deploy. Every file in `$DATA_DIR` is copied (databases as consistent snapshots), and the **same commit** is built and started on the new server.
- `PORT`, `DATA_DIR` and the domain stay the same; the hostname, IP addresses and `TMPDIR` contents don't.
- No migration runs unless your startup runs one anyway.

---

## 14. Security

- Apps are **trusted** but separated: each runs as its own Linux user, can't read other apps' or goboat's files, and only reaches the internet the way any process on the server can.
- **Secrets** go in env vars in the dashboard. Never commit them, and never put them in `goboat.toml`.
- Only Cloudflare reaches the server, and only your Cloudflare account (origin pulls). The client IP headers are safe; everything else in a request is visitor input.
- Your app handles its own sign-in, sessions and CSRF protection; goboat's dashboard login doesn't cover your app.
- Don't log secrets or full request bodies: the owner reads logs in the dashboard.

---

## 15. Complete example

A notes app with migrations, a health check and graceful shutdown. It builds and runs as is.

`goboat.toml`:

```toml
contract    = 1
health_path = "/healthz"
```

`migrations/0001_init.sql`:

```sql
CREATE TABLE notes (
    id         INTEGER PRIMARY KEY,
    body       TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX notes_created ON notes (created_at);
```

`migrations/0002_pinned.sql`:

```sql
-- Expand only: a new column with a default, so the previous build keeps
-- working on this schema.
ALTER TABLE notes ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
```

`main.go`:

```go
// notes: a minimal goboat app.
package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // pure Go: builds with CGO_ENABLED=0
)

//go:embed migrations/*.sql
var migrations embed.FS

var page = template.Must(template.New("page").Parse(`<!doctype html>
<title>notes</title>
<form method="post" action="/notes"><input name="body" required> <button>Add</button></form>
<ul>{{range .}}<li>{{.}}</li>{{end}}</ul>`))

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)) // goboat captures stderr
	if err := run(log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1) // goboat restarts the app with backoff
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	db, err := openDB(filepath.Join(os.Getenv("DATA_DIR"), "app.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	// Migrations run before listening: the health check only passes once
	// the schema is ready.
	if err := migrate(ctx, db, log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		// Explicit columns, never SELECT *: an older build may run on a
		// newer schema after a rollback.
		rows, err := db.QueryContext(r.Context(), `SELECT body FROM notes ORDER BY id DESC LIMIT 50`)
		if err != nil {
			http.Error(w, "error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var notes []string
		for rows.Next() {
			var b string
			if rows.Scan(&b) == nil {
				notes = append(notes, b)
			}
		}
		page.Execute(w, notes)
	})
	mux.HandleFunc("POST /notes", func(w http.ResponseWriter, r *http.Request) {
		body := strings.TrimSpace(r.FormValue("body"))
		if body == "" || len(body) > 1000 {
			http.Error(w, "a note is 1-1000 characters", http.StatusBadRequest)
			return
		}
		if _, err := db.ExecContext(r.Context(), `INSERT INTO notes (body, created_at) VALUES (?, ?)`, body, time.Now().Unix()); err != nil {
			http.Error(w, "error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	srv := &http.Server{
		Addr:              net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", srv.Addr, "commit", os.Getenv("GOBOAT_COMMIT"))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// goboat sends SIGTERM and kills the app 10 s later.
	sctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func openDB(file string) (*sql.DB, error) {
	dsn := "file:" + file + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil { // fails fast on a bad DATA_DIR
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate applies migrations/NNNN_name.sql that are not applied yet, in
// order, each in one transaction together with its schema_migrations row.
// It ignores versions it doesn't know: after a rollback an older build runs
// on a database a newer build migrated.
func migrate(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(path.Base(name), ".sql")
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, time.Now().Unix()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Info("migration applied", "version", version)
	}
	return nil
}
```

Set it up with:

```bash
go mod init example.com/notes
go get modernc.org/sqlite
go mod tidy
```

Try it locally the way goboat runs it:

```bash
CGO_ENABLED=0 go build -o app . && mkdir -p data
DATA_DIR=$PWD/data HOST=127.0.0.1 PORT=8080 ./app
curl localhost:8080/healthz            # ok
```

To test rule 10.1, build the previous commit and run it on the database the current one migrated: it must start and serve.

---

## 16. Checklist before the first deploy

- [ ] `goboat.toml` at the root with `contract = 1` and a `health_path` that checks the database.
- [ ] `CGO_ENABLED=0 go build` works on a clean checkout (generated files committed, `go.sum` committed, private modules vendored).
- [ ] Listens on `net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT"))`.
- [ ] Databases under `$DATA_DIR` with the §9 settings; no other persistent files you can't afford to lose.
- [ ] Migrations at startup, before listening; forward-only, one transaction each, unknown versions ignored, compatible with the previous release.
- [ ] Queries with explicit column lists.
- [ ] `SIGTERM` → `Shutdown` within 8 s → close the database → exit.
- [ ] Logs to stdout/stderr; no log files, no secrets in logs.
- [ ] Config and secrets from env vars; required ones checked at startup.
- [ ] Works within the chosen memory limit (128 MB – 1 GB).
- [ ] No reliance on the server's hostname or IP, on `$TMPDIR` across deploys, or on a second process.
