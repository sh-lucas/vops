# TODO

Ideas with a design sketch. Done things move to README/reference; decisions to reference/decisions.md.

## Data: branching and previews

Phase 1 is done: data lives on btrfs subvolumes, snapshots before every deploy, `vops rollback`. The pieces below build on it without new concepts: a snapshot is a set of read-only subvolumes (`snapshots` + `snapshot_volumes` tables), and `snapshot.Restore` already turns one into a writable copy anywhere on the same filesystem.

### Phase 2: preview environments (database branching)

- `vops preview up <project> --name pr-42 [--ref <git ref>] [--image svc=ref] [--from <snapshot>]`, `vops preview ls|rm`.
- A preview is a synthetic project `<project>@pr-42`: the engine already works per project (labels, hashes, plan/apply), so it mostly needs a different path, names and domain.
- Code: `git worktree add ~/.vops/previews/<name> <ref>` on the host (build contexts and bind mounts come from there). `sync` could push the current branch as `refs/heads/<branch>` so `--ref` just works.
- Data: take (or reuse) a snapshot of the base project and `snapshot.Restore` each volume into the preview's own volume names. O(1), copy-on-write.
- Names: containers/networks/volumes get the preview slug; domain `<service>.pr-42.<project reversed>.<domain>` (autocert already handles arbitrary hosts).
- State: a `previews` table (name, project, ref, image overrides, snapshot, ttl, created, last used) + audit triggers.
- Cleanup: TTL (default 3 days since last update) in housekeeping; `rm` deletes containers, networks, volumes, worktree.
- Guardrails (defaults, not options you have to remember):
  - previews don't join the shared `vops` network (can't reach prod by accident);
  - env: `COMPOSE_PROFILES` and a preview env layer that overrides the project env; document loudly that prod secrets + prod data can send real emails / charge real cards;
  - `x-vops.preview: {skip: true}` for workers and crons;
  - max previews per host (RAM: every preview is another database).
- Deep test: preview with a real postgres, run a destructive migration job in it, prod untouched, rm leaves nothing behind.

### Phase 3: previews from registry tags

- Push `shop/web:preview-pr-42` → create/update preview `pr-42` of the projects running `shop/web`, with that image. The push trigger (`Daemon.onPush`) already finds services by repo; it needs a tag pattern (`x-vops.previews: "preview-*"`) and to call the preview code instead of apply.

### Smaller data items

- Opt-out per project for pre-deploy snapshots (top-level `x-vops: {snapshots: false}` in compose; today it's host-wide in `vops.yml`).
- `vops snapshot diff <id>`: `btrfs subvolume find-new` / sizes, to see how much a snapshot holds exclusively.
- Show snapshot disk usage (needs quotas or `btrfs filesystem du`, which is slow; maybe only on demand).
- Off-host backups: `btrfs send` of a snapshot to a file/ssh target (`vops snapshot export <id> > file`), incremental against the previous one.
- Roll back code and data together: `vops rollback --with-code` = check out the snapshot's commit as a revert commit on the host repo, apply, then restore. Needs care with the git-first model (the laptop must pull it).

## Other

- validate on a real cloud VPS (real dns + let's encrypt, a non-root user with linger), then tag v0.2. root + real sshd + systemd is covered by `just vps-test`.
- re-pull third-party tags (`postgres:16`) on demand from the dashboard ("update images").
- prebuilt release binaries (amd64/arm64) so install and CI don't need go.
- per-container metrics (podman stats) in the dashboard.
- compose `secrets` as podman secrets from the project env (also hides values from `podman inspect`).
- log aggregation in duckdb/parquet if months-long search is ever needed (journald does it today).
