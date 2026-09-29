#!/usr/bin/env bash
# Production-like check: a container with systemd, a real sshd and podman plays the VPS.
# vops v1.2 is installed first (the daemon owns :80), then this build upgrades it in place (the proxy moves
# to vops-proxy.service); then: an upgrade and a daemon kill under load with 0 failed requests, and a
# rolling release under load. Uses the isolated test podman (~/.cache/vops-test). Needs network.
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
mkdir "$W/old" && git archive v1.2.0 | tar -x -C "$W/old"
(cd "$W/old" && CGO_ENABLED=0 go build -ldflags "-X main.version=v1.2.0" -o "$W/vops-1.2" ./cmd/vops)
host() { "$W/ssh" -i "$W/key" -p 2222 root@127.0.0.1 "$@"; }
get() { curl -s -o /dev/null -w '%{http_code}' -H 'Host: site.vps.test' 127.0.0.1:18081; }
# load LABEL SECONDS: requests in the background until SECONDS pass; check with `loaded LABEL`
load() { ( n=0; f=0; end=$((SECONDS+$2)); while [ $SECONDS -lt $end ]; do n=$((n+1)); [ "$(get)" = 200 ] || f=$((f+1)); done; echo "$n $f" > "$W/load" ) & }
loaded() { wait; read -r n f < "$W/load"; echo "$1: $n requests, $f failed"; [ "$f" = 0 ] || { echo FAIL; exit 1; }; }

mkdir "$W/repo" && cd "$W/repo"
$V init --domain vps.test >/dev/null
# no public dns in here, so no acme: plain http. install seeds the host config from vops.yml
sed -i 's/^tls: auto/tls: off/' vops.yml

# v1.2: one vops.service that is also the proxy
"$W/vops-1.2" install root@127.0.0.1 --port 2222 --ssh-key "$W/key"
mkdir -p site/html && echo v1 > site/html/index.html
printf 'services:\n  web:\n    image: docker.io/library/nginx:alpine\n    volumes: ["./html:/usr/share/nginx/html:ro,Z"]\n    environment: {V: "1"}\n    x-vops: {port: 80, replicas: 2}\n' > site/compose.yml
git add -A && git commit -qm v1 && "$W/vops-1.2" sync --yes
[ "$(get)" = 200 ] || { echo "FAIL: site not served by v1.2"; exit 1; }

# upgrade in place: the proxy moves to vops-proxy.service, no manual step
before=$(host 'podman ps -q | sort')
$V install root@127.0.0.1 --port 2222 --ssh-key "$W/key" 2>&1 | tee "$W/out"
grep -q "upgrading vops v1.2.0" "$W/out" || { echo "FAIL: install did not say it upgrades"; exit 1; }
host 'systemctl is-active vops vops-proxy' || { echo "FAIL: units not active"; exit 1; }
[ "$(host 'podman ps -q | sort')" = "$before" ] || { echo "FAIL: the upgrade changed containers"; exit 1; }
[ "$(get)" = 200 ] || { echo "FAIL: site not served after the upgrade"; exit 1; }
host 'ss -ltnpH "sport = :80" | grep -q "pid=$(systemctl show -p MainPID --value vops-proxy),"' || { echo "FAIL: :80 is not the proxy's"; exit 1; }

# upgrading again (same proxy.Version) under load: the proxy is kept, 0 failed requests
load upgrade 12; sleep 2
$V install root@127.0.0.1 --port 2222 --ssh-key "$W/key" 2>&1 | tee "$W/out"
grep -q "vops-proxy.service kept running" "$W/out" || { echo "FAIL: the proxy was restarted"; exit 1; }
loaded "upgrade (daemon restarted, proxy kept)"

# the daemon killed hard under load: systemd restarts it, sites never notice, containers untouched
before=$(host 'podman ps -q | sort')
load "daemon kill" 10; sleep 2
# the main process only, like a crash or the OOM killer (conmon, in the same cgroup, must survive)
host 'kill -9 $(systemctl show -p MainPID --value vops); sleep 4; systemctl is-active vops'
loaded "daemon SIGKILL"
[ "$(host 'podman ps -q | sort')" = "$before" ] || { echo "FAIL: daemon restart changed containers"; exit 1; }

# rolling release under load
sed -i 's/V: "1"/V: "2"/' site/compose.yml && git commit -qam v2
load "rolling release" 20; sleep 2; $V sync --yes
loaded "rolling release"
$V status
host 'systemctl show -p WatchdogUSec -p Type vops vops-proxy'
echo OK
