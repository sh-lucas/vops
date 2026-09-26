#!/usr/bin/env bash
# Production-like check: a container with systemd, a real sshd and podman plays the VPS.
# vops installs over real ssh, syncs over git+ssh, and does a rolling release under load.
# Uses the isolated test podman (~/.cache/vops-test). Needs network (pulls fedora and nginx).
set -euo pipefail
cd "$(dirname "$0")/../.."
P=${VOPS_TEST_PODMAN:-$HOME/.cache/vops-test/podman}
W=$(mktemp -d /tmp/vops-vps.XXXX)
trap '$P rm -f vops-vps >/dev/null 2>&1 || true; rm -rf "$W"' EXIT

[ -x "$P" ] || { echo "run 'just test' once first (it creates $P)"; exit 1; }
ssh-keygen -q -t ed25519 -N '' -f "$W/key"
cp "$W/key.pub" "$W/authorized_keys"
cp e2e/vps/Containerfile "$W/"
$P build -q -t localhost/vops-vps "$W" >/dev/null
$P rm -f vops-vps >/dev/null 2>&1 || true
$P run -d --name vops-vps --privileged --systemd=always -p 127.0.0.1:2222:22 -p 127.0.0.1:18081:80 localhost/vops-vps >/dev/null
sleep 3

printf '#!/bin/sh\nexec ssh -o UserKnownHostsFile=%s/known_hosts "$@"\n' "$W" > "$W/ssh" && chmod +x "$W/ssh"
export VOPS_SSH=$W/ssh GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
CGO_ENABLED=0 go build -o "$W/vops" ./cmd/vops
V=$W/vops
mkdir "$W/repo" && cd "$W/repo"
$V init --domain vps.test >/dev/null
$V install root@127.0.0.1 --port 2222 --ssh-key "$W/key"
# no public dns in here, so no acme: plain http
"$W/ssh" -i "$W/key" -p 2222 root@127.0.0.1 'printf "tls: off\n" > ~/.vops/config.yml && systemctl restart vops'

mkdir -p site/html && echo v1 > site/html/index.html
printf 'services:\n  web:\n    image: docker.io/library/nginx:alpine\n    volumes: ["./html:/usr/share/nginx/html:ro,Z"]\n    environment: {V: "1"}\n    x-vops: {port: 80, replicas: 2}\n' > site/compose.yml
git add -A && git commit -qm v1 && $V sync --yes
get() { curl -s -o /dev/null -w '%{http_code}' -H 'Host: site.vps.test' 127.0.0.1:18081; }
[ "$(get)" = 200 ] || { echo "FAIL: site not served"; exit 1; }

# the daemon restarting must not touch containers
before=$("$W/ssh" -i "$W/key" -p 2222 root@127.0.0.1 'podman ps -q | sort')
"$W/ssh" -i "$W/key" -p 2222 root@127.0.0.1 'systemctl restart vops; sleep 2'
after=$("$W/ssh" -i "$W/key" -p 2222 root@127.0.0.1 'podman ps -q | sort')
[ "$before" = "$after" ] || { echo "FAIL: daemon restart changed containers"; exit 1; }

# rolling release under load
sed -i 's/V: "1"/V: "2"/' site/compose.yml && git commit -qam v2
( n=0; f=0; end=$((SECONDS+20)); while [ $SECONDS -lt $end ]; do n=$((n+1)); [ "$(get)" = 200 ] || f=$((f+1)); done; echo "$n $f" > "$W/load" ) &
sleep 2; $V sync --yes; wait
read -r n f < "$W/load"
echo "rolling release: $n requests, $f failed"
[ "$f" = 0 ] || { echo FAIL; exit 1; }
$V status
echo OK
