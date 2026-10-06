# Compose in vops

vops reads compose files itself and runs `podman run`. It supports the subset below; anything else is an error (so config is never silently ignored). `x-*` keys are allowed anywhere, and anchors/merge keys work.

## Files

A project dir may have `compose.yml`, `compose.yaml`, `docker-compose.yml`, `docker-compose.yaml` and any `*.compose.yml`/`*.compose.yaml`. They are merged; a service defined twice is an error. Dir names must match `[A-Za-z0-9][A-Za-z0-9_-]*`.

Top level: `services`, `volumes` (`name`, `external`), `networks` (see below), `version` and `name` (ignored), `x-vops` (`rate`, `burst`, `max_body`, `timeout`, see x-vops). Not supported: `secrets`, `configs`.

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
| `depends_on` | list or map with `condition` and `required` (see below) |
| `profiles` | see below |
| `networks` | list or map with `aliases`, `ipv4_address`, `ipv6_address` (see below) |
| `healthcheck` | `test`, `interval`, `timeout`, `retries`, `start_period`, `disable`; used as the readiness check |
| `restart` | default `unless-stopped` |
| `user`, `working_dir`, `hostname`, `labels` (not `vops.*`), `cap_add`, `cap_drop`, `devices`, `read_only`, `tmpfs`, `shm_size`, `init`, `stop_signal`, `stop_grace_period`, `dns`, `extra_hosts`, `security_opt`, `privileged`, `sysctls`, `ulimits`, `mem_limit`, `cpus`, `platform`, `pull_policy` | passed to podman |
| `deploy` | only `replicas` and `resources.limits.{memory,cpus,pids}` |
| `network_mode` | only `host` or `none` |

Not supported: `container_name` (vops names containers), `links`, `secrets`, `configs`, `extends`, `depends_on.restart`, `network_mode: service:x`.

## Networks

Same model as compose:

- A service without `networks` joins the project's `default` network (and the shared `vops` network, see below).
- A service with `networks` joins exactly those. `default` is the project default network.
- Services resolve each other by service name on every network they share, plus any `aliases`.
- Top-level networks support `internal` (no way out to the internet), `driver` (`bridge`, `macvlan`, `ipvlan`), `driver_opts`, `labels`, `enable_ipv6`, `ipam.config` (`subnet`, `gateway`, `ip_range`), `name` and `external`. `attachable` is accepted (all podman networks are). You can configure the default network by declaring `default`.
- `external: true` networks must already exist (`podman network create ...`); vops doesn't touch them.
- `vops` is a reserved key: the shared network every project can join, where a service is `<service>.<project reversed>` (e.g. `db.api.shop`). Services without `networks` join it automatically; services with `networks` join it only when they list `vops`, so a database on an internal network stays invisible to other projects.
- A routed service (`x-vops.port`) or one with `ports` needs at least one network that is not internal.
- Changing a network's definition recreates it: the plan warns, and its containers in this project are restarted (no rolling release for that apply). Networks no longer used are removed.

```yaml
services:
  web:
    image: shop/web
    networks: [default, backend, vops]   # public, talks to the db, reachable by other projects
    x-vops: {port: 8080}
  db:
    image: postgres:16
    networks:
      backend: {aliases: [database]}      # only reachable by web, no internet
networks:
  backend: {internal: true}
```

## depends_on and jobs

Services always deploy in dependency order, and vops waits for each one to be ready (see Readiness) before the next, whatever the condition. So `service_started` and `service_healthy` behave the same (the stricter one).

`condition: service_completed_successfully` makes the dependency a **job** (migrations, seeds): it runs to completion and is "ready" when it exits 0. Jobs default to `restart: no` (`always`/`unless-stopped` are errors) and a 10 minute timeout (`x-vops.timeout`). A finished job is not rerun until its definition changes; a job that failed is retried on the next apply.

If a service fails to deploy, everything that depends on it is skipped in that apply (the old containers keep running).

`required: false` lets a dependency be missing because of profiles.

## Profiles

Services with `profiles` only run when one of them is active. Active profiles come from `COMPOSE_PROFILES` in the project env, comma separated (`*` = all):

```sh
vops env set shop COMPOSE_PROFILES=debug,workers && vops apply
```

Depending on a service whose profile is off is an error unless the dependency has `required: false`.

## Data

Named volumes and bind mounts inside the project dir are created as btrfs subvolumes (when the host is on btrfs), so vops can snapshot them before deploys and roll them back. Existing non-empty dirs are never moved. Rootless tip: a container running as a non-root user needs `:U` on its volumes (`data:/var/lib/x:U`) to own them, same as plain podman.

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
      watch: true           # redeploy when this tag is pushed to the vops registry (it also ends an image rollback pin)
      preview:              # this service can get a preview (`preview: {}` = just itself; see Previews)
        with: [redis]       # services that run with it in a preview
        copy: [db]          # services whose data is copied from production (they run with it too)

