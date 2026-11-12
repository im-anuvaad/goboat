# goboat

A small, self-contained PaaS for **Go + SQLite** web apps on a single VPS behind Cloudflare.

Connect a GitHub repository, pick a branch and a domain, press **Deploy**: goboat downloads the code, builds it on the server and runs it. No release workflow, no Docker, no extra reverse proxy. SSH in once and run one command; everything else happens in a web dashboard.

```
Cloudflare (proxied, Full strict) ──HTTPS──► goboat ──► your Go apps on 127.0.0.1
                                             TLS, routing, builds, dashboard,
                                             deploys, backups
```

- **Deploy a branch with a click.** The app page lists the newest commits of the app's branch; **Deploy** builds the one you pick with the Go toolchain on the server and swaps it in. The previous builds are kept, so rollback is one click and needs no rebuild.
- **Only your Cloudflare account can reach your apps**: Cloudflare Origin CA certificates (Full strict), a Cloudflare-only IP filter and Authenticated Origin Pulls with goboat's own certificate. goboat removes every certificate and DNS record it no longer uses.
- **Light isolation**: each app runs as its own Linux user with a memory and CPU limit you pick from a list when you add it, and can change later.
- **SQLite backups to R2 or any S3-compatible bucket** every 10 minutes (skipped when nothing changed) and before every deploy, kept for 48 hours, with one-click restore.
- **Move to another server** from the dashboard: one command on the new server, one click to switch, typically under a minute of downtime.
- **Monitoring and logs**: CPU, memory, requests and response times per app, live logs, build and deploy logs.
- **Read-only status API** for scripts and AI agents ("is my blog up?"): [docs/api.md](docs/api.md).

Scope: 1 server (Ubuntu 24.04+), 1 user, a few Go apps, all domains on Cloudflare.

## What machine you need

Building Go code takes memory, so goboat is sized honestly:

| Machine | What fits |
|---|---|
| **2 vCPU, 2 GB RAM, 25 GB disk (recommended)** | goboat plus **1–2 production apps**, or **3–4 small apps** if you know they stay small (around 128–256 MB each) |
| Less than 2 GB RAM | Not supported: builds of real apps would be slow or fail |

The installer adds a **2 GB swap file** (unless the server already has that much). Builds may use it; apps never swap, so a build can slow down but cannot push your running apps out of memory. The Go toolchain (about 250 MB) and the module and build caches (a few GB over time) live on the server.

## Before you start

- A fresh Ubuntu 24.04+ VPS (x86_64 or arm64) with SSH access, ports 22 and 443 open.
- Your domains on your Cloudflare account. For each zone, turn on **SSL/TLS → Edge Certificates → Always Use HTTPS** (goboat has no port 80).
- A Cloudflare API token (**My Profile → API Tokens → Create Custom Token**) for your zones with:

| Permission | Used for |
|---|---|
| Zone → Zone → Read | finding the zone of each domain |
| Zone → DNS → Edit | proxied DNS records |
| Zone → SSL and Certificates → Edit | Origin CA certificates, the origin pull certificate |
| Zone → Zone Settings → Edit | turning on Authenticated Origin Pulls, reading and setting the SSL/TLS mode |

- A GitHub fine-grained token with *Contents: Read-only* on your app repositories (required for private repositories; recommended for public ones because unauthenticated GitHub API calls are rate limited).
- An R2 bucket with an R2 API token (*Object Read & Write* on that bucket), or any other S3-compatible bucket, for backups.

## Install

```bash
curl -fsSL https://github.com/im-anuvaad/goboat/releases/latest/download/install.sh | sudo bash
```

It asks nothing and takes about a minute: it installs goboat, the Go toolchain and the swap file, then ends with the address to open (`https://<server IP>`) and a **one-time password**. You can close SSH now.

## First steps in the browser

1. Open the address, accept the certificate warning (expected on this first visit), and sign in with the **one-time password**, leaving the email empty.
2. **Set up your account**: your email and a password (at least 12 characters).
3. **Settings → Cloudflare**: paste the API token. goboat checks it and lists the zones it can use and any missing permission.
4. **Settings → Dashboard domain**: e.g. `goboat.example.com`. goboat creates the DNS record, the certificate and the origin pull setup and shows the progress; the first time for a zone this takes about 10 minutes. If the zone's SSL/TLS mode isn't Full (strict), press **Set Full (strict)** under **Zones**. Then open `https://goboat.example.com` and sign in; from then on the IP address no longer answers.
5. **Settings → GitHub** and **Settings → Backups** (for R2: endpoint `https://<account id>.r2.cloudflarestorage.com`, region `auto`, bucket, keys).
6. **Your app repo** follows [docs/app-contract.md](docs/app-contract.md): a Go `main` package and a `goboat.toml` at the root.
7. **Apps → Add app**: pick the repository, then its **branch**, a **domain** (e.g. `blog.example.com`) and the **memory and CPU** for the app. Add env vars if needed, press **Deploy** on the newest commit and watch the build and deploy log.

