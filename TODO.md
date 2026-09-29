# TODO

Ideas with a design sketch. Done things move to README/reference; decisions to reference/decisions.md.

## Data

Done: btrfs subvolumes, pre-deploy snapshots, `vops rollback` (images by pinning, and data), previews (`vops preview`, previews from registry tags), deploy history with the dashboard timeline and previews tab. What's left builds on the same pieces.

### Previews, next

- Reset a preview's data without `rm`: `preview up --from <id>` (or `--from live`) on an existing preview; stop it, restore like rollback, start.
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
