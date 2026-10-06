# vops or versionated-ops

`vops` is a simple project. If it's not simple, then it's wrong.
vops is a cli and a daemon to self-host containers with less pain and overhead than traditional operations: a git repo of compose files is the whole state of a server.

## State, development, lifecycle

**v1.5, in production on one real server.** Everything below "Features" is covered by end-to-end tests against real podman (rootless) and by a production-like check (`just vps-test`: systemd + real sshd + podman in a container, an in-place upgrade from v1.2, then an upgrade, a daemon SIGKILL and a rolling release under load with 0 failed requests). It also runs a real VPS (Ubuntu 24.04, ext4, rootless as a normal user with linger, real DNS and Let's Encrypt), migrated from docker compose + caddy + registry:2 + watchtower with about 1m20s of downtime: [reference/migrating.md](reference/migrating.md). It survived an `apt upgrade` + reboot: linger starts the daemon, which brings every project back in dependency order (~7s for 6 containers).

- Runtime deps on the host: linux, systemd, podman 4+ (netavark), git. btrfs (+ btrfs-progs) for snapshots and data rollback (image rollback works without it); everything else works without it.
- Two processes of the same binary: the daemon (deploys, registry, dashboard; idles at ~16MB RSS) and a small proxy that owns :80/:443. The binary is static, ~13MB.
- Why things are the way they are: [reference/decisions.md](reference/decisions.md).

## How it works

```
your laptop                                   the host
./ (git repo)  --- git push over ssh --->     ~/vops/ (working tree, updated on push)
  vops.yml                                      shop/api/compose.yml  -> containers
  shop/api/compose.yml                          registry/data/        -> your images
  vops-lock.yml (gitignored)                  ~/.vops/                -> sqlite, certs, binary, sockets
```

`vops sync` pulls from the host, pushes to it, shows what will change and asks before applying. The daemon runs the containers with podman and serves a registry at `registry.<domain>` and a dashboard at `vops.<domain>`. The proxy, a separate process, routes `https://<service>.<project>.<domain>` to the containers and gets certificates from Let's Encrypt; it keeps serving when the daemon crashes or is upgraded.

## Install

```sh
go install github.com/sh-lucas/vops/cmd/vops@latest   # go 1.27+, puts vops in $(go env GOPATH)/bin
```

`vops install` copies this same binary to the host, so it must match the host (linux, same arch). Running it again upgrades the host in place (from v1.2 too: the proxy moves to its own service, a few seconds of downtime once). The cli and the host must run the same major.minor: a mismatch is refused with what to do (update yours, or `vops install` to upgrade the host); patch versions may differ. From a mac or for an arm64 host:

```sh
GOOS=linux GOARCH=arm64 go install github.com/sh-lucas/vops/cmd/vops@latest
vops install user@host --binary $(go env GOPATH)/bin/linux_arm64/vops
```

## Quick start

```sh
# on your machine, in a new or existing repo
vops init --domain example.com --email you@example.com
vops install root@203.0.113.10          # or user@host --ssh-key ~/.ssh/id_ed25519
# dns: example.com and *.example.com -> the host

mkdir -p site/html && echo hello > site/html/index.html
cat > site/compose.yml <<'EOF'
services:
  web:
    image: docker.io/library/nginx:alpine
    volumes: [./html:/usr/share/nginx/html:ro]
    environment: [SOME_SECRET]            # comes from `vops env set`
    x-vops: {port: 80}
EOF
vops env set site SOME_SECRET              # prompts, never echoed, never readable again
git add -A && git commit -m site && vops sync
# -> https://site.example.com
```

## Features

### Git sync
- `vops sync`: `git pull` from the host, `git push`, plan, confirm, apply. `-y` for CI.
- Several people (or CI) can push; sync merges the host's commits first.
- Plain `git push vops` works too; the dashboard then shows "the host differs from git" with an apply button.
- Login: `vops-lock.yml` (gitignored, written by `install`) or `--host user@host --ssh-key path`. See [reference/ci.md](reference/ci.md) for GitHub Actions.

### Projects
- Any git-tracked dir with `compose.yml` (or `*.compose.yml`, `docker-compose.yml`) is a project. Nested dirs are nested projects: `shop/api` is served at `api.shop.<domain>`.
- vops reads compose itself (a strict subset: unknown keys are errors). Reference: [reference/compose.md](reference/compose.md).
- Networking like compose: services reach each other by name, custom and `internal` networks, aliases, static ips. On top of that, a shared network where any project reaches any other one at `<service>.<project reversed>` (e.g. `db.api.shop`), opt-out per service.
- `depends_on` conditions, including `service_completed_successfully` jobs (migrations) that gate the services depending on them; `profiles` via `COMPOSE_PROFILES`.
- Rolling releases for routed services: new replica, readiness check, switch traffic, drain, remove old. A failed readiness check, or a proxy that doesn't confirm the switch within 10s, keeps the old version serving. Everything else is recreated (stop, then start). Replicas that fail readiness are always removed, never left running.
- Env vars per project, write-only: set from the cli or the dashboard, used for `${VAR}` and `environment: [VAR]`, never shown again.
- What each service gets: `vops env ls <project>` and the Environment tab list, per service, every variable its containers get and where it comes from (`compose` literal, `env_file`, `vops` env directly or through `${VAR}`, or `missing`: referenced but set nowhere), plus vops keys no service uses. Names only, never values. A literal whose name looks secret is flagged "committed to git: move to vops env".
- Disable/enable a project without deleting it; removing its dir from git removes its containers (volumes and data dirs stay).

### Secrets backup in git
- Env secrets live only on the host, so a dead host used to mean setting them all again. Now the repo holds `.secrets.age`: every `vops env` secret (production and previews), encrypted to the ssh keys that already reach the host. No new tool or key to learn: you open it with your ssh key.
- Who can decrypt: every ed25519/rsa key in the host's `~/.ssh/authorized_keys` (read live), root's keys (copied by `vops install` when it has passwordless sudo; hosting panels put theirs there) and `secrets.recipients` in `vops.yml` (ssh public keys or `age1…` keys, desired state like the rest). `vops env recipients` and the Environment tab list them (comment, type, source) and the keys left out (ecdsa, security keys) with why.
- The host encrypts and never commits; your cli does: `vops env set/rm` and `vops sync` fetch the host's latest and commit only `.secrets.age` (`secrets backup updated: .secrets.age (13 secrets, 2 keys) — committed`), leaving what you staged alone. A `.gitignore` that matches dotfiles doesn't stop it (added with `-f`, with a hint). Dashboard changes land on the next sync; until then `vops status` and the dashboard say git is behind. It is never a deploy. The file only changes when a secret or a key changes.
- Restore: on a new host, `vops sync` sees a backup this host didn't write and offers `restore 4 secret(s) from .secrets.age? [Y/n]` (`-y` says yes). `vops env restore [file]` does it explicitly. Decryption happens on your machine (`--ssh-key`/`vops-lock.yml` key, then `~/.ssh/id_*`, asking passphrases); values go over ssh stdin. Only missing keys are set, never overwritten. Sync warns when none of your keys can open the backup.

### Data safety: snapshots and rollback
- Every named volume and every bind mount inside the project dir is created as a btrfs subvolume, so snapshots are instant and copy-on-write, even for a 50GB database.
- Before any deploy that changes a project, its data is snapshotted (`pre-deploy`); containers using it are paused for the few milliseconds it takes, so all volumes are captured at the same instant.
- `vops rollback <project> [deploy-id]` goes back to right before a deploy (default: the last one that isn't a rollback): the **images** that ran then and the **data** from just before it. It shows what it will do per service (`web: shop/web:v2 (sha256:…) → shop/web:v1 (sha256:…)`, skipped ones and why, the snapshot, the git command if the data's commit differs) and asks. `--images` or `--data` does only that part, `--service web` only some images, `--snapshot id` restores the data of any snapshot (manual ones too).
- Images roll back without git: each service runs the image it ran then, by digest, with a rolling release (no downtime). The service is **pinned**: `vops status`, `vops plan` and the dashboard show `web pinned to #12, compose says shop/web:latest`, and nothing is pending. A pin ends by itself when a newer version arrives (a push of the tag it runs, or another `image:` in compose; other compose changes keep it), or with `vops unpin <project> [service]`. Built images (`build:`) and local images without a digest can't roll back without the code: they are skipped and the plan says so.
- Data rollback stops the project, snapshots the current data (`pre-rollback`), restores, and starts it again (with the pinned images when both roll back). It is always of the whole project: every volume goes back to the same instant. There is no per-service restore on purpose: the db back an hour and the uploads dir not is data that no longer matches. Compose and code are not touched; the snapshot says which commit its data belongs to.
- Every rollback can be undone: `vops rollback <project> <rollback-id>` goes back to right before it, with the same parts.
- `vops snapshot ls|create|rm`, and the same in the dashboard. The newest 5 automatic snapshots per project are kept (`snapshot_keep` in `vops.yml`); manual and uploaded ones stay until deleted.
- Backups off the host: `vops snapshot export shop 12 > shop.tar.gz` (or Download on a snapshot, or "Download current data" in the timeline) streams a snapshot as a .tar.gz with owners, modes and xattrs kept. `vops snapshot import shop < shop.tar.gz` (or "Upload backup") turns one back into a snapshot of the same project, which restores like any other (`vops rollback shop --snapshot <id>`, undoable). Imports are checked entry by entry (no absolute paths, `..`, devices, or writes through symlinks) and a failed one leaves nothing behind.
- Deploy history: every apply that changes a project (sync, apply, dashboard, registry push) and every rollback is recorded with its commit, the images that ran (by digest when known), its result and the snapshot of the data right before it. `vops history <project>`, or the project's Timeline tab.
- Timeline (dashboard): newest first, "now" on top. Each point shows its commit and message, image changes (`web: v1 → v2`), result, snapshot and the previews branched from it. "Roll back…" opens a dialog with a checkbox per service (current → target image; unavailable ones disabled with the reason) and one for the data (and tells you the git command if the code differs); snapshot nodes restore data only. "Preview from here" opens a preview of that point: its commit, its images by digest, a copy of its snapshot. After a rollback the project shows "Rolled back to before #N (images, data) · Undo" until its next deploy; pinned services have a "pinned · #N" badge with Unpin, and the overview lists them as warnings.
- The registry keeps the images of the last `image_keep` (10) deploys of every project, of pins and of previews, even once their tag moved on, so rollback and "Preview from here" work for them. Older points show their image as gone.
- Works rootless (through `podman unshare`), no root and no special mount options.

### Previews (database branching)
- Opt-in per service: only a service with `x-vops.preview` can get a preview. It names what runs with it (`with`) and whose data is copied from production (`copy`, which runs too); nothing else runs, and only that data is copied:
  ```yaml
  services:
    web:
      x-vops: {preview: {with: [redis], copy: [db]}}   # `preview: {}` = just web
  ```
- `vops preview up shop --name pr-42` runs every declared service of project `shop` at `https://web.pr-42.shop.<domain>`: its own containers, networks and volumes, the code of the applied commit (a git worktree in `~/.vops/previews/`), and **a copy-on-write copy of the `copy` services' data**, made in O(1) even for a 50GB database. A destructive migration in the preview leaves production untouched. Every other volume starts empty.
- `--image web=registry.example.com/shop/web:pr-42` runs another image and makes the preview about web (its declaration); `--ref my-branch` another commit (a local branch is pushed to the host first); `--from <snapshot id>` another data point. `vops preview ls [project]`, `vops preview rm shop pr-42`.
- From the registry: pushing `shop/web:preview-pr-42` creates or updates preview `pr-42` of the projects where a service running `shop/web` has `x-vops.preview`, with that image. Pushing `shop/api:preview-pr-42` too lands in the same preview and adds api's declaration. A service without `x-vops.preview` gets nothing (an event says so). A `preview-*` tag never redeploys production.
- Env: previews never get production's env. They get `preview.env` from the project dir (committed, non-secret overrides like `SMTP_HOST=mailpit`) and preview secrets on top: `vops env set shop --preview STRIPE_KEY`.
- Dashboard: a Previews tab per project (urls, where it came from, overrides, expiry, logs, delete, new preview), previews in the sidebar under their project, and "Preview from here" on any timeline point.
- Guardrails by default: previews are off the shared network (they can't reach production, nothing reaches them), services in no declaration (workers, crons) don't run, a `depends_on` outside the preview is an error, no host ports, no extra domains. At most `preview_max: 5` at once; a preview not updated for `preview_ttl: 3d` is removed (both in `vops.yml`). `rm` leaves nothing behind: containers, networks, volumes, data, worktree.

### Audit log
- Every write to the host's database (env changes, users and their repos, logins, snapshots, projects and their limits) is recorded by sqlite triggers, so no code path can forget it. Secrets never land there. `vops audit`, or the Events page.

### Config
- Everything is in `vops.yml` at the repo root (`vops init` writes it with comments): domain, email, listeners, tls, snapshots, images kept, preview limits, who else can open `.secrets.age`. Changes show up in the plan and take effect on apply, like compose files.
- The host keeps the last applied copy in `~/.vops/config.yml` and always runs from it, so a broken `vops.yml` never breaks the daemon. New http/https/tls restart the proxy in place (sites blink for a moment), a new `ui` the daemon (sites don't notice); containers keep running.

### Registry
- Own OCI registry at `registry.<domain>` (works with `podman push`/`docker push`, manifest lists, referrers).
- Users are admins or deployers. Admins log into the dashboard and push/pull every repo with their password (`vops user add ops --admin` prompts it). Deployers only use the registry, with a generated token: every repo (`--global`) or a list (`vops user add ci --repo shop/web --repo shop/api`; a tag suffix like `:latest` is ignored). Push and pull are the same permission. `vops user ls|token|password|rm`, or the Users page.
- Pushing a tag that a service runs redeploys it (rolling). No watchtower. Opt out with `x-vops.watch: false`. Tags like `preview-pr-42` create previews instead (see Previews).
- Garbage collection: dashboard button, `vops registry gc`, and daily. It keeps untagged images that pins, previews and the last `image_keep` deploys of each project run.

### Proxy
- Our own, in its own process (`vops-proxy.service`): :80/:443, Let's Encrypt, http→https, HSTS, routing to containers. It knows nothing about sqlite, podman or the registry.
- The daemon crashing, deadlocking (a watchdog restarts both) or being upgraded never takes the sites down; the dashboard and registry hosts answer 502 until it's back. `vops install` restarts the proxy only when the proxy itself changed.
- The daemon sends it the routing table on every change and waits for the swap (rolling releases stay zero-downtime; one the proxy never confirms fails and keeps the old replicas); the proxy keeps its last table on disk, so it restarts with the right routes even with the daemon down.
- `vops status` and the dashboard's top bar show whether it is up, its version and routes, and warn when it is down or out of sync.
- Per-project limits in compose (`x-vops: {rate: 20/s, burst: 40, max_body: 10MB, timeout: 5m}` at the top level): a token bucket per client ip (`429` + `Retry-After`), a request body limit (`413`) and how long to wait for a container's response headers (default 60s, then `504`; streamed bodies like SSE are never cut), for every routed service of the project and its previews. The dashboard and the registry are never limited. `vops status` and the project header show them and what they refused.
- Hardened defaults: 10s for request headers, at most 64KB of them, a request body that sends nothing for 30s is dropped (`408`), at most 256 connections per client ip and 10000 in total. `X-Forwarded-For` is always the client's address, never what it sent.

### Dashboard
- `vops ui` opens it through an ssh tunnel; also at `https://vops.<domain>`, so it is a web login (an admin's user and password, session cookie), not ssh; deployers can't log in. Anything you do from it (apply, rollback, env, users) is available to every admin. The top bar shows who is logged in.
- Sidebar with every project and its health, overview with stats and warnings, pending changes + apply from any page. Per project: services (restart, unpin), timeline (deploys, rollbacks, snapshots; roll back images and/or data, preview from a point), previews, logs (live, search, date range, download), env vars (production or previews), events. Also registry images and users, git-tracked files (text only), events (50 per page, load more) and audit log, a Notifications page (devices, open and recent alerts, what you get, thresholds), and a System page with the host's cpu, memory, disk io and space, network, pressure (PSI), kernel and uptime, live every 3s (`vops status` prints a one-line summary). Works on a phone.

### Notifications
- Web Push to your phone or computer, no third-party service: Notifications page → "Enable notifications on this device" (and "Send test"). Works on `https://vops.<domain>` and through `vops ui` (localhost). iPhone/iPad: add the dashboard to the home screen first (Share → Add to Home Screen), iOS only pushes to installed web apps. Clicking a notification opens the project's page.
- What is sent, all on by default: **deploy failed** (apply, push, rollback, preview), **restart** (a container died or restarted without vops stopping it, with exit code and OOMKilled), **out of memory** (OOM-killed or SIGKILLed containers, the kernel OOM killer on the host), **down** (a routed service with no running replica or not answering its probe `down_after` times), **unhealthy** (`x-vops.health` answering 4xx/5xx, or the compose healthcheck failing), **resources** (host cpu > 90% for 10m, memory available < 10% for 5m, a filesystem > 90%, memory pressure (PSI) > 20%, before an OOM killer acts; containers near their memory limit or cpu quota). Off by default: **resolved** and **deploy succeeded**. Deploys, rollbacks and restarts done by vops never alert.
- An alert is sent at once, then every hour while it lasts, and stops after 24h (expired). Repeated events coalesce ("shop/web restarted (5 times in 12m)") and end after 30 quiet minutes; conditions end when they clear. Anyone can silence an alert for everyone; the badge in the sidebar counts open ones. `vops notifications` lists them.
- Per user: which kinds you get. Global, under "Advanced": every threshold and interval, with reset to defaults. Devices of every user are listed (last delivery, last error) and anyone can remove any. Nothing of this is in `vops.yml`: it lives in the host's database.
- You only get alerts of projects you can see (admins: all; host-wide alerts: global users).

### Logs
- Containers log to journald; `vops logs <project> [service] -f --grep x` or the dashboard. History survives rollouts; search covers all of it, and the dashboard loads older lines as you scroll up.
- `--since`/`--until` filter by date (`2006-01-02`, `2006-01-02 15:04`, RFC3339, or a duration like `2h` meaning now minus it); the api reports the full available range so the UI knows how far back it can go.

## Limitations

Know these before putting something important on it:

- **Compose is a subset, run by vops itself** (not podman-compose): it translates each service into `podman run`. Unsupported keys are errors, never silently ignored. Not supported: `secrets`, `configs`, `extends`, `links`, `container_name`, `depends_on.restart`, `network_mode: service:x`, most of `deploy`. Full list: [reference/compose.md](reference/compose.md).
- **Semantics that differ from compose:** containers are named `vops-...` (so `podman compose ps` doesn't see them), `restart` defaults to `unless-stopped`, every `depends_on` waits for readiness, and during a rolling release two versions run side by side for a few seconds.
- **One host.** No clustering, no failover; the daemon, proxy and registry run on the same machine as the containers. The proxy is a separate process, so a daemon crash or upgrade doesn't stop the sites, but the host (and the proxy restarting for new http/https/tls) still does.
- **Podman only, netavark only.** CNI setups can't resolve service names.
- **Env values are hidden from the api and dashboard, not from the host:** anyone with a shell on the host can see them with `podman inspect`.
- **Recreate means downtime:** services with published `ports`, without `x-vops.port`, or on a network whose definition changed are stopped before the new container starts.
- **Snapshots need btrfs** and cover only named volumes and bind mounts inside the project dir (not external volumes or absolute host paths). Data created before vops made it a subvolume (non-empty plain dirs) is left alone and shown as "not covered".
- **Snapshots are crash-consistent**, like pulling the plug: fine for postgres, mysql/innodb, sqlite, anything with a journal; not a replacement for application-level backups, and they live on the same disk: copying them off the host (`vops snapshot export`) is up to you, nothing is sent anywhere by itself.
- **Rollback restores images and data, not compose config or code.** Env, ports and the compose file stay as they are; built images (`build:`) need git to go back (the dashboard shows the command). Restoring data stops the project's containers while it runs (seconds); rolling back only images doesn't.
- **Previews hold production data.** Personal data, and whatever the code does with it: a queue of real emails, webhooks, a scheduled charge. Their env is not production's, but the data keeps production's own credentials (db users and passwords), and anything the preview's env doesn't redirect still goes out for real. Copy only what you need (`copy`), leave workers and crons out, point SMTP and payment keys at test ones.
- **Previews need btrfs for data** (without it they start empty) and can't run services with external volumes or projects with fixed-subnet networks. Absolute bind mounts and external networks are shared with production. Registry tags pushed for previews stay until `vops registry rm`.
- **`.secrets.age` is in git history forever:** a key removed from `authorized_keys` still opens the old commits. When someone leaves, rotate the secrets they could read. ecdsa and security keys (`sk-*`) can't decrypt (age needs ed25519 or rsa), and neither can a key that is only in ssh-agent: keep the key file, or add an `age1…` key in `secrets.recipients`.
- **Logs are journald's:** retention and disk use follow its config (`/etc/systemd/journald.conf`); no long-term log store.
- **Third-party images are not re-pulled** on their own: `postgres:16` stays at the version first pulled until you change the tag.

## Commands

```
vops init | install user@host [--force] | sync [-y] | ui
vops status | plan | apply [-y] [project...]
vops logs <project> [service] [-f] [-n N] [--grep s] [--since t] [--until t]
vops restart <project> [service] | enable <project> | disable <project>
vops env ls|set|rm <project> [--preview] ...   vops env recipients | restore [file]
vops user ls|add|rm|token|password ...
vops registry ls|rm|gc                    vops events | notifications | audit | version
vops snapshot ls|create|rm|export|import ...   vops history <project> [-n N]
vops rollback <project> [deploy-id] [--images] [--data] [--service s]... [--snapshot id] [-y]
vops unpin <project> [service...]
vops preview up <project> --name n [--image svc=ref]... [--ref r] [--from id]
vops preview ls [project] | rm <project> <name>
```

Inside a linked repo commands run on the host over ssh (`sync`, `apply` and `install` only work that way); on the host they talk to the daemon directly. The web-facing parts are the dashboard (admins) and the registry (admins' passwords, deployers' tokens), see Dashboard and Registry. On the host, systemd runs `vops daemon` and `vops proxy`.

## Development

```sh
just test       # everything (with -race, needs cgo), needs podman; uses an isolated podman storage in ~/.cache/vops-test
just build      # ./vops for this machine
just release    # dist/vops-linux-{amd64,arm64}
just gen        # sqlc generate (after editing internal/store/queries.sql or migrations/)
just vps-test   # systemd + real sshd + podman in a container as the host (slow, needs network)
```

Tests are end to end on purpose: `e2e/` drives the real binary (install, sync over a fake ssh, registry push, auto redeploy, a second developer, users (an admin pushes everything, a deployer only its repos, `--pattern` refused), the dashboard api with a second admin, a backup exported and imported through the ssh forwarding and restored, previews from registry tags, a push recorded in the history, a preview of a past deploy by digest, an image rollback with its pin and unpin; the proxy as its own process: a rolling release through it with zero failed requests, the daemon SIGKILLed while sites answer, the proxy restarting from its table on disk with the daemon dead, a rolling release while the proxy is down (fails, old replicas keep serving); cli/host version mismatches, upgrade and refused downgrade; the secrets backup committed alone, kept out of deploys, saved after a dashboard change and restored on a new host), `just vps-test` upgrades a real v1.2 host (systemd) in place and checks an upgrade and a daemon SIGKILL under load with zero failed requests, `internal/deploy` checks zero failed requests during a rolling release, runs a destructive migration in a preview of a real postgres, walks the deploy history (deploys, a failed one, rollback and undo, a preview from a past deploy) and rolls images back through its own registry (zero failed requests, pins ending on a push or a compose change, images and data together, gc keeping what rollback needs), round-trips a backup (export, a truncated import leaving nothing, import, restore, owners kept, never pruned), applies proxy limits without recreating containers and checks failures leave nothing half-done (a first deploy that never gets ready leaves no route, an unconfirmed switch keeps the old replicas, a failed pause or pre-deploy snapshot changes nothing, a restore failing on its second volume puts the first back, and stays stopped when that fails too), `internal/registry` pushes and pulls with real podman and refuses traversal in names. `e2e` also checks notifications: a deploy raises nothing, `podman kill` raises a restart (and an oom for the SIGKILL), a `/health` answering 500 raises an unhealthy alert, and a fake push service decrypts what a subscribed admin gets. Unit tests cover Web Push (the RFC 8291 test vector, VAPID checked by a fake push service, 410 removing a device), the alert state machine (coalescing, repeat, expiry, silence, resolution), who sees which alert, thresholds held over time, cgroup reading and which container deaths are vops' own, the proxy limiter (429 + Retry-After, burst, per ip, 413 on Content-Length and chunked, limits surviving a restart) and its hardening (upstream header timeout without cutting streams, stalled bodies, connection caps, `X-Forwarded-For` overwritten, a failed table write keeping the old file), a corrupt `secret.key` refused and left alone, the users rules in sqlite (last admin, cascades, sessions dropped) and the migration from the regex users, registry auth and login, every web route needing a session and the CSRF header, json body limits, backup archive checks (traversal, absolute paths, devices, symlinks, unknown volumes), the secrets backup (authorized_keys options and unsupported keys, no re-encryption without a change, no backup without a recipient, ssh keys with and without a passphrase, restore only setting missing keys); `FuzzSplitPath` and `FuzzLoad` fuzz the registry path splitter and the compose loader (`go test -fuzz`).
