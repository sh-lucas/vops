# Development

```sh
just test       # everything (with -race, needs cgo), needs podman; uses an isolated podman storage in ~/.cache/vops-test
just build      # ./vops for this machine
just release    # dist/vops-linux-{amd64,arm64}
just gen        # sqlc generate (after editing internal/store/queries.sql or migrations/)
just vps-test   # systemd + real sshd + podman in a container as the host (slow, needs network)
```

Tests are end to end on purpose: `e2e/` drives the real binary over a fake ssh (install, sync, registry push and auto redeploy, release approval and canaries through the real proxy, users and permissions, the dashboard api, backups, previews, rollback, the proxy as its own process under a rolling release and a daemon SIGKILL, version mismatches, the secrets backup, notifications), `internal/deploy` checks zero failed requests during rolling releases (and canary promotions), that nothing but an approval ships and that failures leave nothing half-done; `just vps-test` upgrades a real systemd host in place. Unit tests cover the rest, including the web-facing parts: every route needs a session and the CSRF header, inputs are validated, wrong credentials get limited per client, file reads stay in the repo, registry auth and protocol edge cases, Web Push (RFC 8291 vector), the proxy's limits, hardening and canary routing (weights, sticky and forced cookies, 5xx per group). `FuzzSplitPath` and `FuzzLoad` fuzz the registry path splitter and the compose loader (`go test -fuzz`).
