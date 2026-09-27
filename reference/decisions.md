# Decisions

Every non-obvious choice made while building vops, with the reason. Newest last. If you change one, update it here.

## Shape

- **One binary, `vops`.** Same binary is the local CLI, the remote CLI and the daemon (`vops daemon`). Install = copy the binary over ssh. No scp, no rsync: `ssh host 'cat > file' < binary`.
- **Go deps: 3.** `modernc.org/sqlite` (pure Go, keeps the binary static so it can be copied anywhere), `go.yaml.in/yaml/v3` (compose files; writing a YAML parser is not worth it), `golang.org/x/crypto/acme/autocert` (Let's Encrypt). Everything else is stdlib.
- **Runtime deps on the host:** linux, systemd, podman (netavark backend), git. btrfs (+ btrfs-progs) enables snapshots and rollback; without it everything else works and the dashboard says why snapshots are off.
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
| `~/.vops/config.yml` | the config lock: copy of the last applied `vops.yml` |
| `~/.vops/snapshots/` | btrfs snapshots of project data |
| `~/.vops/previews/<slug>/` | a preview's git worktree (and its bind-mounted data) |
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
- Compose `networks` work like compose: no `networks` = project `default` + shared `vops`; with `networks` = exactly those. `vops` is a reserved key for the shared network, so a service can opt in explicitly and a db on an internal network stays invisible to other projects.
- Networks carry a `vops.nethash` label with the hash of their definition. A changed definition means remove the project's containers on it, recreate, redeploy (podman can't change a network in place). The default network created before networks were configurable has no hash; it is accepted as long as it is still a plain bridge, so upgrading doesn't restart everything.
- Service hashes include the hash of the networks they join, so a network change redeploys its services.

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
- Search is `journalctl --grep` (PCRE2 builds; the text is regex-escaped, case-insensitive): it reads backwards until n matches, so it covers the whole history without loading it. Without PCRE2, go filters the last 20k lines.
- Paging: every response carries `X-Vops-Before`, the journald cursor of its oldest line; `?before=<cursor>` returns the n lines before it (`journalctl -r --after-cursor`). The dashboard loads them when scrolled to the top. No header = start of logs. Following = read history to the end, then `-f --after-cursor=<newest>` (no gap, no "silence means caught up" guess).
- The dashboard keeps at most 5000 lines while following; older ones are dropped (Reload to browse history again), so a tab left open doesn't grow forever.
- The dashboard filters by time with `since`/`until` (unix seconds; a set "to" means a closed range, so no follow), shows the available range from `X-Vops-First`/`X-Vops-Last` next to the range loaded, and downloads what is in view as `<project>[-<service>]-YYYYMMDD-HHMM.txt`.
- Date filter: `?since`/`?until` (unix seconds) map straight to `journalctl --since=@n`/`--until=@n`; `until` closes the range so follow is skipped. `X-Vops-First`/`X-Vops-Last` (unix seconds, set only on the first page) give the whole journal's range for these services regardless of grep/since/until, so the UI can show "logs available from X to Y".
- Lines carry the host's local time as text (the cli runs on the host); `X-Vops-Offset` (host UTC offset, seconds) lets the dashboard rewrite them to the browser's zone, the same zone as its from/to inputs.
- DuckDB/parquet was proposed; rejected for now: it needs cgo and a ~30MB library, which breaks "static binary, few resources". Revisit if searching logs across months becomes a real need.

## Web UI

