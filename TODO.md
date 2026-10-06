# TODO

Ideas with a design sketch. Done things move to README/reference; decisions to reference/decisions.md.

## Proxy hardening (next)

Timeouts, header size and connection caps are done (reference/decisions.md, "The proxy process"). Both of these bump `proxy.Version`:

- IP allow/deny list per project in compose's `x-vops`, plus basic-auth in front of a route (staging/previews).
- `x-vops.redirect` www→apex.

## Security

- Login throttle is a global mutex + 1s sleep: an attacker's failed attempts queue in front of the real admins and nothing is per ip. Per-ip backoff (token bucket in memory, like the proxy's limiter), keeping the failed-login event. Same for registry basic auth failures.
- Dashboard roles below admin (viewer, per-project scopes) if someone needs them; notifications already filter through `notify.CanSee`.
- Fuzz the registry manifest parser and the log query parser.

## Releases

- Manual approval per project: `x-vops.approve: true`. A push that would redeploy records a pending release (repo, tag, digest, who pushed) instead of deploying; the dashboard shows Approve/Reject; approve runs the same apply as `onPush`. Needs a `pending_releases` table, a badge like "pending changes" and an `approval` notification kind.
- Tag semantics to document (they already work this way): a service on `myapp:v2.1.8` only redeploys when `v2.1.8` itself is pushed again; pushing `latest` does nothing to it. Warn when a push overwrites an existing non-`latest` tag.
- Prebuilt release binaries (amd64/arm64) so install and CI don't need go.

## Notifications

- Outbound webhooks (generic JSON, ntfy, Discord, Slack) as one more kind of device: a row with its URL (a secret, sealed like env), the same kinds, toggles and access rule, "Send test", added from the Notifications page.
- Per-project mutes.
- Opt-in early kill: `x-vops.oom: kill-first` on a service lets vops restart it when memory PSI stays high, before the kernel or systemd-oomd picks a victim (maybe the proxy). Never a default: it could pick a database. `mem_limit` is the first answer.

## Data

- Per-preview ttl (`--ttl 12h`, `0` = keep) for long-lived staging-like previews.
- Delete a preview's `preview-*` registry tags when it's removed (today they stay until `vops registry rm`).
- Opt-out per project for pre-deploy snapshots (`x-vops: {snapshots: false}`; today it's host-wide in `vops.yml`).
- Scheduled off-host export of snapshots (ssh, S3-compatible); `vops snapshot export|import` exists.
- Tag built images per commit (keep the last N) so built services can roll back too (today image rollback skips them).

## Other

- Proxy: a listener change or a `proxy.Version` bump re-execs it, so sites blink; hand the listening sockets to the new process to make it seamless.
- Re-pull third-party tags (`postgres:16`) on demand from the dashboard ("update images").
- Per-container cpu/memory in the dashboard: the notifications monitor already reads them from cgroups.
- Compose `secrets` as podman secrets from the project env (also hides values from `podman inspect`).
- `vops migrate-db` for the dump/restore/verify dance in reference/migrating.md (postgres, mysql).
