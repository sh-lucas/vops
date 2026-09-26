version := `git describe --tags --always --dirty 2>/dev/null || echo dev`

build:
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version={{version}}" -o vops ./cmd/vops

test:
    go vet ./...
    go test ./...

release:
    for arch in amd64 arm64; do CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version={{version}}" -o dist/vops-linux-$arch ./cmd/vops; done

# wipe the isolated podman the tests use
test-clean:
    ~/.cache/vops-test/podman system reset -f

# production-like: systemd + real sshd + podman in a container (slow, needs network)
vps-test:
    ./e2e/vps/run.sh
