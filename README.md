# vops or versionated-ops

vops is a cli and a daemon to self-host containers with less pain and overhead than traditional operations: a git repo of compose files is the whole state of a server.

## State, development, lifecycle

**In production on one real server**, covered by end-to-end tests against real podman and by `just vps-test` (real systemd + sshd). Details: [reference/migrating.md](reference/migrating.md).

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

`vops install` copies this same binary to the host, so it must match the host (linux, same arch). Running it again upgrades the host in place. The cli and the host must run the same major.minor; patch versions may differ. From a mac or for an arm64 host:

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

Full detail: [reference/features.md](reference/features.md).

- **Git sync:** `vops sync` pulls, pushes, shows the plan and asks before applying. Plain `git push` works too.
- **Projects:** any dir with a `compose.yml` is a project; nested dirs are nested projects (`shop/api` is `api.shop.<domain>`). Strict compose subset, run by vops itself.
- **Rolling releases:** new replica, readiness check, switch, drain. A failed check keeps the old version serving.
- **Env vars and secrets:** write-only; backed up encrypted in git (`.secrets.age`) to your ssh keys.
- **Snapshots and rollback:** btrfs, copy-on-write. `vops rollback` restores images and data from before a deploy.
- **Previews:** per-branch copies of a service with a copy-on-write copy of production data.
- **Registry:** own OCI registry; pushing a watched tag redeploys it. Admins and deployers, per-repo tokens.
- **Releases:** `release: manual` waits for approval; `canary` sends a share of clients to the new version first.
- **Proxy:** own process, Let's Encrypt, rate and body limits. Sites keep serving when the daemon restarts.
- **Dashboard** (`vops ui`), **web push notifications**, **logs** (journald), **audit log**, and **config** in `vops.yml`.

## Limitations

Main ones (full list: [reference/limitations.md](reference/limitations.md)):

- Compose is a subset: unsupported keys are errors.
- One host, no clustering.
- Podman and netavark only.
- Snapshots, and previews' data, need btrfs. Snapshots are crash-consistent and live on the same disk.
- Rollback restores images and data, not compose config or code.
- Env values are hidden from the api and dashboard, not from the host.

## Deploying an application

Step by step: [reference/deploying.md](reference/deploying.md). CI examples: [reference/ci.md](reference/ci.md).

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
vops releases [project] | releases approve|reject <id>
vops preview up <project> --name n [--image svc=ref]... [--ref r] [--from id]
vops preview ls [project] | rm <project> <name>
```

Inside a linked repo commands run on the host over ssh (`sync`, `apply` and `install` only work that way); on the host they talk to the daemon directly. The web-facing parts are the dashboard (admins) and the registry (admins' passwords, deployers' tokens), see Dashboard and Registry. On the host, systemd runs `vops daemon` and `vops proxy`.

## Development

`just test` runs everything (needs podman). More: [reference/development.md](reference/development.md).
