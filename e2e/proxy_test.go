package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	vproxy "github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/snapshot"
)

// The proxy is its own process: the daemon dying (or being upgraded) never takes the sites down,
// and the proxy restarts with its last table even when the daemon is gone.
func TestProxyOwnProcess(t *testing.T) {
	w := setup(t)
	dev := filepath.Join(t.TempDir(), "infra")
	os.MkdirAll(dev, 0o755)
	w.vops(dev, "init", "--domain", "vops.test")
	w.setConfig(dev, map[string]string{"http": w.httpAddr, "https": "off", "ui": w.uiAddr, "tls": "off"})
	w.vops(dev, "install", "dev@fakehost", "--binary", w.bin)
	proxy := w.startProc("proxy")
	daemon := w.startProc("daemon")
	t.Cleanup(func() { snapshot.Delete(context.Background(), filepath.Join(w.hostHome, "vops")) })

	w.vops(dev, "env", "set", "shop", "MSG=v1")
	w.write(dev, map[string]string{"shop/compose.yml": "services:\n  web:\n    image: APP\n    environment: [MSG]\n    x-vops: {port: 8080, health: /health, replicas: 2}\n"})
	w.vops(dev, "sync", "--yes")
	if code, body := w.get("shop.vops.test"); code != 200 || body != "v1" {
		t.Fatalf("site: %d %q", code, body)
	}
	if st := w.vops(dev, "status"); !strings.Contains(st, fmt.Sprintf("proxy up (v%d, 1 routes)", vproxy.Version)) || strings.Contains(st, "! the proxy") {
		t.Fatalf("status should show the proxy in sync:\n%s", st)
	}

	// rolling release under load through the separate proxy: the switch is synchronous, so 0 failures
	var n, failed atomic.Int64
	stop := make(chan struct{})
	loadDone := make(chan struct{})
	go func() {
		defer close(loadDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n.Add(1)
			if code, _ := w.get("shop.vops.test"); code != 200 {
				failed.Add(1)
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	w.vops(dev, "env", "set", "shop", "MSG=v2")
	w.vops(dev, "apply", "--yes")
	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-loadDone
	if _, body := w.get("shop.vops.test"); body != "v2" || failed.Load() != 0 {
		t.Fatalf("rolling release through the proxy: %d/%d failed, now %q", failed.Load(), n.Load(), body)
	}
	t.Logf("rolling release: %d requests, 0 failed", n.Load())

	// the daemon dies hard: sites keep answering, its own hosts say it is down
	daemon.Process.Signal(syscall.SIGKILL)
	daemon.Wait()
	for range 20 {
		if code, body := w.get("shop.vops.test"); code != 200 || body != "v2" {
			t.Fatalf("site with the daemon dead: %d %q", code, body)
		}
	}
	if code, body := w.get("vops.vops.test"); code != 502 || !strings.Contains(body, "daemon is not running") {
		t.Fatalf("dashboard host with the daemon dead: %d %q", code, body)
	}

	// the proxy restarts with the daemon still dead: routes come back from disk
	proxy.Process.Signal(syscall.SIGKILL)
	proxy.Wait()
	if code, _ := w.get("shop.vops.test"); code != 0 {
		t.Fatalf("nothing should listen now: %d", code)
	}
	proxy = w.startProc("proxy")
	if code, body := w.get("shop.vops.test"); code != 200 || body != "v2" {
		t.Fatalf("site after a proxy restart without daemon: %d %q", code, body)
	}

	// the daemon is back; then the proxy is down during a rolling release: the switch can't be confirmed, so the
	// old replicas keep serving and the deploy fails; the proxy comes back with them, the next apply goes through
	w.startProc("daemon")
	if code, body := w.get("vops.vops.test"); code != 200 || !strings.Contains(body, "vops") {
		t.Fatalf("dashboard through the proxy: %d", code)
	}
	proxy.Process.Signal(syscall.SIGKILL)
	proxy.Wait()
	if st := w.vops(dev, "status"); !strings.Contains(st, "proxy down") || !strings.Contains(st, "! the proxy is not running") {
		t.Fatalf("status should warn that the proxy is down:\n%s", st)
	}
	w.vops(dev, "env", "set", "shop", "MSG=v3")
	if out, err := w.try(dev, "", "apply", "--yes"); err == nil || !strings.Contains(out, "proxy did not confirm the new routes") {
		t.Fatalf("rolling release with the proxy down: %v\n%s", err, out)
	}
	w.startProc("proxy")
	if code, body := w.get("shop.vops.test"); code != 200 || body != "v2" {
		t.Fatalf("old replicas after the failed release: %d %q", code, body)
	}
	w.eventually("status in sync", func() bool {
		st := w.vops(dev, "status")
		return strings.Contains(st, fmt.Sprintf("proxy up (v%d, 1 routes)", vproxy.Version)) && !strings.Contains(st, "! the proxy")
	})
	w.vops(dev, "apply", "--yes")
	if _, body := w.get("shop.vops.test"); body != "v3" {
		t.Fatalf("after the proxy is back: %q", body)
	}
	if ev := w.vops(dev, "events"); !strings.Contains(ev, "proxy unreachable") || !strings.Contains(ev, "proxy reachable again") {
		t.Fatalf("events should record the proxy outage:\n%s", ev)
	}
}

// A cli and a host with different major.minor refuse to talk; install upgrades, and refuses to downgrade.
func TestVersionCheck(t *testing.T) {
	w := setup(t)
	newer := filepath.Join(filepath.Dir(w.bin), "vops-9.9")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.0", "-o", newer, "../cmd/vops")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dev := filepath.Join(t.TempDir(), "infra")
	os.MkdirAll(dev, 0o755)
	w.vops(dev, "init", "--domain", "vops.test")
	w.setConfig(dev, map[string]string{"http": w.httpAddr, "https": "off", "ui": w.uiAddr, "tls": "off"})
	if out := w.vops(dev, "install", "dev@fakehost", "--binary", w.bin); !strings.Contains(out, "installing vops ") {
		t.Fatalf("first install:\n%s", out)
	}
	old := strings.TrimPrefix(strings.TrimSpace(w.vops(dev, "version")), "vops ")
	if out := w.vops(dev, "install", "dev@fakehost", "--binary", w.bin); !strings.Contains(out, "reinstalling vops "+old) {
		t.Fatalf("reinstall:\n%s", out)
	}
	code := func(err error) int {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return -1
	}
	vops9 := func(args ...string) (string, error) {
		cmd := exec.Command(newer, args...)
		cmd.Dir, cmd.Env = dev, w.env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// a newer cli against the host: refused before anything runs there
	if out, err := vops9("status"); code(err) != 3 || !strings.Contains(out, "you have v9.9: run `vops install` to upgrade it") {
		t.Fatalf("newer cli: %v\n%s", err, out)
	}
	if out, err := vops9("install", "dev@fakehost", "--binary", newer); err != nil || !strings.Contains(out, "upgrading vops "+old+" → v9.9.0") {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	// now the cli is the older one
	if out, err := w.try(dev, "", "status"); code(err) != 3 || !strings.Contains(out, "the host runs vops v9.9") || !strings.Contains(out, "update yours (go install") {
		t.Fatalf("older cli: %v\n%s", err, out)
	}
	if out, err := w.try(dev, "", "install", "dev@fakehost", "--binary", w.bin); code(err) != 3 || !strings.Contains(out, "--force") {
		t.Fatalf("downgrade must be refused: %v\n%s", err, out)
	}
	if out := w.vops(dev, "install", "dev@fakehost", "--binary", w.bin, "--force"); !strings.Contains(out, "downgrading vops v9.9.0 → "+old) {
		t.Fatalf("forced downgrade:\n%s", out)
	}
	// version always answers, whatever cli asks
	cmd := exec.Command(filepath.Join(w.hostHome, ".vops", "bin", "vops"), "version")
	cmd.Env = append(w.env, "VOPS_CLIENT=v9.9.0")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), old) {
		t.Fatalf("version with another cli: %v %s", err, out)
	}
}
