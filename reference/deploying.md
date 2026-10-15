# Deploy a new application on an existing host

Run vops commands from the linked infrastructure repo; Docker builds run from the application repo.

1. Prepare the Dockerfiles for every service. If the frontend embeds its API URL, set it during the build; browser variables must never contain secrets.
2. Add `shop/compose.yml` to the infra repo with the exact image names and tags CI will push, persistent volumes, dependencies, and `x-vops.port` and `x-vops.health` for HTTP services. See [the Compose reference](reference/compose.md).
3. Point every application domain to the host, including those in `x-vops.domains`. Create individual DNS records if you have no wildcard.
4. Set runtime secrets before the first deploy: `vops env set shop DB_PASSWORD` prompts without echoing the value. Repeat for each secret and reference them in Compose; setting secrets does not deploy anything.
5. Create a registry user restricted to the application's image repositories (`--repo` omits the registry hostname and tag):

   ```sh
   vops user add shop-ci --repo shop/web --repo shop/api
   ```

Save the token shown once. In the application's GitHub Actions secrets, store the username and token (for example, `DOCKER_REGISTRY_USERNAME` and `DOCKER_REGISTRY_PASSWORD`). The token is the registry password.
6. Configure application CI to log into `registry.<domain>`, build and push **every** application image, using the same repository names and tags as Compose. Separate frontend and backend steps make progress easier to follow. See [the CI examples](reference/ci.md).
7. Once the images are available (`vops registry ls`), commit the Compose file and run `vops sync`, or push to an infra repo whose CI runs sync. Infra CI needs its own SSH secret `VOPS_SSH_KEY` and variable `VOPS_HOST`; registry credentials alone do not grant host access.
8. Check `vops status`, `vops logs shop api`, and the public HTTPS endpoints. Confirm the frontend reaches the intended API and complete any application-specific setup, such as creating its first administrator.

By default, later pushes of a watched tag trigger redeploys for services using it in the host's Compose; each image push can deploy separately. `release: manual` waits for approval; `canary` waits for approval before full rollout. Compose changes still need sync. If a service stays `pending create`, inspect the failed deploy, fix the cause, then retry with `vops apply shop` or the infra workflow. A successful image push alone does not confirm a healthy application.
