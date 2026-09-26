# Compose in vops

vops reads compose files itself and runs `podman run`. It supports the subset below; anything else is an error (so config is never silently ignored). `x-*` keys are allowed anywhere, and anchors/merge keys work.

## Files

A project dir may have `compose.yml`, `compose.yaml`, `docker-compose.yml`, `docker-compose.yaml` and any `*.compose.yml`/`*.compose.yaml`. They are merged; a service defined twice is an error. Dir names must match `[A-Za-z0-9][A-Za-z0-9_-]*`.

Top level: `services`, `volumes` (`name`, `external`), `version` and `name` (ignored). Not supported: `networks`, `secrets`, `configs`.

## Service keys

| key | notes |
|---|---|
| `image` | |
| `build` | string or `{context, dockerfile, args, target}`; built on the host, rebuilt only when the context's git tree changes. `image` is ignored when `build` is set |
| `command`, `entrypoint` | list or string (split like a shell) |
| `environment` | map or list; a bare `KEY` takes the project env var |
| `env_file` | relative to the project dir (must be committed) |
| `ports` | published on the host (short or long syntax). Forces `recreate` |
| `expose` | accepted, informational |
| `volumes` | `./rel:/path[:opts]`, `/abs:/path`, `named:/path` (declared at top level), anonymous, long syntax with `type: bind/volume/tmpfs` |
| `depends_on` | list or map (conditions ignored: vops always waits for readiness in order) |
| `healthcheck` | `test`, `interval`, `timeout`, `retries`, `start_period`, `disable`; used as the readiness check |
| `restart` | default `unless-stopped` |
| `user`, `working_dir`, `hostname`, `labels` (not `vops.*`), `cap_add`, `cap_drop`, `devices`, `read_only`, `tmpfs`, `shm_size`, `init`, `stop_signal`, `stop_grace_period`, `dns`, `extra_hosts`, `security_opt`, `privileged`, `sysctls`, `ulimits`, `mem_limit`, `cpus`, `platform`, `pull_policy` | passed to podman |
| `deploy` | only `replicas` and `resources.limits.{memory,cpus,pids}` |
| `network_mode` | only `host` or `none` |

Not supported: `container_name` (vops names containers), `networks`, `links`, `secrets`, `configs`, `profiles`, `extends`.

## x-vops

```yaml
services:
  web:
    image: registry.example.com/shop/web:latest
    x-vops:
      port: 8080            # http port inside the container: routes <service>.<project>.<domain> to it
      domains: [shop.com]   # extra domains (get certificates too)
      health: /healthz      # readiness: GET must answer 2xx/3xx
      replicas: 2           # routed services only
      strategy: rolling     # rolling (default when routed, no host ports) | recreate
      timeout: 60s          # readiness timeout
      watch: true           # redeploy when this tag is pushed to the vops registry
```

Domains: project `shop/api` is `api.shop.<domain>`. Each routed service gets `<service>.api.shop.<domain>`; when a project has a single routed service it also gets `api.shop.<domain>`.

Readiness, in order: compose `healthcheck` → `x-vops.health` → any HTTP answer except 502/503/504 on `port` → "still running after 2s".

## Interpolation

`$VAR`, `${VAR}`, `${VAR:-default}`, `${VAR-default}`, `${VAR:?error}`, `${VAR?error}`, `${VAR:+alt}`, `${VAR+alt}`, `$$` for a literal `$`. Values come from the project env (`vops env set`), plus `VOPS_PROJECT`, `VOPS_DOMAIN` and `VOPS_PROJECT_DOMAIN`. Unset variables become empty with a warning in the plan.

## Names on the host

| thing | name |
|---|---|
| container | `vops-<project with dots>-<service>-<random>` |
| project network | `vops-<project with dots>`, alias `<service>` |
| shared network | `vops`, alias `<service>.<project reversed>` |
| named volume | `vops-<project with dots>-<volume>` (unless `name:`/`external`) |
| built image | `localhost/vops/<project>-<service>:<hash>` |
| logs | journald `SYSLOG_IDENTIFIER=vops.<project with dots>.<service>` |
