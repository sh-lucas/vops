# Migrating a docker compose server to vops

What worked moving a real server (docker compose + caddy + registry:2 + watchtower; a postgres app and a mysql app) with ~1m20s of downtime and no DNS changes.

## Before (no downtime)

1. **Host packages** (needs sudo once): `podman netavark aardvark-dns uidmap slirp4netns`; `net.ipv4.ip_unprivileged_port_start=80` in `/etc/sysctl.d/` (write it with `sudo sh -c`, `sudo -S` eats the stdin of `tee`); `loginctl enable-linger <user>`.
2. **Firewall**: docker bypasses ufw, vops doesn't. If ufw is on, `ufw allow 80/tcp` and `443/tcp` (it changes nothing for the running docker stack).
3. **Repo**: the compose repo becomes the vops repo on a branch. `vops init --domain <domain>`, then in `vops.yml` a rehearsal listener that doesn't clash with the old proxy: `http: "127.0.0.1:18080"`, `https: "off"`.
4. **Compose changes**: drop `container_name` (use service names: `http://backend:3001`), drop the external proxy network, routes from the Caddyfile become `x-vops: {port, domains}`, `env_file` values go to `vops env set <project> < .env`, anonymous database volumes become named volumes, pin database images to the digest the old stack runs (`docker inspect -f '{{index .RepoDigests 0}}'`), add a healthcheck to databases so apps wait for them.
5. **Install**: `vops install <host>`; `vops admin password`; keep CI credentials with `vops user add <ci-user> --pattern '.*' --token-stdin`.
6. **Images**: on the host, `docker login 127.0.0.1:9984` (docker trusts loopback registries) with a throwaway `DOCKER_CONFIG`, then `docker tag`/`docker push` every image, using the running containers' image ids so the new stack runs the same bytes.
7. **Databases first**: sync a commit with only the database services, restore a live dump (`pg_dump`, `mysqldump --single-transaction --databases x`), compare exact row counts per table. Only then sync the apps: they never boot against an empty database (no surprise migrations or seeds).
8. **Rehearsal**: send the same requests to the old stack (public https) and to vops (ssh tunnel to the rehearsal listener) and compare status + body, including endpoints that hit the database (a failed login is perfect: it queries and changes nothing).

## Cutover (downtime)

1. Stop the old proxy and apps (keep the old databases running), final dumps.
2. Stop the new backends, drop/recreate the new databases, restore, compare row counts against the old databases. MySQL: `RESET BINARY LOGS AND GTIDS` before restoring a second dump into the same server, or it refuses the dump's `GTID_PURGED`.
3. Start the new backends, stop the old databases.
4. Switch `vops.yml` to `http: ":80"`, `https: ":443"` and sync: the daemon re-execs on the new ports, certificates are issued on the first request of each domain (~5s).
5. Test from outside: the same battery as the rehearsal, bodies against a baseline taken before starting, and the registry flow CI uses (login, pull, push).

Rollback at any point: `systemctl --user stop vops` and `docker start` the old containers (nothing of the old stack is deleted; `restart: unless-stopped` keeps them stopped across reboots).