x-vops:                     # top level: project settings
  rate: 20/s                # requests per client ip, per s, m or h (600/m); default unlimited
  burst: 40                 # requests allowed at once (default: rate per second rounded up, min 1)
  max_body: 10MB            # request body size: B, KB, MB, GB, 1024-based (10MB = 10485760 bytes); default unlimited
  timeout: 5m               # how long the proxy waits for a container's response headers (whole seconds); default 60s
```

Limits apply in the proxy to every routed service of the project (and its previews), per client ip (the tcp peer; `X-Forwarded-For` is ignored): over `rate` the answer is `429` with `Retry-After`, a bigger body `413` (early on `Content-Length`, while streaming for chunked bodies), no response headers within `timeout` `504`. `timeout` only covers the wait for headers: once they arrive, a streamed body (SSE, downloads) runs as long as it wants, so raise it for long polling only. The top-level `timeout` is the proxy's; a service's `x-vops.timeout` is its readiness timeout. They are set in one compose file of the project. A change shows in the plan (`~ limits: unlimited -> rate 20/s burst 40`) and applies without restarting containers.

Domains: project `shop/api` is `api.shop.<domain>`. Each routed service gets `<service>.api.shop.<domain>`; when a project has a single routed service it also gets `api.shop.<domain>`.

Readiness, in order: compose `healthcheck` → `x-vops.health` → any HTTP answer except 502/503/504 on `port` → "still running after 2s".

## Previews

Previews are opt-in per service: only a service with `x-vops.preview` can get one, and it says what runs with it and whose data is copied.

```yaml
services:
  web:
    image: registry.example.com/shop/web
    x-vops:
      preview:
        with: [redis]   # runs with it, starts empty
        copy: [db]      # runs with it, on a copy of production's data
  db: ...
  redis: ...
  worker: ...           # in no declaration: never runs in a preview
```

A preview (`vops preview up shop --name pr-42`, or a push of `shop/web:preview-pr-42`) runs the same compose files as project `shop@pr-42`, with these differences:

- **What runs**: the services the preview is for (the ones pushed or given with `--image`; none = every service with `x-vops.preview`), plus their `with` and `copy`. Nothing else. A required `depends_on` on a service outside that set is an error when the compose loads (`add db to x-vops.preview.with of web`); `with` and `copy` must name services of the project. `--image` of a service that doesn't run in the preview is an error; it can pin a `with`/`copy` service's image.
- **Data**: the volumes and bind mounts inside the project dir of `copy` services start as a copy of production's (btrfs; empty without it), made once when the preview is created. Every other volume starts empty; volumes only non-running services use aren't created. A volume shared by a copied service and another running one is copied (the plan warns).
- **Env**: production's env is not used. Variables come from `preview.env` in the project dir (committed, `KEY=VALUE` lines like `env_file`, for non-secret overrides such as `SMTP_HOST=mailpit`), then from preview secrets (`vops env set shop --preview KEY`), which win. `COMPOSE_PROFILES` too. `VOPS_PREVIEW` is the preview's name.
- **Code**: the compose files, build contexts and bind paths come from a git worktree of the preview's commit.
- No shared `vops` network: a service that lists it gets the preview's `default` network instead. Other projects can't reach the preview, and it can't reach them.
- No published `ports` (dropped with a warning) and no `x-vops.domains`.
- `name:` of volumes and networks is ignored (it would be production's). `external` volumes used by running services are an error; `external` networks and absolute bind mounts are shared with production (the plan warns about the latter).
- Networks with a fixed `ipam` subnet can't be created twice, so their projects can't have previews.

Tags: a pushed tag `preview-pr-42` is preview `pr-42` (the part after `preview-`, lowercased, `_` and `.` become `-`). It sets that tag on every service running the pushed repo that has `x-vops.preview`, and adds them to what the preview is for (pushing `web` then `api` runs both declarations). Services running the repo without it are skipped (an event says so). A `preview-*` tag never redeploys production.

## Interpolation

`$VAR`, `${VAR}`, `${VAR:-default}`, `${VAR-default}`, `${VAR:?error}`, `${VAR?error}`, `${VAR:+alt}`, `${VAR+alt}`, `$$` for a literal `$`. Values come from the project env (`vops env set`), plus `VOPS_PROJECT`, `VOPS_DOMAIN` and `VOPS_PROJECT_DOMAIN`. Unset variables become empty with a warning in the plan.

## Names on the host

| thing | name |
|---|---|
| container | `vops-<project with dots>-<service>-<random>` |
| project default network | `vops-<project with dots>`, alias `<service>` |
| other project networks | `vops-<project with dots>-<key>` (unless `name:`/`external`) |
| shared network | `vops`, alias `<service>.<project reversed>` |
| named volume | `vops-<project with dots>-<volume>` (unless `name:`/`external`) |
| built image | `localhost/vops/<project>-<service>:<hash>` |
| logs | journald `SYSLOG_IDENTIFIER=vops.<project with dots>.<service>` |
| preview | project `<project>@<name>`, `<project with dots>--<name>` in all names above, domain `<service>.<name>.<project reversed>.<domain>` |
