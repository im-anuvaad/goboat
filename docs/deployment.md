# Deploying and running goboat

Everything needed to install goboat, deploy apps with it, keep it running, update it, move it to another server and fix problems. How to write an app for goboat is in [app-contract.md](app-contract.md); how goboat works inside is in [architecture.md](architecture.md).

**Contents:**
1. [What you need](#1-what-you-need)
2. [Install](#2-install)
3. [First-time setup](#3-first-time-setup)
4. [Adding an app](#4-adding-an-app)
5. [Day to day](#5-day-to-day)
6. [Backups and restore](#6-backups-and-restore)
7. [Updating goboat](#7-updating-goboat)
8. [Moving to another server](#8-moving-to-another-server)
9. [Locked out, or the server is gone](#9-locked-out-or-the-server-is-gone)
10. [Deleting an app, removing goboat](#10-deleting-an-app-removing-goboat)
11. [Troubleshooting](#11-troubleshooting)
12. [FAQ](#12-faq)

---

## 1. What you need

### A server

| | |
|---|---|
| **OS** | Ubuntu 24.04 or newer, x86_64 or arm64, with systemd and cgroup v2 (the default) |
| **Size** | **2 vCPU, 2 GB RAM, 25 GB disk** recommended. The installer refuses less than about 1.8 GB of RAM or 10 GB of free disk |
| **Ports** | 22 (SSH) and 443 open to the internet. Nothing else may use port 443 |

What fits on the recommended size: goboat plus **1–2 production apps**, or **3–4 small apps** if you know they stay around 128–256 MB each. goboat builds your apps on the server, and a first build can take over 1 GB of memory, so the installer adds a **2 GB swap file**. Builds may use it; apps never do.

### Cloudflare

- Your domains in your Cloudflare account (any plan, Free works).
- For each zone: **SSL/TLS → Edge Certificates → Always Use HTTPS** on. goboat has no port 80.
- A **Cloudflare API token** (*My Profile → API Tokens → Create Custom Token*) for the zones you'll use, with:

| Permission | Used for |
|---|---|
| Zone → Zone → Read | finding the zone of each domain |
| Zone → DNS → Edit | proxied DNS records |
| Zone → SSL and Certificates → Edit | Origin CA certificates, the origin pull certificate |
| Zone → Zone Settings → Edit | turning on Authenticated Origin Pulls, reading and setting the SSL/TLS mode |

goboat sets the zone's origin pulls up itself and checks that its SSL/TLS mode is **Full (strict)**; the dashboard offers a button to set it.

### GitHub

A **fine-grained token** with *Contents: Read-only* on your app repositories. Required for private repositories; recommended for public ones, because GitHub limits requests without a token to 60 an hour.

### A bucket for backups

Any S3-compatible bucket. With **Cloudflare R2**: create a bucket and an R2 API token with *Object Read & Write* on it; the endpoint is `https://<account id>.r2.cloudflarestorage.com`, region `auto`. Without a bucket, backups stay on the server only.

---

## 2. Install

On the server, as a user with `sudo`:

```bash
curl -fsSL https://github.com/im-anuvaad/goboat/releases/latest/download/install.sh | sudo bash
```

It asks nothing and takes about a minute. It:
1. checks the server (Ubuntu version, systemd, cgroup v2, CPU, RAM, disk);
2. downloads goboat and checks its SHA-256;
3. installs the latest Go into `/usr/local/lib/goboat/go` (a Go installed elsewhere is never touched), creates the build user and the 2 GB swap file;
4. installs and starts the `goboat` systemd service;
5. prints the address to open (`https://<server IP>`) and a **one-time password**.

You can close SSH now. To install a specific version: `curl -fsSL …/install.sh | sudo GOBOAT_VERSION=v1.0.0 bash`.

---

## 3. First-time setup

1. Open the printed address. Accept the certificate warning: it is expected on this first visit only.
2. Sign in with the **one-time password**, leaving the email empty.
3. **Set up your account**: your email and a password (at least 12 characters).
4. **Settings → Cloudflare**: paste the API token. goboat lists the zones it can use and any missing permission.
5. **Settings → Dashboard domain**, e.g. `goboat.example.com`. goboat creates the DNS record, the certificate and the origin pulls and shows the progress. The first time in a zone this takes about 10 minutes, because Cloudflare rolls the origin pull certificate out slowly. If the zone isn't in Full (strict), press **Set Full (strict)** under *Zones*.
6. Open `https://goboat.example.com` and sign in. From now on the IP address doesn't answer any more.
7. **Settings → GitHub**: the token.
8. **Settings → Backups**: endpoint, region, bucket, access key and secret. goboat saves them only after a test upload, read and delete.

The home page lists whatever is still missing.

---

## 4. Adding an app

The app's repository must follow the [app contract](app-contract.md): a Go `main` package and a `goboat.toml` at the root.

**Apps → Add app:**

| Field | |
|---|---|
| GitHub repository | Suggestions come from your token. The app is named after the repository (`My_Blog` → `my-blog`). |
| Branch | Empty = the repository's default branch. Deploys build commits of this branch. |
| Domain | **Required**, e.g. `blog.example.com`, in one of your Cloudflare zones. goboat creates its DNS record and certificate. |
| Memory, CPU | 128 MB – 1 GB, 0.5 – 2 cores. The form shows how much of the server's memory all apps may use together; keep it under about 75 %. |
| Start with data from | Only with a bucket: restores the newest backup of a bucket folder before the first deploy (e.g. when re-adding a deleted app). |

Then, on the app's page:
1. Add **env vars** the app needs (stored encrypted, never shown again).
2. Press **Deploy** on the newest commit and watch the build and deploy log.

---

## 5. Day to day

| Task | How |
|---|---|
| **Deploy** | Push to the app's branch, open the app page, pick the commit (newest preselected), **Deploy**. goboat never deploys by itself. The running version keeps serving during the build; the switch takes well under a second plus the app's start. |
| **Roll back** | *Kept builds* → **Roll back** next to one of the last 3 builds. No rebuild. The database is not rolled back. |
| **Change branch, domain, memory, CPU** | The app's **Settings**. A new branch applies on the next deploy, a new domain at once, new limits on the next restart (the page shows **Restart to apply**). The domain can be changed, not removed. |
| **Env vars** | The app's *Environment variables*. Apply on the next **Restart** or deploy. |
| **Restart, stop, start** | Buttons at the top of the app page. A stopped app stays stopped after reboots. |
| **Logs** | **Logs** on the app page (live). Deploy logs, with the compiler output and the build's memory and swap, are under *Deployments*. |
| **Monitoring** | Charts on each app page and under **Monitoring** (CPU, memory, requests, response times; 1 h, 24 h, 7 days). Warnings (disk, memory, swap, crashes, failed backups, certificates) appear on the home page. |
| **Status API** | **Account → API token** for a read-only token for scripts and AI agents: [api.md](api.md). |

Behaviour worth knowing:
- Only one build or deploy runs at a time on the server; others wait in a queue.
- An app that exits is restarted with backoff; after 5 exits in 5 minutes it is marked **crashed** and waits for **Start**.
- If a new build doesn't build or doesn't become healthy within 30 seconds, the previous one keeps or resumes serving, and the deploy log says why.

---

## 6. Backups and restore

| | |
|---|---|
| **What** | The SQLite databases in each app's data folder. Other files there (uploads) are **not** backed up. |
| **When** | Every 10 minutes when a database changed, before every deploy and rollback, and with **Back up now**. |
| **Kept** | 48 hours in the bucket (the newest always), in the folder `<app>/`; the last 2 also on the server. |

- **Restore:** the app's *Backups* → **Restore** next to a backup. goboat checks the backup completely first, then stops the app briefly, keeps the current databases in a `.pre-restore-<time>` folder (the last 2), puts the backup in place and starts the app. **Show all backups** lists every one, by day.
- **After a bad migration:** restore the backup of kind *pre-deploy* taken right before that deploy, then deploy fixed code. What was written after it is lost.
- **Bucket outage:** backups are kept on the server and uploaded when the bucket answers again; the home page warns meanwhile.
- **Changing the bucket:** enter the new settings in **Settings → Backups**. Old backups stay in the old bucket.
- goboat's own settings are not in the bucket. Moving them is what [§8](#8-moving-to-another-server) is for.

---

## 7. Updating goboat

Run the install command again:

```bash
curl -fsSL https://github.com/im-anuvaad/goboat/releases/latest/download/install.sh | sudo bash
```

It installs the latest goboat and Go, keeps every app, setting and backup, restarts goboat and the apps (a few seconds of downtime) and prints a new one-time password, which you can ignore. A database update runs by itself.

- To install a specific version, put `GOBOAT_VERSION=vX.Y.Z` before `bash` as in §2.
- Going back to an older version after an update that changed the database isn't supported: the older binary refuses the newer database.
- goboat never updates itself; the dashboard footer shows the running version.

---

## 8. Moving to another server

Moves everything (account, settings, apps, env vars, certificates and every file in the apps' data folders) to a new server. The old server keeps serving until you switch; measured downtime is under 2 seconds for small apps.

1. **Create the new server** (same requirements as §1). Don't install goboat on it yourself.
2. **Old dashboard: Settings → Move to another server**, enter your password, **Create move code**. It shows one command, valid for an hour. Treat it like a password: it gives a copy of everything.
3. **Run that command on the new server** over SSH. It installs the same goboat version and copies the settings, keys and certificates. The new server then builds every app and copies their data while the old one keeps serving; the old dashboard shows each step. **Settings and apps can't be changed** until the move ends.
4. When the page says **ready**, press **Switch now**. The old server stops its apps, the new one takes the files that changed, starts the apps and points the DNS records at itself.
5. Sign in at your dashboard domain as usual. The old server now only says that it was moved, passes stray connections on for 30 minutes, and never touches Cloudflare or the bucket again. **Delete it after that half hour.**

| If… | then |
|---|---|
| you press **Cancel** before the switch is done | everything stays on the old server; the new one removes what it received |
| the new server fails before the switch (a build error, an older version) | the same: the old server keeps serving, unfrozen; the error is shown. Fix it and start a new move |
| the new server fails after the switch | the old server restarts its apps when told so (or press **Cancel**); the new server keeps what it has and retries, it never throws anything away the old server doesn't have |
| goboat restarts on the old server during the move | the move ends as failed and the apps run there as before |

The versions must match: if you updated only one server, the move stops and says which one to update. Logs, metrics and the server's local backup copies don't move; the bucket backups stay usable.

---

## 9. Locked out, or the server is gone

**Forgot the password, or the dashboard domain doesn't work:** run the install command again (§7). It prints a new one-time password, valid for 24 hours on `https://<server IP>`. Sign in there with the empty email and set a new password.

**Nothing answers at all:** SSH in and check `systemctl status goboat` and `journalctl -u goboat -n 100`, then run the install command again.

**The old server is gone (no move possible):**
1. Install goboat on a new server and repeat the first-time setup (§3) with the same Cloudflare token, dashboard domain and bucket.
2. **Add app** for each app with the same repository and domain. *Start with data from* preselects the bucket folder with the app's name: its newest backup is restored before the first deploy.
3. Add the env vars again and deploy. DNS records follow automatically.

Only SQLite data comes back this way (up to 10 minutes old); other files in the data folders are lost.

---

## 10. Deleting an app, removing goboat

**Delete an app:** the app's page → *Delete app*, type its name. goboat stops it and removes its builds, logs and system user, and its DNS record and certificate at Cloudflare (retried until Cloudflare confirms; shown under *Settings → Cloudflare → Cleanup pending*). By default its data folder is kept in `/var/lib/goboat/deleted/<app>-<time>/` and its backups in the bucket; untick or tick the boxes to change that.

**Remove goboat from a server:** delete the apps in the dashboard first (that cleans Cloudflare up), then:

```bash
sudo systemctl disable --now goboat
sudo rm -f /etc/systemd/system/goboat.service && sudo systemctl daemon-reload
sudo rm -rf /usr/local/bin/goboat /usr/local/lib/goboat /var/lib/goboat /etc/goboat
sudo swapoff /goboat.swap && sudo rm -f /goboat.swap && sudo sed -i '\#^/goboat.swap #d' /etc/fstab
sudo rm -f /etc/sysctl.d/90-goboat.conf
sudo userdel goboatbuild
```

The dashboard domain's DNS record and certificate and the zone's origin pull setup stay in Cloudflare; remove them there (*DNS*, *SSL/TLS → Origin Server*) if you don't install goboat again. Backups stay in the bucket.

---

## 11. Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| Install stops with `port 443 is already in use by …` | Another web server holds port 443; goboat serves HTTPS itself. Stop and disable it, run the install command again |
| Install stops about RAM or disk | The server is below about 2 GB of RAM or 10 GB free disk |
| `no Cloudflare zone found for …` when saving a domain | The domain isn't in this Cloudflare account, is misspelled, or the token doesn't cover that zone |
| `… lacks the … permission` | Add the permission to the token in Cloudflare, then **Sync now** in Settings |
| `has a CNAME record` | Delete the CNAME in Cloudflare; goboat creates the A record |
| Dashboard domain stays "being set up" | The page says what it waits for: the origin pull rollout (about 10 minutes the first time), a zone in Flexible mode (**Set Full (strict)**), or a token without Zone Settings permission |
| Cloudflare error **521** | The SSL/TLS mode is Flexible or Off: **Settings → Zones → Set Full (strict)**. A Page Rule or Configuration Rule can also set Flexible for one hostname; remove it |
| Cloudflare error **526** | The certificate isn't installed yet (or Cloudflare refused one briefly): **Sync now**. goboat also retries after 1, 5 and 15 minutes |
| Cloudflare error **525 / 520** | Origin pulls were changed in Cloudflare; the next sync repairs them and waits a few minutes before requiring them again |
| *Cleanup pending* in Settings | goboat couldn't delete a certificate or DNS record it no longer uses; the error is shown and it retries on every sync |
| *Unused certificates* in Settings | Origin CA certificates for hostnames goboat doesn't serve (older setups, other servers). **Revoke** the ones you know are unused; goboat never does it by itself |
| Build fails with `goboat.toml` errors | The file is missing at the repository root or has an unknown key: [app contract §3](app-contract.md#3-repository-and-goboattoml) |
| Build fails with "the build ran out of memory", or is very slow | It needed more than its 1.4–1.5 GB plus swap (the deploy log shows the peaks). Stop an app you don't need while building, or use a bigger server |
| Build fails on a private module | Only the app's own repository is fetched with the token; vendor other private modules (`go mod vendor`) |
| Build fails with `undefined` errors after editing templ or similar files | The generated files aren't committed or are stale: regenerate and commit them |
| "failed its health check" | The app didn't answer `health_path` with 2xx/3xx within 30 seconds. The deploy log has the reason (crash, wrong port, slow migration); the previous build is serving again |
| App shows **crashed** | It exited 5 times in 5 minutes. Read its **Logs**, fix, deploy or press **Start** |
| "killed for running out of memory" | The app went over its memory limit: raise it in Settings and restart, or reduce its memory use |
| Warning about swap use outside builds | Apps and the system need more memory than the server has: lower limits, stop an app, or use a bigger server |
| Backups "only on this server" | The bucket doesn't answer or the keys are wrong; they upload by themselves once it works. Check **Settings → Backups** |
| A move stops with a version mismatch | Update the server running the older version (§7), then start a new move |

Over SSH, `journalctl -u goboat -f` shows goboat's own log; app logs are in `/var/lib/goboat/apps/<app>/logs/`.

---

## 12. FAQ

**Can it deploy on every push?** No, on purpose: you press Deploy. The status API is read-only too.

**Can I host apps that aren't Go?** No. goboat builds Go with `CGO_ENABLED=0` and runs the binary.

**Does it work with templ, htmx, Tailwind?** Yes, if generated files are committed: `templ generate` output and the built CSS. goboat runs only `go build`. See [app contract §4](app-contract.md#4-the-build).

**Can an app use Postgres or Redis?** It can connect to any outside service through env vars, but goboat neither runs nor backs them up. It is built for SQLite.

**How many apps?** Up to 20, but memory decides: see §1.

**Several users or servers?** One account per installation and one server. Run another installation for another server.

**Domains outside Cloudflare?** No: every domain must be a proxied hostname in your Cloudflare account. That is what keeps the server reachable only through your Cloudflare account.

**Where is everything on the server?** `/usr/local/bin/goboat`, `/var/lib/goboat` (database, apps, builds, caches, backups), `/etc/goboat/master.key` (encrypts tokens and env vars; never share it), `/usr/local/lib/goboat/go` (Go), `/goboat.swap`.

**What if the bucket or Cloudflare is down?** Apps keep running. Backups wait on the server; DNS and certificate changes wait for the next sync.
