# CI

CI needs no lock file: pass `--host` and `--ssh-key`. Unknown host keys are accepted on first use (`StrictHostKeyChecking=accept-new`); pin them with `ssh-keyscan` into `~/.ssh/known_hosts` if you care.

## Deploy on push (GitHub Actions)

Create a deploy key on the host (`~/.ssh/authorized_keys`), store the private key as the `VOPS_SSH_KEY` secret and `user@host` as the `VOPS_HOST` variable.

```yaml
name: deploy
on:
  push:
    branches: [main]
concurrency: deploy
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 0}      # sync merges the host's history
      - uses: actions/setup-go@v5
        with: {go-version: "1.27"}
      - run: go install github.com/sh-lucas/vops/cmd/vops@latest
      - run: |
          install -m 600 /dev/null key && printf '%s\n' "$KEY" > key
          git config user.name ci && git config user.email ci@localhost
          vops sync --yes --host "$HOST" --ssh-key key
        env:
          KEY: ${{ secrets.VOPS_SSH_KEY }}
          HOST: ${{ vars.VOPS_HOST }}
```

If sync has to merge the host's commits, CI pushes that merge to the host but not back to GitHub; add a `git push` step if you want both to match.

## Push images from CI

```sh
vops user add shop-ci --pattern 'shop/.*'     # prints a token once
```

```yaml
      - run: |
          echo "$TOKEN" | podman login -u shop-ci --password-stdin registry.example.com
          podman build -t registry.example.com/shop/web:latest .
          podman push registry.example.com/shop/web:latest
        env:
          TOKEN: ${{ secrets.VOPS_REGISTRY_TOKEN }}
```

Services running `registry.example.com/shop/web:latest` are redeployed right after the push (rolling), unless they set `x-vops.watch: false`.
