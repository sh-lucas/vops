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

- Release modes (next). One key at the compose top level, per project (a push also touches jobs sharing the image, so per service would run a gated `migrate` at once):
  ```yaml
  x-vops:
    release: auto        # default: a push deploys (rolling or recreate, per strategy)
    release: manual      # a push waits for Approve in the dashboard
    release: canary      # a push starts a canary at 10% of traffic; Approve promotes it, Reject removes it
    release: canary 25%
  ```
  Only registry pushes are gated: `vops sync`/`apply` over ssh already ask a human (the plan prompt), and anyone with ssh can bypass a gate anyway, so this is a workflow gate for CI deployers, not a security boundary. Admins approve in the dashboard; deployers can't log in, so the pusher never approves their own push.
  - **Step 1, `manual`.** A push records a pending release (project, repo:tag, digest, who, when) and holds the running services on their current digest: a hold works like a pin (GC keeps it, the plan shows nothing pending) but shows as "waiting approval", has no Unpin, and `onPush` stops passing `Unpin` for gated projects. The pending digest joins the GC keep set. A newer push of the same repo:tag supersedes it. Approve (now, or at a time: stored as unix time, run by the minute housekeeping tick) deploys exactly that digest and refuses if the tag moved; Reject drops it. `plan` and `status` show it as a line, not an action. New `releases` table (migration + `just gen`), an `approval` notification kind (on by default), timeline rows for push held, approved, rejected, a badge like "pending changes".
  - **Step 2, `canary`, on top of manual.** At push, jobs in the set run (v2 needs its schema, the same as a rolling release where v1 and v2 already share it, just longer: migrations must be expand/contract); workers wait for Approve. Each routed service gets one canary container whatever its `replicas`, labelled with role and weight so `RefreshRoutes` rebuilds the split from podman, ignored by `compare`, with no network alias for its service name (internal traffic never hits it). Proxy: an `omitempty` canary group on `Route` (bump `proxy.Version`), sticky by cookie assigned by weight on the first request; a "open the canary" link in the dashboard sets the cookie so you can test it on the real domain. A crash, restart or failed readiness of the canary rejects it and notifies. The dashboard shows the canary's age and its 5xx next to the stable's (counted in the proxy per group, like 429/413); a human decides, no automatic analysis. Approve = the usual rolling release to that digest, then the canary goes. A compose change to a held service rebuilds the canary with the new definition. Routed services with `strategy: recreate` (or none routed) behave as `manual`.
  - Open: is "scheduled" only Approve-at-a-time (above), or also a config window where pushes go live at a fixed hour without approval (a different feature)?
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
