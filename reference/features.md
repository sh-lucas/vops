# Features

Full detail of every feature. The README has the short list.

## Git sync
- `vops sync`: `git pull` from the host, `git push`, plan, confirm, apply. `-y` for CI.
- Several people (or CI) can push; sync merges the host's commits first.
- Plain `git push vops` works too; the dashboard then shows "the host differs from git" with an apply button.
- Login: `vops-lock.yml` (gitignored, written by `install`) or `--host user@host --ssh-key path`. See [reference/ci.md](reference/ci.md) for GitHub Actions.

## Projects
- Any git-tracked dir with `compose.yml` (or `*.compose.yml`, `docker-compose.yml`) is a project. Nested dirs are nested projects: `shop/api` is served at `api.shop.<domain>`.
- vops reads compose itself (a strict subset: unknown keys are errors). Reference: [reference/compose.md](reference/compose.md).
- Networking like compose: services reach each other by name, custom and `internal` networks, aliases, static ips. On top of that, a shared network where any project reaches any other one at `<service>.<project reversed>` (e.g. `db.api.shop`), opt-out per service.
- `depends_on` conditions, including `service_completed_successfully` jobs (migrations) that gate the services depending on them; `profiles` via `COMPOSE_PROFILES`.
- Rolling releases for routed services: new replica, readiness check, switch traffic, drain, remove old. A failed readiness check, or a proxy that doesn't confirm the switch within 10s, keeps the old version serving. Everything else is recreated (stop, then start). Replicas that fail readiness are always removed, never left running.
- Env vars per project, write-only: set from the cli or the dashboard, used for `${VAR}` and `environment: [VAR]`, never shown again.
- What each service gets: `vops env ls <project>` and the Environment tab list, per service, every variable its containers get and where it comes from (`compose` literal, `env_file`, `vops` env directly or through `${VAR}`, or `missing`: referenced but set nowhere), plus vops keys no service uses. Names only, never values. A literal whose name looks secret is flagged "committed to git: move to vops env".
- Disable/enable a project without deleting it; removing its dir from git removes its containers (volumes and data dirs stay).

## Secrets backup in git
- The repo holds `.secrets.age`: every `vops env` secret (production and previews), encrypted to the ssh keys that already reach the host, so a dead host doesn't mean setting them all again. You open it with your ssh key.
- Recipients: every ed25519/rsa key in the host's `~/.ssh/authorized_keys` (read live), root's keys (copied by `vops install` with passwordless sudo) and `secrets.recipients` in `vops.yml` (ssh or `age1…` keys). `vops env recipients` and the Environment tab list them, and the keys left out (ecdsa, security keys) with why.
- The host encrypts, your cli commits: `vops env set/rm` and `vops sync` fetch the latest and commit only `.secrets.age`, leaving what you staged alone (even past a `.gitignore` matching dotfiles). Dashboard changes land on the next sync; until then `vops status` and the dashboard say git is behind. Never a deploy; the file only changes when a secret or a key does.
- Restore: on a new host, `vops sync` offers `restore 4 secret(s) from .secrets.age? [Y/n]`; `vops env restore [file]` does it explicitly. Decryption is on your machine (`--ssh-key`/`vops-lock.yml` key, then `~/.ssh/id_*`), values go over ssh stdin, only missing keys are set. Sync warns when none of your keys can open it.