The home page always lists what is still missing.

## Day to day

- **Deploy**: push to the app's branch, open the app page, press **Deploy**. goboat never deploys by itself.
- **Change the branch, domain or limits**: the app page's **Settings**. A new branch applies on the next deploy, a new domain at once, new limits on the next restart (the page says so).
- **Rollback**: pick one of the last 3 builds under **Rollback**.

## Moving to a new server

The old server keeps serving until you press **Switch now**.

1. Create the new VPS (same requirements as above).
2. In the old dashboard: **Settings → Move to another server → Start**. It shows one command.
3. Run that command on the new server over SSH. It installs goboat and copies the settings, apps, env vars and certificates from the old server. The new server then builds every app; the old dashboard shows the progress.
4. When every app is built, press **Switch now** in the old dashboard. The old server stops its apps and hands over their latest data; the new server starts them and points the DNS records at itself. Typically under a minute of downtime.
5. Sign in at your dashboard domain as usual (same account) and delete the old VPS.

If the old server is already gone, install goboat on a new one, repeat the first steps and **Add app** for each app: the form offers the app's backup folder in the bucket and restores its newest backup before the first deploy.

## Updating, and when you are locked out

Run the install command again. It installs the latest goboat and Go toolchain, keeps all apps and settings, restarts them (a few seconds of downtime) and prints a new one-time password. Use that password only if you need it, e.g. after forgetting your password or when the dashboard domain stops working: for 24 hours the server's IP address accepts it.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `no Cloudflare zone found for …` when saving a domain | The domain isn't on this Cloudflare account, is misspelled, or the token doesn't cover that zone |
| `… lacks the … permission` | Add the missing permission to the token, then **Sync now** in Settings |
| Build fails with `goboat.toml` errors | The file is missing at the repository root or has an unknown key; see [docs/app-contract.md](docs/app-contract.md) |
| Build fails with "the build ran out of memory", or builds are very slow | The build needed more than its 1.4–1.5 GB plus swap (the deploy log shows its peak memory and swap). Stop an app you don't need while building, or use a bigger server |
| Build fails on a private module dependency | Not supported: only the app's own repository is fetched with the GitHub token; other modules must be public or vendored (`go mod vendor`) |
| Dashboard domain stays "being set up" | The page says what it waits for: Cloudflare rolling out the origin pull certificate (about 10 minutes the first time), a zone in Flexible mode (**Set Full (strict)**), or a token without Zone Settings permission |
| Cloudflare error 521 | The SSL/TLS mode is Flexible or Off: **Settings → Zones → Set Full (strict)**. A Page Rule or Configuration Rule can also set Flexible for one hostname; remove it |
| Cloudflare error 526 | Full (strict) but the certificate isn't installed yet: **Sync now** in Settings |
| Cloudflare error 525 / 520 | Origin pulls were changed in Cloudflare; goboat repairs them on the next sync (**Sync now**) and waits a few minutes before requiring them again |
| **Settings → Cloudflare** shows "cleanup pending" | goboat could not delete a certificate or DNS record it no longer uses (the error is shown). It retries on every sync |
| `has a CNAME record` | Delete the CNAME in Cloudflare; goboat creates the A record |
| Nothing answers at all | Re-run the install command and use the address and one-time password it prints |
| Install stops with `port 443 is already in use by …` | Another web server holds port 443; goboat serves HTTPS itself. Stop and disable it, then run the install command again |

## License and maintenance

goboat is licensed under the [Apache License 2.0](LICENSE). It is built for our own use first: changes stay within the scope above (one server, one user, a few Go apps behind Cloudflare).

## Development

Requires Go (version in `go.mod`).

```bash
make build       # dist/goboat for this machine
make build-all   # static linux amd64 + arm64 binaries + checksums.txt
make lint        # gofmt, go vet, staticcheck
make test        # go test -race ./...
```

goboat itself is released as prebuilt binaries: push a `vX.Y.Z` tag. How goboat works inside, and how to check a change on a real server before releasing it: [docs/architecture.md](docs/architecture.md).
