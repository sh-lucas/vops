## Thoughts

A daemon (if needed) and a cli. The cli should install the daemon on the host by ssh (and modify systemd or just use podman run there) and have sub-commands for managing the daemon and containers.
`podman` runs the container, the daemon handles and abstracts the state, change, management, log aggregation and gitops side of things.

The vops cli should have a push command that pushes the current git repo to any ssh host; it should save the repo itself in `~/vops/` and itself's state in `~/.vops/`.
You don't need to implement the daemon pulling the repo, but know that it's a planned feature. It would be cool if you could make releated git repos and pull and push via SSH, but that's optional; consider that creating a github actions manually for the repo is fine, just guarantee that there is a way to "log-in" with a gitignored vops-lock.toml/yml (and in this case add the line to a gitignore file automatically on vops init) AND by passing `--ssh-key <path>` to the cli (or something like that).

`~/vops/` is the repo on the remote, `./` is the repo locally, `~/vops/registry` should be created automatically and contain the data for the registry, `~/vops/<projectName>` is the way to declare a new project.
If the repo is pushed to the remote by ssh, vops should check and up and down any compose files with vops sync, asking for confirmation before taking any action.

It should be almost staless if possible, and I would love to see rooling releases; that's to say: create a new container replica, redirect the trafic (after a readyness probe confirmation) and just then remove the old container.
All state should be saved on an sqlite database on the remote machine.

Example:
- ~/vops/registry is fixed, and should be created automatically with a .gitignore to ignore data/ (that should contain all blobs for the images themselves).
- ~/vops/myProject/ is an old project that I have commited and pushed.
    - ~/vops/myProject/compose.yml is the compose file for the project.
    - ~/vops/myProject/data/ is the stuff I declare, and it's my responsability to gitignore it.
- `./myCoolProject/` is another project that I just created locally.
    - `vops sync` should git pull and then push the latest commit I did, do a podman compose up if there is any "*compose.yml" file in `./myCoolProject`.
    - If I created `./myCoolProject/mySubProject/compose.yml`, vops sync should create a new subproject `~/vops/myCoolProject/mySubProject/` and up the containers as expected.
    - In the previous case, mySubProject.myCoolProject.myDomain.com should be the path of the network in that case.
    - I don't really care how you do the networking, but keep it simple and specify a default for every project so it's easy to connect one to another.
- preferably, vops should have it's own container namespace or at least use the sqlite to manage everything.


When pushed, vops should compare the local state (of the repo) to the state on the remote host and push any changes, upping new containers that follow `~/vops/<projectName>/compose.yml` and create a new network automatically routing it using the daemon, or Traefik, or Caddy, or nginx, or whatever you feel that it's beter.

Everything should be testable using local containers, docker-in-docker or, if really needed, ask the user for a real cloud machine for the testing. You can (and probably should) ship the daemon with subcommands that does the upping and downing of the containers, since that way it would be easier to understand what's happening.

vops.yourDomain.com/ should have a simple UI to manage the projects, basically see which one is which, and allow the user to:
- Deactivate/turn off a project
- see the whole VPS structure in a tree view
- see container logs, restart containers
- see the images pushed to the registry
- CRUD for registry users; that's important for me, and should be very very simple way to create a new user for manaing an entire project.
    - you can use whatever user:pass or jwt's, I don't care.
    - you should be able to define a regex and a list of names allowed for that user to push.
- the login and password for the dashboard itself should be capable of downloading any image, but not pushing any shit.
- It should not be a file explorer; just the folders tracked by git should be shown and only folders and the most common text files.
- it should allow environment variables to be set for each project.
    - Yeah, any user can set any project's environment variables, but no way that anybody should see the variable itself.

## Implementation map

The thoughts above are the original brief. How it ended up (details and reasons in [decisions.md](decisions.md)):

| package | does |
|---|---|
| `cmd/vops` | main |
| `internal/cli` | commands: forwards host commands over ssh (`remote.go`), talks to the socket on the host (`host.go`), init/install/sync/apply (`local.go`), host setup + the two systemd units (`setup.go`), cli/host version check (`version.go`) |
| `internal/daemon` | unix sockets (cli, `web.sock` for the proxy) and the ui listener, json api, login and sessions, registry auth, logs, git-tracked file tree, pushing the routing table to the proxy |
| `internal/deploy` | plan (git vs podman), apply, rolling/recreate, readiness, routes from labels, pre-deploy snapshots (`snapshots.go`), rollback of images (pins) and data (`rollback.go`), previews: worktree, data copy, env layers, push targets, ttl (`preview.go`), deploy history and timeline (`history.go`), backups: snapshots as .tar.gz, export and checked import (`backup.go`), proxy limits from compose into the table (`plan.go`, `apply.go`) |
| `internal/compose` | compose subset parser, interpolation, service → podman args |
| `internal/secrets` | the `.secrets.age` backup: recipients from authorized_keys and `vops.yml`, the plaintext format, age encrypt/decrypt, private keys in `~/.ssh`. The host side is `daemon/secrets.go`, the laptop side `cli/secrets.go` |
| `internal/registry` | OCI registry on the filesystem, GC (with a keep set of digests from the daemon) |
| `internal/proxy` | routing table + reverse proxy, per-project limits (`limit.go`: token bucket per client ip, body size); the `vops proxy` process (`server.go`: :80/:443, autocert, control socket, table on disk, forwarding to the daemon) and its client (`client.go`) |
| `internal/sdnotify` | systemd READY=1 and watchdog pings (stdlib) |
| `internal/store` | sqlite: env (encrypted), users (admins/deployers, their repos; rules as checks and triggers), sessions, events, snapshots, previews, deploys, pins, project limits, push subscriptions, alerts, notification prefs and settings, audit log. SQL in `queries.sql` + `migrations/`, Go generated by sqlc into `queries/` |
| `internal/snapshot` | btrfs primitives: subvolumes, read-only snapshots, restore in place, delete, commands in the user namespace (rootless via `podman unshare`) |
| `internal/notify` | notifications: Web Push (VAPID + aes128gcm, `webpush.go`), the alert state machine and access rule (`alert.go`), kinds and thresholds (`settings.go`), detectors' pure logic (`detect.go`), the Notifier (db + delivery, `notifier.go`) and the Monitor in the daemon: podman events, reconciliation, probes, resources (`watch.go`). The api is `daemon/notify.go` |
| `internal/sysmon` | host load from `/proc` and `/sys` (cpu, memory, disk io and space, network, PSI, kernel, uptime), rates from the previous sample, oom kills; `/api/system`, sampled in the background for alerts |
| `internal/ui` | embedded dashboard (html/css/js), service worker and manifest (push, installable) |
| `e2e` | the real binary end to end, with a fake ssh |
