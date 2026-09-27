# vops or versionated-ops

`vops` is a simple project. If it's not simple, then it's wrong.
vops is a cli and a daemon to self-host containers with less pain and overhead than traditional operations: a git repo of compose files is the whole state of a server.

## State, development, lifecycle

**v0.3, in production on one real server.** Everything below "Features" is covered by end-to-end tests against real podman (rootless) and by a production-like check (`just vps-test`: systemd + real sshd + podman in a container, rolling release under load with 0 failed requests). It also runs a real VPS (Ubuntu 24.04, ext4, rootless as a normal user with linger, real DNS and Let's Encrypt), migrated from docker compose + caddy + registry:2 + watchtower with about 1m20s of downtime: [reference/migrating.md](reference/migrating.md).

- Runtime deps on the host: linux, systemd, podman 4+ (netavark), git. btrfs (+ btrfs-progs) for snapshots and rollback; everything else works without it.
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
- Disable/enable a project without deleting it; removing its dir from git removes its containers (volumes and data dirs stay).

### Data safety: snapshots and rollback (btrfs)
- Every named volume and every bind mount inside the project dir is created as a btrfs subvolume, so snapshots are instant and copy-on-write, even for a 50GB database.
- Before any deploy that changes a project, its data is snapshotted (`pre-deploy`); containers using it are paused for the few milliseconds it takes, so all volumes are captured at the same instant.
- `vops rollback <project> [id]` stops the project, snapshots the current data (`pre-rollback`, so the rollback itself can be undone), restores, starts again. Code is not touched; the snapshot says which commit its data belongs to.
- `vops snapshot ls|create|rm`, and the same in the dashboard. The newest 5 automatic snapshots per project are kept (`snapshot_keep` in `vops.yml`); manual ones stay until deleted.
- Works rootless (through `podman unshare`), no root and no special mount options.

### Audit log
- Every write to the host's database (env changes, users, logins, snapshots, projects) is recorded by sqlite triggers, so no code path can forget it. Secrets never land there. `vops audit`, or the Events page.

### Config
- Everything is in `vops.yml` at the repo root (`vops init` writes it with comments): domain, email, listeners, tls, snapshots. Changes show up in the plan and take effect on apply, like compose files.
- The host keeps the last applied copy in `~/.vops/config.yml` and always runs from it, so a broken `vops.yml` never breaks the daemon. New listeners restart the daemon in place (containers keep running).

### Registry
- Own OCI registry at `registry.<domain>` (works with `podman push`/`docker push`, manifest lists, referrers).
- Users are simple: a name, a generated token, and a regex and/or a list of repos they may push/pull. `vops user add ci --pattern 'shop/.*'`.
- The dashboard admin pulls everything and pushes nothing.
- Pushing a tag that a service runs redeploys it (rolling). No watchtower. Opt out with `x-vops.watch: false`.
- Garbage collection: dashboard button, `vops registry gc`, and daily.

### Dashboard
- `vops ui` opens it through an ssh tunnel; also at `https://vops.<domain>`.
- Tree of projects/services/containers, pending changes + apply, logs (live, filter), restart, enable/disable, env vars, registry images and users, git-tracked files (text only), events.

### Logs
- Containers log to journald; `vops logs <project> [service] -f --grep x` or the dashboard. History survives rollouts; search covers all of it, and the dashboard loads older lines as you scroll up.

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
- **Rollback restores data, not code**, and stops the project's containers while it runs (seconds).
- **Logs are journald's:** retention and disk use follow its config (`/etc/systemd/journald.conf`); no long-term log store.
- **Third-party images are not re-pulled** on their own: `postgres:16` stays at the version first pulled until you change the tag.
- **Not yet run on a real cloud VPS** (see State above).

## Commands

```
vops init | install user@host | sync [-y] | ui
vops status | plan | apply [-y] [project...]
vops logs <project> [service] [-f] [-n N] [--grep s]
vops restart <project> [service] | enable <project> | disable <project>
vops env ls|set|rm <project> ...         vops user ls|add|rm|token ...
vops registry ls|rm|gc                    vops admin password | events | audit | version
vops snapshot ls|create|rm ...            vops rollback <project> [id] [-y]
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

Tests are end to end on purpose: `e2e/` drives the real binary (install, sync over a fake ssh, registry push, auto redeploy, a second developer, the dashboard api), `internal/deploy` checks zero failed requests during a rolling release, `internal/registry` pushes and pulls with real podman.
