# vops or versionated-ops

vops is a cli and a daemon to self-host containers with less pain and overhead than traditional operations: a git repo of compose files is the whole state of a server.

## State, development, lifecycle

**v1.5, in production on one real server.** Everything under "Features" is covered by end-to-end tests against real podman (rootless) and by `just vps-test` (systemd + real sshd + podman in a container: in-place upgrade from v1.2, a daemon SIGKILL and a rolling release under load, 0 failed requests). It runs a real VPS (Ubuntu 24.04, ext4, rootless with linger, real DNS and Let's Encrypt), migrated from docker compose + caddy + registry:2 + watchtower with ~1m20s of downtime ([reference/migrating.md](reference/migrating.md)), and survives reboots: linger starts the daemon, which brings every project back in dependency order (~7s for 6 containers).

- Host deps: linux, systemd, podman 4+ (netavark), git. btrfs (+ btrfs-progs) only for snapshots and data rollback.
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
- The repo holds `.secrets.age`: every `vops env` secret (production and previews), encrypted to the ssh keys that already reach the host, so a dead host doesn't mean setting them all again. You open it with your ssh key.
- Recipients: every ed25519/rsa key in the host's `~/.ssh/authorized_keys` (read live), root's keys (copied by `vops install` with passwordless sudo) and `secrets.recipients` in `vops.yml` (ssh or `age1…` keys). `vops env recipients` and the Environment tab list them, and the keys left out (ecdsa, security keys) with why.
- The host encrypts, your cli commits: `vops env set/rm` and `vops sync` fetch the latest and commit only `.secrets.age`, leaving what you staged alone (even past a `.gitignore` matching dotfiles). Dashboard changes land on the next sync; until then `vops status` and the dashboard say git is behind. Never a deploy; the file only changes when a secret or a key does.
- Restore: on a new host, `vops sync` offers `restore 4 secret(s) from .secrets.age? [Y/n]`; `vops env restore [file]` does it explicitly. Decryption is on your machine (`--ssh-key`/`vops-lock.yml` key, then `~/.ssh/id_*`), values go over ssh stdin, only missing keys are set. Sync warns when none of your keys can open it.

### Data safety: snapshots and rollback
- Every named volume and every bind mount inside the project dir is created as a btrfs subvolume, so snapshots are instant and copy-on-write, even for a 50GB database.
- Before any deploy that changes a project, its data is snapshotted (`pre-deploy`); containers using it are paused for the few milliseconds it takes, so all volumes are captured at the same instant.
- `vops rollback <project> [deploy-id]` goes back to right before a deploy (default: the last non-rollback one): the **images** that ran then and the **data** from just before it. It shows the plan per service (`web: shop/web:v2 (sha256:…) → shop/web:v1 (sha256:…)`, skipped ones and why, the snapshot, the git command if the code differs) and asks. `--images`/`--data` for one part, `--service web` for some images, `--snapshot id` restores any snapshot.
- Images roll back without git, by digest, with a rolling release. The service is then **pinned** (`web pinned to #12, compose says shop/web:latest`, nothing pending) until a newer version arrives (a push of its tag, or another `image:` in compose) or `vops unpin <project> [service]`. Built images (`build:`) and local images without a digest are skipped, and the plan says so.
- Data rollback stops the project, snapshots the current data (`pre-rollback`), restores and starts it again. Always the whole project, every volume at the same instant (a db back an hour with the uploads dir not is data that no longer matches). Compose and code are not touched.
- Every rollback can be undone: `vops rollback <project> <rollback-id>`.
- `vops snapshot ls|create|rm`, also in the dashboard. The newest 5 automatic snapshots per project are kept (`snapshot_keep`); manual and uploaded ones stay until deleted.
- Off-host backups: `vops snapshot export shop 12 > shop.tar.gz` (or Download in the dashboard) streams a snapshot as .tar.gz with owners, modes and xattrs. `vops snapshot import shop < shop.tar.gz` (or "Upload backup") makes it a snapshot again, restorable like any other. Imports are checked entry by entry (no absolute paths, `..`, devices, writes through symlinks); a failed one leaves nothing.
- Deploy history: every apply that changes a project (sync, apply, dashboard, registry push) and every rollback, with its commit, images (by digest), result and pre-deploy snapshot. `vops history <project>` or the Timeline tab.
- Timeline (dashboard): newest first, each point with commit, image changes (`web: v1 → v2`), result, snapshot and previews branched from it. "Roll back…" picks services and/or data; "Preview from here" previews that point (its commit, images by digest, a copy of its snapshot). After a rollback the project shows "Rolled back to before #N · Undo" until its next deploy; pinned services get a badge with Unpin.
- The registry keeps the images of the last `image_keep` (10) deploys of every project, of pins and of previews, even after their tag moved on. Older points show their image as gone.
- Works rootless (through `podman unshare`), no root and no special mount options.

### Previews (database branching)
- Opt-in per service: only a service with `x-vops.preview` can get a preview. It names what runs with it (`with`) and whose data is copied from production (`copy`, which runs too); nothing else runs, and only that data is copied:
  ```yaml
  services:
    web:
      x-vops: {preview: {with: [redis], copy: [db]}}   # `preview: {}` = just web
  ```
- `vops preview up shop --name pr-42` runs the declared services at `https://web.pr-42.shop.<domain>`: own containers, networks and volumes, the applied commit (a git worktree in `~/.vops/previews/`) and **a copy-on-write copy of the `copy` services' data**, O(1) even for 50GB. A destructive migration there leaves production untouched; other volumes start empty.
- `--image web=registry.example.com/shop/web:pr-42` another image, `--ref my-branch` another commit (pushed to the host first), `--from <snapshot id>` another data point. `vops preview ls [project]`, `vops preview rm shop pr-42`.
- From the registry: pushing `shop/web:preview-pr-42` creates or updates preview `pr-42` wherever a service running `shop/web` has `x-vops.preview`; `shop/api:preview-pr-42` joins the same preview. A `preview-*` tag never redeploys production.
- Env: never production's. `preview.env` from the project dir (committed, non-secret overrides like `SMTP_HOST=mailpit`) plus preview secrets: `vops env set shop --preview STRIPE_KEY`.
- Dashboard: a Previews tab per project, previews in the sidebar, "Preview from here" on the timeline.
- Guardrails: off the shared network, undeclared services (workers, crons) don't run, a `depends_on` outside the preview is an error, no host ports or extra domains. At most `preview_max: 5`; removed after `preview_ttl: 3d` without updates. `rm` leaves nothing behind.

### Audit log
- Every write to the host's database (env changes, users and their repos, logins, snapshots, projects and their limits) is recorded by sqlite triggers, so no code path can forget it. Secrets never land there. `vops audit`, or the Events page.

### Config
- Everything is in `vops.yml` at the repo root (`vops init` writes it with comments): domain, email, listeners, tls, snapshots, images kept, preview limits, who else can open `.secrets.age`. Changes show up in the plan and take effect on apply, like compose files.
- The host runs from its last applied copy (`~/.vops/config.yml`), so a broken `vops.yml` never breaks the daemon. New http/https/tls restart the proxy (sites blink), a new `ui` the daemon; containers keep running.

### Registry
- Own OCI registry at `registry.<domain>` (works with `podman push`/`docker push`, manifest lists, referrers).
- Users are admins or deployers. Admins log into the dashboard and push/pull every repo with their password (`vops user add ops --admin` prompts it). Deployers only use the registry, with a generated token: every repo (`--global`) or a list (`vops user add ci --repo shop/web --repo shop/api`). Push and pull are one permission. `vops user ls|token|password|rm`, or the Users page.
- Wrong credentials are limited per client ip, dashboard and registry together: after 10, `429` for 15 minutes (an `auth` event says who).
- Pushing a tag that a service runs redeploys it (rolling). No watchtower. Opt out with `x-vops.watch: false`. Tags like `preview-pr-42` create previews instead (see Previews).
- Garbage collection: dashboard button, `vops registry gc`, and daily. It keeps untagged images that pins, previews and the last `image_keep` deploys of each project run.

### Proxy
- Our own, in its own process (`vops-proxy.service`): :80/:443, Let's Encrypt, http→https, HSTS, routing to containers. It knows nothing about sqlite, podman or the registry.
- The daemon crashing, deadlocking (a watchdog restarts it) or being upgraded never takes the sites down; the dashboard and registry answer 502 until it's back. `vops install` restarts the proxy only when the proxy itself changed.
- The daemon sends the routing table on every change and waits for the swap (a switch the proxy never confirms fails and keeps the old replicas). The proxy keeps its last table on disk, so it restarts with the right routes even with the daemon down. `vops status` and the top bar show its state.
- Per-project limits (`x-vops: {rate: 20/s, burst: 40, max_body: 10MB, timeout: 5m}` at the compose top level): a token bucket per client ip (`429` + `Retry-After`), a body limit (`413`) and a response header timeout (default 60s, then `504`; streams like SSE are never cut), for every routed service and its previews. `vops status` and the project header show what they refused.
- Hardened defaults: 10s and 64KB for request headers, a body silent for 30s is dropped (`408`), at most 256 connections per client ip and 10000 total. `X-Forwarded-For` is always the client's address.

### Dashboard
- `vops ui` opens it through an ssh tunnel; also at `https://vops.<domain>` with an admin's user and password (session cookie). Deployers can't log in.
- Sidebar with every project and its health, overview with warnings, pending changes + apply from any page. Per project: services (restart, unpin), timeline, previews, logs (live, search, date range, download), env, events. Also registry images, users, git-tracked text files, events and audit log, Notifications, and a System page (cpu, memory, disk, network, PSI, live every 3s). Works on a phone.

### Notifications
- Web Push, no third-party service: Notifications page → "Enable notifications on this device" (and "Send test"), on `https://vops.<domain>` or through `vops ui`. iOS only pushes to installed web apps: Share → Add to Home Screen first.
- On by default: **deploy failed**, **restart** (a container died without vops stopping it), **out of memory** (containers and the host's OOM killer), **down** (a routed service with no replica, or failing its probe `down_after` times), **unhealthy** (`x-vops.health` 4xx/5xx or the compose healthcheck), **resources** (host cpu > 90% for 10m, memory available < 10% for 5m, a filesystem > 90%, memory PSI > 20%; containers near their limits). Off by default: **resolved**, **deploy succeeded**. What vops itself does never alerts.
- Sent at once, then hourly, expiring after 24h. Repeats coalesce ("shop/web restarted (5 times in 12m)"). Anyone can silence an alert for everyone; `vops notifications` lists them.
- Per user: which kinds. Global ("Advanced"): thresholds and intervals. Every user's devices are listed and removable. All in the host's database, not `vops.yml`. You only get alerts of projects you can see.

### Logs
- Containers log to journald; `vops logs <project> [service] -f --grep x` or the dashboard. History survives rollouts and search covers all of it.
- `--since`/`--until` take `2006-01-02`, `2006-01-02 15:04`, RFC3339 or a duration (`2h` ago).

## Limitations

Know these before putting something important on it:

- **Compose is a subset, run by vops itself** (not podman-compose): it translates each service into `podman run`. Unsupported keys are errors, never silently ignored. Not supported: `secrets`, `configs`, `extends`, `links`, `container_name`, `depends_on.restart`, `network_mode: service:x`, most of `deploy`. Full list: [reference/compose.md](reference/compose.md).
- **Semantics that differ from compose:** containers are named `vops-...` (so `podman compose ps` doesn't see them), `restart` defaults to `unless-stopped`, every `depends_on` waits for readiness, and during a rolling release two versions run side by side for a few seconds. Podman names come from the project dir, so dirs that only differ in case (`Shop`, `shop`), or a project `shop-api` next to `shop`'s network `api`, are refused by the plan.
- **One host.** No clustering, no failover; the daemon, proxy and registry run on the same machine as the containers. The proxy is a separate process, so a daemon crash or upgrade doesn't stop the sites, but the host (and the proxy restarting for new http/https/tls) still does.
- **Podman only, netavark only.** CNI setups can't resolve service names.
- **Env values are hidden from the api and dashboard, not from the host:** anyone with a shell on the host can see them with `podman inspect`.
- **Recreate means downtime:** services with published `ports`, without `x-vops.port`, or on a network whose definition changed are stopped before the new container starts.
- **Snapshots need btrfs** and cover only named volumes and bind mounts inside the project dir (not external volumes or absolute host paths). Data created before vops made it a subvolume (non-empty plain dirs) is left alone and shown as "not covered".
- **Snapshots are crash-consistent**, like pulling the plug: fine for postgres, mysql/innodb, sqlite, anything with a journal; not a replacement for application-level backups, and they live on the same disk: copying them off the host (`vops snapshot export`) is up to you, nothing is sent anywhere by itself.
- **Rollback restores images and data, not compose config or code.** Env, ports and the compose file stay as they are; built images (`build:`) need git to go back (the dashboard shows the command). Restoring data stops the project's containers while it runs (seconds); rolling back only images doesn't.
- **Previews hold production data**, and whatever the code does with it (queued emails, webhooks, scheduled charges) still goes out for real unless their env redirects it. The data keeps production's own db credentials. Copy only what you need, leave workers out, point SMTP and payment keys at test ones.
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

Tests are end to end on purpose: `e2e/` drives the real binary over a fake ssh (install, sync, registry push and auto redeploy, users and permissions, the dashboard api, backups, previews, rollback, the proxy as its own process under a rolling release and a daemon SIGKILL, version mismatches, the secrets backup, notifications), `internal/deploy` checks zero failed requests during rolling releases and that failures leave nothing half-done, and `just vps-test` upgrades a real systemd host in place. Unit tests cover the rest, including the web-facing parts: every route needs a session and the CSRF header, inputs are validated, wrong credentials get limited per client, file reads stay in the repo, registry auth and protocol edge cases, Web Push (RFC 8291 vector), the proxy's limits and hardening. `FuzzSplitPath` and `FuzzLoad` fuzz the registry path splitter and the compose loader (`go test -fuzz`).
