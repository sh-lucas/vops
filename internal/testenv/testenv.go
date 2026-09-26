// Package testenv gives e2e tests an isolated podman (own storage, netavark) and a tiny test image.
package testenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"

	"github.com/sh-lucas/vops/internal/podman"
)

var (
	once     sync.Once
	wrapper  string
	setupErr error
	lockFile *os.File
)

// Podman points VOPS_PODMAN at an isolated podman and returns the wrapper path.
// Storage lives in ~/.cache/vops-test so images survive between runs; containers are per test.
func Podman(t testing.TB) string {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not installed")
	}
	once.Do(func() {
		cache, _ := os.UserCacheDir()
		root := filepath.Join(cache, "vops-test")
		run := filepath.Join("/tmp", "vops-test-"+os.Getenv("USER"))
		for _, d := range []string{root, run} {
			if setupErr = os.MkdirAll(d, 0o700); setupErr != nil {
				return
			}
		}
		// test binaries of different packages run in parallel and share this podman: one at a time
		lock, err := os.OpenFile(filepath.Join(root, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
		lockFile = lock // kept open (and referenced) for the life of the test binary
		if setupErr = err; err != nil {
			return
		}
		if setupErr = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); setupErr != nil {
			return
		}
		conf := filepath.Join(root, "containers.conf")
		os.WriteFile(conf, []byte("[network]\nnetwork_backend = \"netavark\"\n[engine]\nevents_logger = \"file\"\n"), 0o644)
		wrapper = filepath.Join(root, "podman")
		script := "#!/bin/sh\nexport CONTAINERS_CONF_OVERRIDE=" + conf + "\nexec podman --root " + filepath.Join(root, "storage") + " --runroot " + run + " \"$@\"\n"
		setupErr = os.WriteFile(wrapper, []byte(script), 0o755)
	})
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	t.Setenv("VOPS_PODMAN", wrapper)
	return wrapper
}

// AppImage builds the test app (see app/) into a scratch image and returns its name.
func AppImage(t testing.TB) string {
	t.Helper()
	Podman(t)
	_, file, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(file), "app")
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "app"), ".")
	build.Dir = src
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build app: %v\n%s", err, out)
	}
	bin, _ := os.ReadFile(filepath.Join(dir, "app"))
	sum := sha256.Sum256(bin)
	name := "localhost/vops-test-app:" + hex.EncodeToString(sum[:6])
	ctx := context.Background()
	if podman.ImageID(ctx, name) != "" {
		return name
	}
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM scratch\nCOPY app /app\nENTRYPOINT [\"/app\"]\n"), 0o644)
	if out, err := podman.Run(ctx, "build", "-q", "-t", name, dir); err != nil {
		t.Fatalf("podman build: %v %s", err, out)
	}
	return name
}

// Cleanup removes containers, networks and volumes carrying label on test end.
func Cleanup(t testing.TB, label string) {
	t.Helper()
	clean := func() {
		ctx := context.Background()
		podman.Run(ctx, "rm", "-af", "-t", "0", "--filter", "label="+label)
		podman.Run(ctx, "network", "prune", "-f", "--filter", "label="+label)
		podman.Run(ctx, "volume", "prune", "-f", "--filter", "label="+label)
	}
	clean()
	t.Cleanup(clean)
}
