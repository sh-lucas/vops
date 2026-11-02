# vops

Self-host containers from a git repo. Push compose files over ssh; vops deploys them with podman, serves them over https and snapshots their data on btrfs. One static binary, no cluster, no extra services.

- **GitOps:** the repo is the whole state of the server. `vops sync` shows the plan and asks before applying.
- **Safe deploys:** rolling releases with readiness checks; `vops rollback` restores images *and* data (btrfs, copy-on-write).
- **Batteries included:** proxy with Let's Encrypt, OCI registry, previews with a copy of prod data, write-only secrets, dashboard, push notifications.
- **Plain linux:** systemd, podman and git. The daemon idles at ~30MB RSS.

## Quick start

```sh
go install github.com/sh-lucas/vops/cmd/vops@latest   # go 1.27+

vops init --domain example.com --email you@example.com
vops install root@203.0.113.10        # dns: example.com and *.example.com -> the host

mkdir -p site/html && echo hello > site/html/index.html
cat > site/compose.yml <<'EOF'
services:
  web:
    image: docker.io/library/nginx:alpine
    volumes: [./html:/usr/share/nginx/html:ro]
    x-vops: {port: 80}
EOF
git add -A && git commit -m site && vops sync   # -> https://site.example.com
```

## Docs

- [Features, commands and requirements](reference/features.md)
- [Deploying an app](reference/deploying.md) and [CI](reference/ci.md)
- [Supported compose](reference/compose.md) and [limitations](reference/limitations.md): one host, podman only, snapshots need btrfs.
- [Design decisions](reference/decisions.md) and [development](reference/development.md) (`just test`, needs podman).