## Data safety: snapshots and rollback
- Every named volume and every bind mount inside the project dir is created as a btrfs subvolume, so snapshots are instant and copy-on-write, even for a 50GB database.
- Before any deploy that changes a project, its data is snapshotted (`pre-deploy`); containers using it are paused for the few milliseconds it takes, so all volumes are captured at the same instant.
- `vops rollback <project> [deploy-id]` goes back to right before a deploy (default: the last non-rollback one): the **images** that ran then and the **data** from just before it. It shows the plan per service (`web: shop/web:v2 (sha256:…) → shop/web:v1 (sha256:…)`, skipped ones and why, the snapshot, the git command if the code differs) and asks. `--images`/`--data` for one part, `--service web` for some images, `--snapshot id` restores any snapshot.
- Images roll back without git, by digest, with a rolling release. The service is then **pinned** (`web pinned to #12, compose says shop/web:latest`, nothing pending) until a newer version arrives (a push of its tag, or another `image:` in compose) or `vops unpin <project> [service]`. Built images (`build:`) and local images without a digest are skipped, and the plan says so.
- Data rollback stops the project, snapshots the current data (`pre-rollback`), restores and starts it again. Always the whole project, every volume at the same instant (a db back an hour with the uploads dir not is data that no longer matches). Compose and code are not touched. If a restore fails part way and putting the data back fails too, the project stays stopped and held (boot and apply don't start it) until a good restore or `vops restart`. A daemon that dies mid-restore leaves it held the same way.
- Every rollback can be undone: `vops rollback <project> <rollback-id>`.
- `vops snapshot ls|create|rm`, also in the dashboard. The newest 5 automatic snapshots per project are kept (`snapshot_keep`); manual and uploaded ones stay until deleted.
- Off-host backups: `vops snapshot export shop 12 > shop.tar.gz` (or Download in the dashboard) streams a snapshot as .tar.gz with owners, modes and xattrs. `vops snapshot import shop < shop.tar.gz` (or "Upload backup") makes it a snapshot again, restorable like any other. Imports are checked entry by entry (no absolute paths, `..`, devices, writes through symlinks); a failed one leaves nothing.
- Deploy history: every apply that changes a project (sync, apply, dashboard, registry push) and every rollback, with its commit, images (by digest), result and pre-deploy snapshot. `vops history <project>` or the Timeline tab.
- Timeline (dashboard): newest first, each point with commit, image changes (`web: v1 → v2`), result, snapshot and previews branched from it. "Roll back…" picks services and/or data; "Preview from here" previews that point (its commit, images by digest, a copy of its snapshot). After a rollback the project shows "Rolled back to before #N · Undo" until its next deploy; pinned services get a badge with Unpin.
- The registry keeps the images of the last `image_keep` (10) deploys of every project, of pins and of previews, even after their tag moved on. Older points show their image as gone.
- Works rootless (through `podman unshare`), no root and no special mount options.

## Previews (database branching)
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

## Audit log
- Every write to the host's database (env changes, users and their repos, logins, snapshots, projects and their limits) is recorded by sqlite triggers, so no code path can forget it. Secrets never land there. `vops audit`, or the Events page.

## Config
- Everything is in `vops.yml` at the repo root (`vops init` writes it with comments): domain, email, listeners, tls, snapshots, images kept, preview limits, who else can open `.secrets.age`. Changes show up in the plan and take effect on apply, like compose files.
- The host runs from its last applied copy (`~/.vops/config.yml`), so a broken `vops.yml` never breaks the daemon. New http/https/tls restart the proxy (sites blink), a new `ui` the daemon; containers keep running.

## Registry
- Own OCI registry at `registry.<domain>` (works with `podman push`/`docker push`, manifest lists, referrers).
- Users are admins or deployers. Admins log into the dashboard and push/pull every repo with their password (`vops user add ops --admin` prompts it). Deployers only use the registry, with a generated token: every repo (`--global`) or a list (`vops user add ci --repo shop/web --repo shop/api`). Push and pull are one permission. `vops user ls|token|password|rm`, or the Users page.
- Pushes are refused (`507`) while the disk is over 95% used or has under 2GB free; the disk alert fires at 70%.
- Wrong credentials are limited per client ip (ipv6: per /64), dashboard and registry together: after 10, `429` for 15 minutes (an `auth` event says who).
- Pushing a tag that a service runs redeploys it (rolling). No watchtower. Opt out with `x-vops.watch: false`. Tags like `preview-pr-42` create previews instead (see Previews).
- Pushes record who pushed (in events and, for gated projects, in the release).

## Releases: approval and canaries
- Per project, at the compose top level: `x-vops: {release: manual}` (or `canary`, `canary 25%`; default `auto`, a push deploys).
- `manual`: a push waits for an admin. Every service running the tag (and jobs sharing the image) keeps its digest: `vops plan` and `status` show `web: waiting approval of #3 (…)`, not a pending change, and no apply, sync, unpin or rollback deploys it, not even one racing the push. The dashboard lists it on the overview and the project page with Approve now, Approve at a time (the minute tick runs it) and Reject; a "waiting approval" badge in the top bar; a push notification (`approval`, on by default). Approve deploys exactly that digest (rolling, pre-deploy snapshot, history row `approve`) and refuses if the tag moved. A newer push supersedes. Reject keeps what runs until a newer push. `vops releases [project]`, `vops releases approve|reject <id>`; releases show on the timeline and in `vops history`.
- `canary`: at push the jobs of the set run (migrations), each routed rolling service gets one canary container (whatever its replicas) with the new digest, taking that share of clients (10% by default), sticky by cookie; workers wait for Approve. "Open the canary" in the dashboard puts your browser on it on the real domain (`?vops-canary=on`, `off` to leave); the service row shows its age and its 5xx next to the stable's. Approve = the usual rolling release, then the canary goes; Reject removes it. A canary that fails readiness, crashes or restarts rejects its release and notifies. A compose change rebuilds the canary; a daemon or proxy restart keeps the split. Routed services with `strategy: recreate` (or a set with nothing routed) behave as `manual`.
- Garbage collection: dashboard button, `vops registry gc`, and daily. It keeps untagged images that pins, previews and the last `image_keep` deploys of each project run.

## Proxy
- Our own, in its own process (`vops-proxy.service`): :80/:443, Let's Encrypt, http→https, HSTS, routing to containers. It knows nothing about sqlite, podman or the registry.
- The daemon crashing, deadlocking (a watchdog restarts it) or being upgraded never takes the sites down; the dashboard and registry answer 502 until it's back. `vops install` restarts the proxy only when the proxy itself changed.
- The daemon sends the routing table on every change and waits for the swap (a switch the proxy never confirms fails and keeps the old replicas). The proxy keeps its last table on disk, so it restarts with the right routes even with the daemon down. `vops status` and the top bar show its state.
- A replica that refuses connections (stopped, removed) costs a retry on the next replica of its group, not a 502.
- Per-project limits (`x-vops: {rate: 20/s, burst: 40, max_body: 10MB, timeout: 5m}` at the compose top level): a token bucket per client ip (ipv6: per /64; `429` + `Retry-After`), a body limit (`413`) and a response header timeout (default 60s, then `504`; streams like SSE are never cut), for every routed service and its previews. `vops status` and the project header show what they refused.
- Hardened defaults: 10s and 64KB for request headers, a body silent for 30s is dropped (`408`), at most 256 connections per client ip (ipv6: per /64) and 10000 total. `X-Forwarded-For` is always the client's address.

## Dashboard
- `vops ui` opens it through an ssh tunnel; also at `https://vops.<domain>` with an admin's user and password (session cookie). Deployers can't log in.
- Sidebar with every project and its health, overview with warnings, pending changes + apply from any page. Per project: services (restart, unpin, canaries), pushes waiting for approval, timeline, previews, logs (live, search, date range, download), env, events. Also registry images, users, git-tracked text files, events and audit log, Notifications, and a System page (cpu, memory, disk, network, PSI, live every 3s). Works on a phone.

## Notifications
- Web Push, no third-party service: Notifications page → "Enable notifications on this device" (and "Send test"), on `https://vops.<domain>` or through `vops ui`. iOS only pushes to installed web apps: Share → Add to Home Screen first.
- On by default: **deploy failed** (and failed canaries), **waiting approval** (a gated push), **restart** (a container died without vops stopping it), **out of memory** (containers and the host's OOM killer), **down** (a routed service with no replica, or failing its probe `down_after` times), **unhealthy** (`x-vops.health` 4xx/5xx or the compose healthcheck), **resources** (host cpu > 90% for 10m, memory available < 10% for 5m, a filesystem > 70%, memory PSI > 20%; containers near their limits). Off by default: **resolved**, **deploy succeeded**. What vops itself does never alerts.
- Sent at once, then hourly, expiring after 24h. Repeats coalesce ("shop/web restarted (5 times in 12m)"). Anyone can silence an alert for everyone; `vops notifications` lists them.
- Per user: which kinds. Global ("Advanced"): thresholds and intervals. Every user's devices are listed and removable. All in the host's database, not `vops.yml`. You only get alerts of projects you can see.

## Logs
- Containers log to journald; `vops logs <project> [service] -f --grep x` or the dashboard. History survives rollouts and search covers all of it.
- `--since`/`--until` take `2006-01-02`, `2006-01-02 15:04`, RFC3339 or a duration (`2h` ago).
