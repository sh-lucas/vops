# Decisions

Every non-obvious choice made while building vops, with the reason. Newest last. If you change one, update it here.

## Shape

- **One binary, `vops`.** Same binary is the local CLI, the remote CLI and the daemon (`vops daemon`). Install = copy the binary over ssh. No scp, no rsync: `ssh host 'cat > file' < binary`.
- **Go deps: 3.** `modernc.org/sqlite` (pure Go, keeps the binary static so it can be copied anywhere), `go.yaml.in/yaml/v3` (compose files; writing a YAML parser is not worth it), `golang.org/x/crypto/acme/autocert` (Let's Encrypt). Everything else is stdlib.
- **Runtime deps on the host:** linux, systemd, podman (netavark backend), git. btrfs is not required yet (see "Not done yet").
- **The daemon is the reverse proxy.** No Caddy/Traefik/nginx. Rolling releases need to flip traffic the moment a new replica is ready; doing that in-process is a map swap instead of a config reload. TLS comes from autocert (HTTP-01 + TLS-ALPN-01).
- **Local CLI talks to the host through ssh only.** `vops status` locally runs `ssh host ~/.vops/bin/vops status` there; the remote vops talks to the daemon over a unix socket (`~/.vops/vops.sock`). ssh is the auth. There is no public API besides the web UI.

## Paths on the host

| path | what |
|---|---|
| `~/vops/` | the git repo (working tree), pushed to over ssh |
| `~/vops/registry/data/` | registry blobs (gitignored) |
| `~/vops/<project>/compose.yml` | a project |
| `~/.vops/bin/vops` | the binary (absolute path, so non-interactive ssh `PATH` doesn't matter) |
| `~/.vops/vops.db` | sqlite: env vars, users, sessions, events, project flags |
| `~/.vops/config.yml` | host-only settings (listen addresses, tls) |
| `~/.vops/secret.key` | AES key for env values at rest |
| `~/.vops/certs/` | ACME cache |
| `~/.vops/vops.sock` | daemon socket |

## Git sync

- `~/vops` is a normal (non-bare) repo with `receive.denyCurrentBranch=updateInstead`. A `git push` updates the working tree directly, no hooks, no checkout step. It refuses if someone edited tracked files on the server, which is what we want.
- `vops sync` = `git pull vops main` → `git push vops HEAD:main` → remote `vops plan` → show → confirm → remote `vops apply --commit <sha>`. Apply refuses if the remote HEAD moved since the plan.
- Plain `git push vops` also works; nothing is deployed until `vops sync`, `vops apply` or the "apply" button in the UI (which shows the pending plan first).
- `vops-lock.yml` (gitignored) holds `host`, `port`, `ssh_key`. Flags `--host`/`--ssh-key` override it, so CI needs no file.

## Projects & compose

- A project is any git-tracked directory containing `compose.yml`, `compose.yaml`, `*.compose.yml` or `docker-compose.yml`. Several compose files in one dir are merged (duplicate service = error). Nested dirs are separate projects.
- vops parses compose itself and calls `podman run`. No podman-compose/docker-compose: they can't do rolling releases and are one more dependency. Unsupported keys are an **error**, not silently ignored.
- Project `a/b` gets domain `b.a.<domain>`. A routed service gets `<service>.b.a.<domain>`; if it is the only routed service of the project it also gets `b.a.<domain>`. Extra domains via `x-vops.domains`.
- Routing is opt-in per service with `x-vops.port` (the HTTP port inside the container).
- Default `restart: unless-stopped` (compose default is `no`, which is a bad default for a server).
- Interpolation (`${VAR}`, `${VAR:-x}`, `${VAR:?err}`, `$$`) uses the project env vars stored by vops, not the shell. `environment: [KEY]` passes the project var through.

## Networking

- Each project has network `vops-<project>`; services resolve each other by service name (compose behaviour).
- Every container also joins the shared network `vops` with alias `<service>.<project reversed>` (e.g. `db.sub.proj`), so any project can reach any other one by a predictable name.
- The proxy reaches containers through a port published on `127.0.0.1:<port>`; vops picks the port (stable across restarts). Works the same rootless or rootful.
- netavark is required (aliases + DNS). CNI setups without dnsname don't resolve names.

## Deploys

- Desired state = git + env vars. Actual state = podman containers with `vops.*` labels. The db is not the source of truth for containers; the daemon rebuilds its routing table from podman on start.
- Each service has a hash of everything that defines it (resolved args, image, own-registry digest, build context tree hash). Different hash = redeploy.
- Strategy: `rolling` for routed services (new replicas → readiness → switch traffic → drain 2s → stop old), `recreate` for everything else (two postgres on the same volume is how you lose data). Override with `x-vops.strategy`.
- Readiness: compose `healthcheck` (vops runs `podman healthcheck run` itself, so it works without systemd timers) → else `x-vops.health` HTTP path → else TCP connect on the routed port → else "still running after 2s". Failed readiness removes the new replicas and keeps the old ones.
- `build:` builds on the host, tagged with the git tree hash of the context, so it only rebuilds when the context changes.

## Registry (own implementation)

- OCI distribution spec, stdlib only, filesystem storage: `blobs/sha256/<ab>/<digest>`, `repos/<name>/tags/<tag>` (file with the digest), `repos/<name>/manifests/<digest>` (empty marker file), `uploads/<uuid>`.
- Auth is HTTP Basic over HTTPS. Registry users get a random token as password (stored as sha256; high-entropy tokens don't need slow hashing). Each user has a regex and/or a list of repo names; they can push and pull those.
- The dashboard admin can pull everything and push nothing.
- The daemon pulls its own images through the loopback UI listener with an in-memory token, then tags them with the public name. No DNS/TLS needed for the server to deploy its own images.
- Watchtower replacement: pushing a tag that a service uses redeploys that service (rolling). Opt out with `x-vops.watch: false`.
- GC is mark & sweep: untagged manifests (not referenced by a tagged index or as the subject of a kept one) and unreferenced blobs go; anything younger than 1h is kept so pushes in progress are safe. Runs daily, from `vops registry gc` and from the dashboard. Uploads older than 24h are dropped.

## Logs

- Containers use podman's `journald` log driver with tag `vops.<project>.<service>`. journald already does aggregation, rotation, compression, indexing and history across replicas. Reading = `journalctl`, falling back to `podman logs`.
- DuckDB/parquet was proposed; rejected for now: it needs cgo and a ~30MB library, which breaks "static binary, few resources". Revisit if searching logs across months becomes a real need.

## Web UI

- Plain HTML + CSS + vanilla JS, embedded with `embed`. It uses the same JSON API as the socket.
- Served on `ui` listen address (default `127.0.0.1:9984`) and on `vops.<domain>` over HTTPS. The default is loopback because plain HTTP with a password over the internet is bad; `vops ui` opens an ssh tunnel and the browser. Set `ui: ":9984"` in `~/.vops/config.yml` to expose it.
- Sessions: random id in an `HttpOnly; SameSite=Strict` cookie, sha256 stored in sqlite. Mutating requests also need the `X-Vops: 1` header (blocks CSRF without tokens).
- Env values are write-only: the API has no way to read them back.

## Install

- `vops install user@host` checks arch/podman/git/systemd, uploads the binary and runs `vops setup` on the host, which: creates dirs, inits `~/vops`, writes the systemd unit, enables linger (non-root), starts the daemon and prints the admin password once.
- root → system unit `/etc/systemd/system/vops.service`; non-root → user unit, needs `net.ipv4.ip_unprivileged_port_start<=80` (setup tells you the sudo command if it isn't).
- `KillMode=process` so restarting the daemon never kills containers.

## Small ones

- Nothing deploys without an explicit apply: env changes, enable/disable and plain `git push` only show up as "pending" (cli `status`, dashboard banner). The exception is the registry push trigger, which is the point of it.
- `apply` from the cli shows the plan and then applies the exact commit it showed (`--commit`), so a push in between can't sneak in.
- A plan error in one project (bad compose, image not pushed yet) fails that project only; the others still apply.
- Env values and the admin password travel over ssh stdin, never as command-line args (no `ps`/history leaks).
- The unix socket lives in `~/.vops/`, so the home path must be under ~100 chars (unix socket limit). Real homes are.
- Dashboard logins are serialized and a failed attempt costs 1s (plus pbkdf2 600k): slow for brute force, no lockout to abuse.
- Registry basic-auth results are cached for 5 minutes (pbkdf2 per blob request would be slow); changing users or the admin password clears the cache.
- HTTPS listens even before a domain exists (the domain arrives with the first sync); until then port 80 serves plain http.
- Tests share one isolated podman storage (`~/.cache/vops-test`) guarded by a file lock, so `go test ./...` runs packages in parallel safely.
- The daemon idles at ~16MB RSS.
- The systemd unit sets `HOME` explicitly (system units have none) and `StartLimitIntervalSec=0` (never give up restarting). `setup` waits 2s before trusting `is-active`.
- Containers get `--log-driver journald` explicitly when journald is running: some distros (Fedora's podman image) default to k8s-file, which loses logs of removed replicas.
- Removing a container counts as done when it is gone, even if podman errors cleaning up its network afterwards (seen with nested podman).
- Changing only readiness settings (`health`, `timeout`, `strategy`) doesn't recreate containers; they aren't part of the service hash.
- `just vps-test` is the production-like check: fake ssh in `go test` covers the logic fast, the vps container covers real sshd/systemd/root.

## Not done yet

- btrfs snapshots of `data/` dirs before deploys (the reason btrfs is on the list).
- Re-pulling third-party tags (`postgres:16`) on a schedule.
- Daemon pulling the repo from somewhere else (GitHub → server).
