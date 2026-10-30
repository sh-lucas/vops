version := `git describe --tags --always --dirty 2>/dev/null || echo dev`

build:
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version={{version}}" -o vops ./cmd/vops

gen:
    sqlc generate

test:
    go vet ./...
    go test -race -timeout 40m $(go list ./... | grep -v /e2e$)
    go test -race -timeout 40m -count=1 ./e2e  # it builds the binary: a cached pass may be stale

release:
    for arch in amd64 arm64; do CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version={{version}}" -o dist/vops-linux-$arch ./cmd/vops; done

# lint and test the dashboard (dev only, needs node; the dashboard itself has no build step)
ui:
    cd internal/ui && npm ci --silent && npm run lint && npm test

# wipe the isolated podman the tests use
test-clean:
    ~/.cache/vops-test/podman system reset -f

# production-like: systemd + real sshd + podman in a container (slow, needs network)
vps-test:
    ./e2e/vps/run.sh
