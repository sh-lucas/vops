# TODO

Ideas with a design sketch. Done things move to README/reference; decisions to reference/decisions.md.

## Proxy hardening (next)

Timeouts, header size and connection caps are done (reference/decisions.md, "The proxy process"). Both of these bump `proxy.Version`:

- IP allow/deny list per project in compose's `x-vops`, plus basic-auth in front of a route (staging/previews).
- `x-vops.redirect` www→apex.

## Security

- Dashboard roles below admin (viewer, per-project scopes) if someone needs them; notifications already filter through `notify.CanSee`.
- Fuzz the registry manifest parser and the log query parser.

## Releases

- Release modes: scheduled approval is Approve-at-a-time. Open: also a config window where pushes go live at a fixed hour without approval (a different feature)?
- Release modes: automatic canary analysis (promote/reject on the 5xx ratio) if a human deciding turns out to be the bottleneck.
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