- Plain HTML + CSS + vanilla JS, embedded with `embed`. It uses the same JSON API as the socket.
- Still no build step, no libraries, no external fonts or CDNs: it works offline. Everything is rendered with `h()` (never `innerHTML`).
- Layout: fixed sidebar (nav + project tree with a status dot: green running, amber pending/partial, red failing/invalid, grey disabled), a top bar with the host's commit, domain and a "pending changes" badge that opens plan/apply from any page. Below 880px the sidebar is a drawer. Light/dark follow the system.
- Status is polled every 10s by the shell (sidebar, top bar); pages subscribe instead of polling on their own.
- Project page = header + tabs (`#/p/<path>/<tab>`: services, timeline, previews, logs, env, events). A tab is one entry in `TABS` in app.js. Old links (`#/p/<path>`, `#/p/<path>/data` → timeline, `#/logs/<path>?service=x`, which is the full-page log view) keep working.
- Streamed actions started from a dialog (restore, preview up) show their progress in a modal, like apply; closing it doesn't stop them.
- Destructive actions ask through an in-page `<dialog>` that states the consequences, not `confirm()`.
- Served on `ui` listen address (default `127.0.0.1:9984`) and on `vops.<domain>` over HTTPS. The default is loopback because plain HTTP with a password over the internet is bad; `vops ui` opens an ssh tunnel and the browser. Set `ui: ":9984"` in `vops.yml` to expose it.
- Sessions: random id in an `HttpOnly; SameSite=Strict` cookie, sha256 stored in sqlite. Mutating requests also need the `X-Vops: 1` header (blocks CSRF without tokens).
- Env values are write-only: the API has no way to read them back.
- The env view (`/api/env/usage`, `vops env ls`) reports names and sources only, never values, not even of compose literals: it is computed by the same `compose.Load` + env merge the plan uses (the interpolator records which `${VAR}`s each environment entry uses), so it can't drift from what podman gets. A compose literal whose key matches `PASSWORD|SECRET|TOKEN|KEY|PRIVATE` (case-insensitive) is flagged: it is committed to git; the fix is `environment: [KEY]` + `vops env set`. A key nothing references (`environment: [KEY]` or `${KEY}` anywhere in the files; `COMPOSE_PROFILES` counts as used) is shown as unused.

## Install

