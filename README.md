# vops or versionated-ops

`vops` is a simple project. If it's not simple, then it's wrong.
vops is a cli and a daemon to self-host containers with less pain and overhead than traditional operations: a git repo of compose files is the whole state of a server.

## State, development, lifecycle

**v1.2, in production on one real server.** Everything below "Features" is covered by end-to-end tests against real podman (rootless) and by a production-like check (`just vps-test`: systemd + real sshd + podman in a container, rolling release under load with 0 failed requests). It also runs a real VPS (Ubuntu 24.04, ext4, rootless as a normal user with linger, real DNS and Let's Encrypt), migrated from docker compose + caddy + registry:2 + watchtower with about 1m20s of downtime: [reference/migrating.md](reference/migrating.md). It survived an `apt upgrade` + reboot: linger starts the daemon, which brings every project back in dependency order (~7s for 6 containers).

- Runtime deps on the host: linux, systemd, podman 4+ (netavark), git. btrfs (+ btrfs-progs) for snapshots and data rollback (image rollback works without it); everything else works without it.
- The daemon idles at ~16MB RSS. The binary is static, ~13MB.
- Why things are the way they are: [reference/decisions.md](reference/decisions.md).

## How it works

```
your laptop                                   the host
./ (git repo)  --- git push over ssh --->     ~/vops/ (working tree, updated on push)
  vops.yml                                      shop/api/compose.yml  -> containers
  shop/api/compose.yml                          registry/data/        -> your images
  vops-lock.yml (gitignored)                  ~/.vops/                -> sqlite, certs, binary, socket
```

`vops sync` pulls from the host, pushes to it, shows what will change and asks before applying. The daemon runs the containers with podman, routes `https://<service>.<project>.<domain>` to them, gets certificates from Let's Encrypt, serves a registry at `registry.<domain>` and a dashboard at `vops.<domain>`.

## Install

```sh
go install github.com/sh-lucas/vops/cmd/vops@latest   # go 1.27+, puts vops in $(go env GOPATH)/bin
```

`vops install` copies this same binary to the host, so it must match the host (linux, same arch). From a mac or for an arm64 host:

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
- Rolling releases for routed services: new replica, readiness check, switch traffic, drain, remove old. A failed readiness check keeps the old version serving. Everything else is recreated (stop, then start).
- Env vars per project, write-only: set from the cli or the dashboard, used for `${VAR}` and `environment: [VAR]`, never shown again.
- What each service gets: `vops env ls <project>` and the Environment tab list, per service, every variable its containers get and where it comes from (`compose` literal, `env_file`, `vops` env directly or through `${VAR}`, or `missing`: referenced but set nowhere), plus vops keys no service uses. Names only, never values. A literal whose name looks secret is flagged "committed to git: move to vops env".
- Disable/enable a project without deleting it; removing its dir from git removes its containers (volumes and data dirs stay).

### Data safety: snapshots and rollback
- Every named volume and every bind mount inside the project dir is created as a btrfs subvolume, so snapshots are instant and copy-on-write, even for a 50GB database.
- Before any deploy that changes a project, its data is snapshotted (`pre-deploy`); containers using it are paused for the few milliseconds it takes, so all volumes are captured at the same instant.
- `vops rollback <project> [deploy-id]` goes back to right before a deploy (default: the last one that isn't a rollback): the **images** that ran then and the **data** from just before it. It shows what it will do per service (`web: shop/web:v2 (sha256:…) → shop/web:v1 (sha256:…)`, skipped ones and why, the snapshot, the git command if the data's commit differs) and asks. `--images` or `--data` does only that part, `--service web` only some images, `--snapshot id` restores the data of any snapshot (manual ones too).
- Images roll back without git: each service runs the image it ran then, by digest, with a rolling release (no downtime). The service is **pinned**: `vops status`, `vops plan` and the dashboard show `web pinned to #12, compose says shop/web:latest`, and nothing is pending. A pin ends by itself when a newer version arrives (a push of the tag it runs, or another `image:` in compose; other compose changes keep it), or with `vops unpin <project> [service]`. Built images (`build:`) and local images without a digest can't roll back without the code: they are skipped and the plan says so.
- Data rollback stops the project, snapshots the current data (`pre-rollback`), restores, and starts it again (with the pinned images when both roll back). It is always of the whole project: every volume goes back to the same instant. There is no per-service restore on purpose: the db back an hour and the uploads dir not is data that no longer matches. Compose and code are not touched; the snapshot says which commit its data belongs to.
- Every rollback can be undone: `vops rollback <project> <rollback-id>` goes back to right before it, with the same parts.
- `vops snapshot ls|create|rm`, and the same in the dashboard. The newest 5 automatic snapshots per project are kept (`snapshot_keep` in `vops.yml`); manual ones stay until deleted.
- Deploy history: every apply that changes a project (sync, apply, dashboard, registry push) and every rollback is recorded with its commit, the images that ran (by digest when known), its result and the snapshot of the data right before it. `vops history <project>`, or the project's Timeline tab.
- Timeline (dashboard): newest first, "now" on top. Each point shows its commit and message, image changes (`web: v1 → v2`), result, snapshot and the previews branched from it. "Roll back…" opens a dialog with a checkbox per service (current → target image; unavailable ones disabled with the reason) and one for the data (and tells you the git command if the code differs); snapshot nodes restore data only. "Preview from here" opens a preview of that point: its commit, its images by digest, a copy of its snapshot. After a rollback the project shows "Rolled back to before #N (images, data) · Undo" until its next deploy; pinned services have a "pinned · #N" badge with Unpin, and the overview lists them as warnings.
- The registry keeps the images of the last `image_keep` (10) deploys of every project, of pins and of previews, even once their tag moved on, so rollback and "Preview from here" work for them. Older points show their image as gone.
- Works rootless (through `podman unshare`), no root and no special mount options.

### Previews (database branching)
- `vops preview up shop --name pr-42` runs a full copy of project `shop` at `https://web.pr-42.shop.<domain>`: its own containers, networks and volumes, the code of the applied commit (a git worktree in `~/.vops/previews/`), and **a copy-on-write copy of production's data**, made in O(1) even for a 50GB database. A destructive migration in the preview leaves production untouched.
- `--image web=registry.example.com/shop/web:pr-42` runs another image; `--ref my-branch` another commit (a local branch is pushed to the host first); `--from <snapshot id>` another data point. `vops preview ls [project]`, `vops preview rm shop pr-42`.
- From the registry: pushing `shop/web:preview-pr-42` creates or updates preview `pr-42` of the projects running `shop/web`, with that image. Pushing `shop/api:preview-pr-42` too lands in the same preview. A preview tag never redeploys production. The pattern is `x-vops: {previews: "preview-*"}` at the top of the compose file (default; `off` disables it).
- Env: previews never get production's env. They get `preview.env` from the project dir (committed, non-secret overrides like `SMTP_HOST=mailpit`) and preview secrets on top: `vops env set shop --preview STRIPE_KEY`.
- Dashboard: a Previews tab per project (urls, where it came from, overrides, expiry, logs, delete, new preview), previews in the sidebar under their project, and "Preview from here" on any timeline point.
- Guardrails by default: previews are off the shared network (they can't reach production, nothing reaches them), services with `x-vops.preview: {skip: true}` (workers, crons) don't run, no host ports, no extra domains. At most `preview_max: 5` at once; a preview not updated for `preview_ttl: 3d` is removed (both in `vops.yml`). `rm` leaves nothing behind: containers, networks, volumes, data, worktree.

### Audit log
- Every write to the host's database (env changes, users, logins, snapshots, projects) is recorded by sqlite triggers, so no code path can forget it. Secrets never land there. `vops audit`, or the Events page.

### Config
- Everything is in `vops.yml` at the repo root (`vops init` writes it with comments): domain, email, listeners, tls, snapshots, images kept, preview limits. Changes show up in the plan and take effect on apply, like compose files.
- The host keeps the last applied copy in `~/.vops/config.yml` and always runs from it, so a broken `vops.yml` never breaks the daemon. New listeners restart the daemon in place (containers keep running).

### Registry
- Own OCI registry at `registry.<domain>` (works with `podman push`/`docker push`, manifest lists, referrers).
- Users are simple: a name, a generated token, and a regex and/or a list of repos they may push/pull. `vops user add ci --pattern 'shop/.*'`.
- The dashboard admin pulls everything and pushes nothing.
- Pushing a tag that a service runs redeploys it (rolling). No watchtower. Opt out with `x-vops.watch: false`. Tags like `preview-pr-42` create previews instead (see Previews).
- Garbage collection: dashboard button, `vops registry gc`, and daily. It keeps untagged images that pins, previews and the last `image_keep` deploys of each project run.

### Dashboard
- `vops ui` opens it through an ssh tunnel; also at `https://vops.<domain>`.
- Sidebar with every project and its health, overview with stats and warnings, pending changes + apply from any page. Per project: services (restart, unpin), timeline (deploys, rollbacks, snapshots; roll back images and/or data, preview from a point), previews, logs (live, search, date range, download), env vars (production or previews), events. Also registry images and users, git-tracked files (text only), events and audit log. Works on a phone.

### Logs
- Containers log to journald; `vops logs <project> [service] -f --grep x` or the dashboard. History survives rollouts; search covers all of it, and the dashboard loads older lines as you scroll up.
- `--since`/`--until` filter by date (`2006-01-02`, `2006-01-02 15:04`, RFC3339, or a duration like `2h` meaning now minus it); the api reports the full available range so the UI knows how far back it can go.

## Limitations

Know these before putting something important on it:

- **Compose is a subset, run by vops itself** (not podman-compose): it translates each service into `podman run`. Unsupported keys are errors, never silently ignored. Not supported: `secrets`, `configs`, `extends`, `links`, `container_name`, `depends_on.restart`, `network_mode: service:x`, most of `deploy`. Full list: [reference/compose.md](reference/compose.md).
- **Semantics that differ from compose:** containers are named `vops-...` (so `podman compose ps` doesn't see them), `restart` defaults to `unless-stopped`, every `depends_on` waits for readiness, and during a rolling release two versions run side by side for a few seconds.
- **One host.** No clustering, no failover; the daemon, proxy and registry run on the same machine as the containers.
- **Podman only, netavark only.** CNI setups can't resolve service names.
- **Env values are hidden from the api and dashboard, not from the host:** anyone with a shell on the host can see them with `podman inspect`.
- **Recreate means downtime:** services with published `ports`, without `x-vops.port`, or on a network whose definition changed are stopped before the new container starts.
- **Snapshots need btrfs** and cover only named volumes and bind mounts inside the project dir (not external volumes or absolute host paths). Data created before vops made it a subvolume (non-empty plain dirs) is left alone and shown as "not covered".
- **Snapshots are crash-consistent**, like pulling the plug: fine for postgres, mysql/innodb, sqlite, anything with a journal; not a replacement for application-level backups, and they live on the same disk (no off-host copy yet).
- **Rollback restores images and data, not compose config or code.** Env, ports and the compose file stay as they are; built images (`build:`) need git to go back (the dashboard shows the command). Restoring data stops the project's containers while it runs (seconds); rolling back only images doesn't.
- **Previews hold production data.** Personal data, and whatever the code does with it: a queue of real emails, webhooks, a scheduled charge. Their env is not production's, but the data keeps production's own credentials (db users and passwords), and anything the preview's env doesn't redirect still goes out for real. Skip workers and crons, point SMTP and payment keys at test ones.
- **Previews need btrfs for data** (without it they start empty) and can't be made of projects with external volumes or fixed-subnet networks. Absolute bind mounts and external networks are shared with production. Registry tags pushed for previews stay until `vops registry rm`.
- **Logs are journald's:** retention and disk use follow its config (`/etc/systemd/journald.conf`); no long-term log store.
- **Third-party images are not re-pulled** on their own: `postgres:16` stays at the version first pulled until you change the tag.

## Commands

```
vops init | install user@host | sync [-y] | ui
vops status | plan | apply [-y] [project...]
vops logs <project> [service] [-f] [-n N] [--grep s] [--since t] [--until t]
vops restart <project> [service] | enable <project> | disable <project>
vops env ls|set|rm <project> [--preview] ...   vops user ls|add|rm|token ...
vops registry ls|rm|gc                    vops admin password | events | audit | version
vops snapshot ls|create|rm ...            vops history <project> [-n N]
vops rollback <project> [deploy-id] [--images] [--data] [--service s]... [--snapshot id] [-y]
vops unpin <project> [service...]
vops preview up <project> --name n [--image svc=ref]... [--ref r] [--from id]
vops preview ls [project] | rm <project> <name>
```

Inside a linked repo commands run on the host over ssh; on the host they talk to the daemon directly.

## Development

```sh
just test       # everything, needs podman; uses an isolated podman storage in ~/.cache/vops-test
just build      # ./vops for this machine
just release    # dist/vops-linux-{amd64,arm64}
just gen        # sqlc generate (after editing internal/store/queries.sql or migrations/)
just vps-test   # systemd + real sshd + podman in a container as the host (slow, needs network)
```

Tests are end to end on purpose: `e2e/` drives the real binary (install, sync over a fake ssh, registry push, auto redeploy, a second developer, the dashboard api, previews from registry tags, a push recorded in the history, a preview of a past deploy by digest, an image rollback with its pin and unpin), `internal/deploy` checks zero failed requests during a rolling release, runs a destructive migration in a preview of a real postgres, walks the deploy history (deploys, a failed one, rollback and undo, a preview from a past deploy) and rolls images back through its own registry (zero failed requests, pins ending on a push or a compose change, images and data together, gc keeping what rollback needs), `internal/registry` pushes and pulls with real podman.
