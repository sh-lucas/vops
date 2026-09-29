# TODO

Ideas with a design sketch. Done things move to README/reference; decisions to reference/decisions.md.

## Security and abuse (next)

Facts today: sqlite is in WAL (`store.go`); all SQL is sqlc (parameterized); registry patterns are anchored RE2 (no ReDoS); sessions are HttpOnly/SameSite=Strict + `X-Vops` header; env is write-only.

- Login throttle is a global mutex + 1s sleep: an attacker's failed attempts queue in front of the real admin (lockout by spam) and nothing is per IP. Replace with per-IP backoff (token bucket, in memory) and keep the failed-login event. Same for registry basic auth failures.
- Separate the dashboard admin from registry credentials: today `admin` is both the dashboard user and a registry user (pull all), and registry users have no dashboard access. Proposal: the registry never accepts the dashboard password; the daemon's own loopback pull keeps its in-memory token; `vops registry user add` gives pull-only and push tokens (`--pull-only`), each with pattern/repos. Optional: several dashboard users with roles (viewer/deployer/admin) and per-project scopes.
- Tests to add (missing today): `UserAllows` (pattern anchoring, `a|b`, exact repos, invalid patterns), `ValidName` traversal cases (`../`, `%2e`, `blobs` as a path segment), login (wrong password, throttle, expired/deleted session, logout), every mutating route without the header / without a session (walk the mux and assert 401/403), sqlite injection strings through project/env/user names, oversized/malformed JSON bodies, `authCache` revocation (a deleted user's token works up to 5 min: clear the cache on user change).
- Fuzz the registry path splitter and the compose loader.
- Session cleanup: expired sessions are only checked, not purged (housekeeping tick).
- Audit `X-Forwarded-*` handling before adding anything per-IP (only trust it when the proxy sits behind a known CDN).

## Proxy toggles

Small in the proxy (stdlib, no dependency); the real question is where the setting lives, because `vops.yml`/compose are desired state and a dashboard-only toggle would be drift.

- Decision to make: settings in git (`vops.yml` global, `x-vops` per project/service, shown in the dashboard read-only) vs. dashboard-editable stored in sqlite and sent in the routing table (`PUT /routes`, so the daemon stays the source of truth and the proxy keeps it on disk). Recommendation: routes carry per-route options from compose `x-vops` (git), and a dashboard "override" table for emergencies (block an IP, lower a limit) with an expiry, shown in the plan like pins.
- Rate limit per IP: in-memory token bucket per (route, ip), `429` + `Retry-After`; options `rate: 20/s`, `burst`. Exempt the registry and dashboard hosts (dashboard gets its own strict limiter on `/api/login`).
- Payload limit: `max_body: 10MB` per route via `http.MaxBytesReader` (`413`); registry stays unlimited (image pushes).
- Other toggles worth having: request/response header timeouts per route, max concurrent connections per IP, IP allow/deny list, basic-auth in front of a route (staging/previews), `x-vops.redirect` www→apex, security headers (HSTS is on).
- Bump `proxy.Version` when it ships. Show active limits and 429/413 counters in `GET /status` and the dashboard.

## Releases

- Manual approval per project: `x-vops.approve: true` (or a repo pattern). A push that would redeploy records a pending release (repo, tag, digest, who pushed) instead of deploying; the dashboard shows Approve/Reject (reject = untag/ignore); approve runs the same apply as `onPush` does today. Needs a `pending_releases` table + a badge like "pending changes".
- Notifications: events already exist (`push`, deploy, failed deploy, auth, preview). Add outbound webhooks (generic JSON, plus ntfy/Discord/Slack formats) configured in `vops.yml` (`notify: [{url, on: [deploy_failed, push, approval]}]`; the URL is a secret, so it lives in env). Browser notifications: the dashboard is HTTPS at `vops.<domain>`, so the Notification API works while a tab is open; real Web Push (service worker + VAPID, stored subscriptions) is a second step.
- Tag semantics to document (they already work this way): a service on `myapp:v2.1.8` only redeploys when `v2.1.8` itself is pushed again (mutable tag = new digest = redeploy); pushing `latest` does nothing to it. Consider `x-vops.watch: digest-only`/`false` for "never redeploy from a push", and a warning when a push overwrites an existing non-`latest` tag.
- Rollback to test on a real host (no btrfs and with btrfs): images-only rollback needs no btrfs; check the dashboard flow on a real deploy and write the result in README.

## Data

Done: btrfs subvolumes, pre-deploy snapshots, `vops rollback` (images by pinning, and data), previews (`vops preview`, previews from registry tags, `--from` snapshot), deploy history with the dashboard timeline and previews tab.

### Previews, next

- Reset a preview's data without `rm`: `preview up --from <id>` (or `--from live`) on an existing preview (today it errors and asks for `rm` first); stop it, restore like rollback, start.
- Delete the registry tags pushed for a preview when it's removed (today they stay until `vops registry rm`): only tags matching the project's `x-vops.previews` pattern.
- Remove a preview when its branch is deleted or its PR merged: a `vops preview rm` from CI is enough today; maybe a push of an empty/special tag.
- Per-preview ttl (`--ttl 12h`) and a "keep" flag for long-lived staging-like previews.
- Previews of projects with fixed-subnet networks: rewrite or drop `ipam` in previews.

### Smaller data items

- Opt-out per project for pre-deploy snapshots (top-level `x-vops: {snapshots: false}` in compose; today it's host-wide in `vops.yml`).
- `vops snapshot diff <id>`: `btrfs subvolume find-new` / sizes, to see how much a snapshot holds exclusively.
- Show snapshot disk usage (needs quotas or `btrfs filesystem du`, which is slow; maybe only on demand).
- Off-host backups: `btrfs send` of a snapshot to a file/ssh target (`vops snapshot export <id> > file`), incremental against the previous one.
- Tag built images per commit (`localhost/vops/<project>-<service>:<commit>`, keep the last N) so built services can roll back too (today image rollback skips them: "can't roll back without the code").

## Other

- Proxy: a listener change (http/https/tls) re-execs it, so sites blink for a moment; hand the listening sockets to the new process (or bind the new ones before closing the old) to make it seamless. Same for a `proxy.Version` bump on upgrade.
- Watchdog in `just vps-test`: SIGSTOP the proxy and the daemon and check systemd restarts them (takes WatchdogSec, 30-60s, so it isn't there yet).
- conmon (podman's per-container monitor) runs inside `vops.service`'s cgroup: harmless with `KillMode=process`, but systemd attributes image-pull page cache to the service. Run podman through `systemd-run --user --scope` (or set conmon's cgroup) so each container is fully outside the daemon.
- migration helper: `vops migrate-db` for the dump/restore/verify dance in reference/migrating.md (postgres, mysql).
- re-pull third-party tags (`postgres:16`) on demand from the dashboard ("update images").
- prebuilt release binaries (amd64/arm64) so install and CI don't need go.
- per-container metrics (podman stats) in the dashboard.
- compose `secrets` as podman secrets from the project env (also hides values from `podman inspect`).
- log aggregation in duckdb/parquet if months-long search is ever needed (journald does it today).