- `vops install user@host` checks arch/podman/git/systemd, uploads the binary and runs `vops setup` on the host, which: creates dirs, inits `~/vops`, writes the systemd unit, enables linger (non-root), starts the daemon and prints the admin password once.
- root → system unit `/etc/systemd/system/vops.service`; non-root → user unit, needs `net.ipv4.ip_unprivileged_port_start<=80` (setup tells you the sudo command if it isn't).
- `KillMode=process` so restarting the daemon never kills containers.

## depends_on, jobs, profiles

- vops always waits for readiness between services, so `service_started` behaves like `service_healthy`. Stricter than compose, never looser.
- `service_completed_successfully` turns the dependency into a job: ready = exit 0, default `restart: no` (an always-restarting job would loop), 10 min timeout. A finished job isn't rerun until its definition changes (compose `up` does the same); a failed one is retried on the next apply.
- When a service fails, its dependents are skipped in that apply instead of deployed against a broken dependency.
- Active profiles come from `COMPOSE_PROFILES` in the project env: the compose-standard variable, set the vops way. `depends_on.restart` is rejected: vops never restarts dependents behind your back.

## Config: vops.yml and its lock

- One config file, `vops.yml`, committed: domain, email, listeners, tls, snapshots, preview limits. It is desired state like the compose files: a change shows up in the plan (`~ ui: a -> b`) and takes effect on apply.
- The host keeps `~/.vops/config.yml`, a copy of the last applied `vops.yml` (the "lock"). The daemon always starts from the lock, never straight from the repo, so a broken or half-pushed `vops.yml` can't stop it: the plan warns "keeping the config in effect" and nothing changes.
- `vops install` sends the local `vops.yml` to the host as its first lock, so the daemon starts with the right ports and tls before the first sync.
- A hand edit of the lock is drift: the next plan shows it and apply puts `vops.yml` back.
- Listener changes (http, https, ui, tls, acme email) need new sockets: after apply the daemon re-execs itself (same pid, systemd doesn't notice; containers keep running, routes are rebuilt from labels). Everything else (domain, snapshots, preview limits) applies live.
- Parsing is strict: a typo in `vops.yml` is an error, not a silently ignored key.

## Snapshots and rollback

- btrfs subvolume snapshots, not copies or dumps: O(1), atomic per volume, copy-on-write, and database-agnostic. Anything with a journal (postgres, mysql/innodb, sqlite) recovers from a crash-consistent snapshot like from a power cut.
- Data dirs become subvolumes when vops creates them (empty volume `_data`, missing bind dir). A non-empty plain dir is never moved: it shows as "not covered".
- Rootless: files belong to subuids, and btrfs only lets the owner snapshot or remove a subvolume. Every btrfs op runs in `podman unshare` (the namespace podman itself uses). Deleting uses `property set ro false` + `rm -rf`, which unprivileged users may do since linux 4.18. No root, no `user_subvol_rm_allowed`.
- Snapshots live in `~/.vops/snapshots/<project>/<time>`, same filesystem as the data (btrfs can't snapshot across filesystems). Metadata in sqlite: `snapshots` (one moment) + `snapshot_volumes` (one row per volume).
- Pre-deploy snapshot whenever a project gets a create/update, of all its data, not only of the changed service: a migration job changes the db's data without redeploying the db. Running containers that use the data are paused (milliseconds) so all volumes are captured at the same instant. A failed pre-deploy snapshot aborts that project's deploy: deploying without the safety net must be a decision (`snapshots: off`).
- Data is found from both the containers' mounts (what runs) and compose (what should run), so a disabled project can still be snapshotted.
- Rollback stops the project's containers, snapshots the current data as `pre-rollback` (undo point), restores each volume (the live one is moved aside and put back if the restore fails), starts what was running. `rollback` without an id skips `pre-rollback` snapshots, so running it twice doesn't ping-pong.
- Retention: newest `snapshot_keep` (5) automatic snapshots per project; manual ones are never pruned.
- Rollback restores data only. Code rollback stays a git operation (revert + sync), so git remains the single source of truth.

## Deploy history and the timeline

- `deploys` table: one row per project per apply that changes it (any action other than none, or a plan error), and one per rollback. Written by the engine (`applyProject`, `Rollback`), so cli, dashboard and registry pushes are all covered; the caller only names the trigger (`sync`, `apply`, `ui`, `push`; `rollback` is set by the engine). A no-op apply records nothing. Previews have no rows: the previews table is their record.
- The row is written when the deploy ends (no "running" state that a crash would leave behind). Recording never fails a deploy.
- `snapshot_id` is always the data right before that event: the pre-deploy snapshot for a deploy, the pre-rollback (undo) snapshot for a rollback. One meaning, so every node's "Restore data" and "Preview from here" start from the same place: the moment before it happened. The alternative (a deploy owns the snapshot taken by the next one, "its data at the end") needs the next row to exist and makes rollbacks a special case.
- Images: what runs after the event, per service, read back from podman: a service whose containers carry the desired hash runs the desired image (the hash covers our registry's digest); otherwise (a failed update kept the old replicas) the previous row's entry is carried over. Digests: our registry's from the plan, pulled images' from `podman image inspect`, none for `localhost/` and built images (built ones are marked `built`: the commit rebuilds them).
- Own-registry images referenced by digest (`registry.<domain>/shop/web@sha256:…`, which is how a preview pins a past deploy's image) run the loopback pull ref directly: a digest can't be a local tag. So "Preview from here" gets exactly the bytes that ran, even after the tag moved.
- A rollback that restores the undo snapshot of an earlier rollback records `undoes` = that rollback. The project banner "Data restored to #N · Undo" shows while the project's newest row is a successful rollback that isn't an undo; the next deploy (or the undo) clears it. It comes from `last_deploy` in `/api/status`, so it shows on every tab and needs no extra polling.
- Retention: 5000 rows host-wide (a trigger, like events). The timeline shows the newest 100 deploys of a project; snapshots referenced by rows are shown on them, other snapshots (manual, or older pre-deploy/pre-rollback) are their own nodes. Pruned snapshots just disappear from their row ("no snapshot").
- Previews record `deploy_id`: the node they branched from (the node picked in the dashboard; for live data, the project's newest row at creation; 0 when made from a snapshot, which places them on the node showing that snapshot). The timeline shows them as chips there.
- The Data tab was folded into the timeline: coverage warnings on top, "Snapshot now" on the "now" node, delete on snapshot nodes. One place for "what happened to my data".
- Code rollback stays a git operation: the restore dialog shows the data's commit next to the running one and, when they differ, the exact commands (`git restore --source=<sha> --staged --worktree -- <project>/`, commit, `vops sync`). Git stays the single source of truth; a rollback that rewrote the host repo would diverge from every laptop.
- `/api/timeline?project=` returns everything the tab and `vops history` need in one call (rows, snapshots, image changes vs the previous row, preview chips, commit subjects via one `git log --no-walk`), so both render the same thing.

## SQL: sqlc, migrations, triggers

- sqlc (same config as golang-tmpl): SQL in `internal/store/queries.sql`, schema in `internal/store/migrations/*.sql`, generated Go committed in `internal/store/queries/`. The store package keeps only what SQL can't do (encryption, hashing, json repos) and the friendlier types. sqlc is a dev tool only: `go install` and the binary don't need it.
- Migrations run in name order, each once, in a transaction, tracked in `schema_migrations` (the golang-tmpl runner).
- The audit log is written by triggers on every table, so no code path can forget it. Triggers never copy secrets: env values, token hashes, the admin hash and session ids are left out (sessions show 8 chars of the id hash). Retention is a trigger too (20k rows audit, 5k events).
- `events` (human-readable, written by code) and `audit_log` (every db write, written by triggers) are separate on purpose: one says "deployed shop/api", the other "projects.commit changed".

## Small ones

- Nothing deploys without an explicit apply: env changes, enable/disable and plain `git push` only show up as "pending" (cli `status`, dashboard banner). The exception is the registry push trigger, which is the point of it.
- `apply` from the cli shows the plan and then applies the exact commit it showed (`--commit`), so a push in between can't sneak in.
- A plan error in one project (bad compose, image not pushed yet) fails that project only; the others still apply.
- Env values and the admin password travel over ssh stdin, never as command-line args (no `ps`/history leaks).
- The unix socket lives in `~/.vops/`, so the home path must be under ~100 chars (unix socket limit). Real homes are.
- Dashboard logins are serialized and a failed attempt costs 1s (plus pbkdf2 600k): slow for brute force, no lockout to abuse.
- Registry basic-auth results are cached for 5 minutes (pbkdf2 per blob request would be slow); changing users or the admin password clears the cache.
- HTTPS listens even before a domain exists (the domain arrives with the first sync); until then port 80 serves plain http.
- Every `podman.Run` has a timeout (2m + the `-t` grace period; 30m for pull/build): a hung podman (storage lock, dead mount) would otherwise hold the deploy lock forever and block every apply and restart. Log streams are exempt.
- The daemon crashing doesn't touch containers (`KillMode=process`, systemd restarts it in 2s, routes are rebuilt from labels), but the proxy lives in it: sites are down for those seconds.
- Tests share one isolated podman storage (`~/.cache/vops-test`) guarded by a file lock, so `go test ./...` runs packages in parallel safely.
- The daemon idles at ~16MB RSS.
- The systemd unit sets `HOME` explicitly (system units have none) and `StartLimitIntervalSec=0` (never give up restarting). `setup` waits 2s before trusting `is-active`.
- Containers get `--log-driver journald` explicitly when journald is running: some distros (Fedora's podman image) default to k8s-file, which loses logs of removed replicas.
- Removing a container counts as done when it is gone, even if podman errors cleaning up its network afterwards (seen with nested podman).
- Changing only readiness settings (`health`, `timeout`, `strategy`) doesn't recreate containers; they aren't part of the service hash.
- `just vps-test` is the production-like check: fake ssh in `go test` covers the logic fast, the vps container covers real sshd/systemd/root.

## Previews

- A preview is the synthetic project `<project>@<name>`, run by the same engine (labels, hashes, plan/apply, rolling releases). `@` can't be in a dir name, so it never clashes with a real project. Names: slug `shop--pr-42` for podman objects (a dir could produce that slug, so `preview up` refuses a name whose slug or dns name belongs to an existing project), domain `<service>.pr-42.shop.<domain>`, which autocert already handles.
- Previews are not in the plan: git doesn't describe them. `Plan` ignores containers of `@` projects (else they'd be "removed from git"); the `previews` table is their desired state (commit, image overrides). No pre-deploy snapshots of them, no `projects` row.
- Code: a detached git worktree of the host repo in `~/.vops/previews/<slug>`, at the project's applied commit (or `--ref`). It gives each preview its own bind paths, build contexts, `env_file`s and `preview.env` at that commit, with no copying. The commit is pinned: later pushes and syncs don't move it, `preview up --ref` does. `vops preview up --ref <branch>` pushes a local branch to the host first (never `main`), so `--ref` just works from the laptop.
- Data: copied once, at creation, as writable btrfs snapshots straight from the live subvolumes (the project's containers are paused for the milliseconds it takes, like a pre-deploy snapshot). No snapshot row is recorded, so previews never push pre-deploy snapshots out of retention. `--from <id>` copies a recorded snapshot instead. Updates keep the preview's data; to start over: `rm` + `up`. Volumes map by compose key (`vops-shop-db` → `vops-shop--pr-42-db`), binds by path inside the project dir (into the worktree). Without btrfs a preview starts empty.
- Env: two layers only, never production's env: `preview.env` in the project dir (committed, not secret: `SMTP_HOST=mailpit`), then preview secrets (`vops env set shop --preview KEY`) on top. The copied data still carries production's own credentials (db passwords): set what the preview needs with `--preview`. Preview secrets live in the `env` table under project `<project>@*`: same encryption, same write-only api, same audit, no new table. `COMPOSE_PROFILES` comes from these layers; `VOPS_PREVIEW` is set.
- Guardrails are defaults, not options: no shared `vops` network (a service asking for it gets the preview's default network), `x-vops.preview.skip` services don't run (dependencies on them are dropped), no published host ports (they'd clash with production), no `x-vops.domains` (they're production's), `name:` of volumes and networks ignored (it would be production's object), external volumes are an error. External networks and absolute bind mounts are kept (the plan warns about the latter): they are explicit infrastructure.
- Limits: `preview_max` (5) previews on the host, counted from the table; `preview_ttl` (3d) since the last `up` or push. Housekeeping checks every minute. Both live in `vops.yml` like everything else.
- Registry trigger: a top-level `x-vops.previews` tag pattern per project (default `preview-*`, `off`). A pushed tag matching it creates or updates the preview named by `*` in every project with a service running that repo; the image keeps its registry host and gets the pushed tag. Overrides accumulate, so `shop/web:preview-pr-42` then `shop/api:preview-pr-42` is one preview with both. A tag that matches the pattern is never a production redeploy, even if production runs that exact tag.
- `rm` removes containers (and their anonymous volumes), networks, volumes, built images and local tags of the overrides, then the worktree with its data, then the row. If a step fails the row stays, so a retry or the ttl finishes the job. Registry tags stay (they're the user's pushes).
- Removing a container also removes its anonymous volumes (`podman rm -v`), for previews and projects alike: an image's `VOLUME` (postgres has one) left one volume behind per removed job or replica, forever.
- Preview lifecycle events are recorded under the base project (kind `preview`), so `vops events shop` shows them; deploy lines use the preview path.
- Dashboard: Previews tab per project, previews as subtle rows under their project in the sidebar and a count on the overview. The "copy of production data" warning is shown once, on top of the tab, not per preview. The empty state explains the `preview-*` push with a command built from the project's own images. Preview logs are `/api/logs?project=shop@pr-42` (same labels and journald tag scheme as projects) on the full-page log view.
- The Environment tab switches between production and preview secrets (`?scope=previews`), with one line saying previews don't inherit production's env and read `preview.env` from the repo.

## Not done yet

- Re-pulling third-party tags (`postgres:16`) on a schedule.
- Daemon pulling the repo from somewhere else (GitHub → server).
