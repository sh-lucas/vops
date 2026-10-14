package e2e

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
)

// Release modes through the real daemon and proxy: a CI push to a manual project waits (`vops plan` has nothing
// to do, `vops status` says why), `vops releases approve` deploys it, reject keeps what runs; then canary 50%:
// the proxy splits clients and keeps them (cookie), ?vops-canary=on puts a browser on it, a daemon SIGKILL and
// restart keep the split, and a canary killed by hand is rejected by the monitor.
func TestReleaseModes(t *testing.T) {
	w := setup(t)
	dev := filepath.Join(t.TempDir(), "infra")
	os.MkdirAll(dev, 0o755)
	w.vops(dev, "init", "--domain", "vops.test")
	w.setConfig(dev, map[string]string{"http": w.httpAddr, "https": "off", "ui": w.uiAddr, "tls": "off"})
	w.vops(dev, "install", "dev@fakehost", "--binary", w.bin)
	w.startProc("proxy")
	daemon := w.startProc("daemon")
	t.Cleanup(func() { podman.Run(context.Background(), "rmi", "-f", w.uiAddr+"/shop/web:latest") })

	token := regexp.MustCompile(`token: (\S+)`).FindStringSubmatch(w.vops(dev, "user", "add", "ci", "--repo", "shop/web"))[1]
	ctx := context.Background()
	push := func(msg string) {
		t.Helper()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+w.app+"\nENV MSG="+msg+"\n"), 0o644)
		ref := w.uiAddr + "/shop/web:latest"
		if _, err := podman.Run(ctx, "build", "-q", "-t", ref, dir); err != nil {
			t.Fatal(err)
		}
		if _, err := podman.Run(ctx, "push", "-q", "--tls-verify=false", "--creds", "ci:"+token, ref); err != nil {
			t.Fatal(err)
		}
	}
	body := func() string { _, b := w.get("shop.vops.test"); return b }
	pending := func() string {
		t.Helper()
		var id string
		w.eventually("a pending release", func() bool {
			m := regexp.MustCompile(`(?m)^(\d+)\s+shop\s+.*pending`).FindStringSubmatch(w.vops(dev, "releases"))
			if m != nil {
				id = m[1]
			}
			return m != nil
		})
		return id
	}
	compose := func(mode string) map[string]string {
		return map[string]string{"shop/compose.yml": "x-vops: {release: " + mode + "}\nservices:\n  web:\n    image: UI/shop/web:latest\n    x-vops: {port: 8080, health: /health}\n"}
	}

	push("one")
	w.write(dev, compose("manual"))
	w.vops(dev, "sync", "--yes")
	if body() != "one" {
		t.Fatalf("first deploy: %q", body())
	}
	push("two")
	id := pending()
	if plan := w.vops(dev, "plan"); !strings.Contains(plan, "web: waiting approval of #"+id) || !strings.Contains(plan, "nothing to do") {
		t.Fatalf("plan:\n%s", plan)
	}
	if st := w.vops(dev, "status"); !strings.Contains(st, "waiting approval of #"+id) || strings.Contains(st, "differs from git") || body() != "one" {
		t.Fatalf("status:\n%s", st)
	}
	if out := w.vops(dev, "releases", "approve", id); body() != "two" {
		t.Fatalf("approve: %q\n%s", body(), out)
	}
	if hist := w.vops(dev, "history", "shop"); !strings.Contains(hist, "approve") || !strings.Contains(hist, "r"+id) || !strings.Contains(hist, "approved by cli") {
		t.Fatalf("history:\n%s", hist)
	}
	push("three")
	id = pending()
	w.vops(dev, "releases", "reject", id)
	if out := w.vops(dev, "apply", "--yes"); body() != "two" || !strings.Contains(w.vops(dev, "releases", "shop"), "rejected by cli") {
		t.Fatalf("rejected: %q\n%s", body(), out)
	}

	// canary
	w.write(dev, compose("canary 50%"))
	w.vops(dev, "sync", "--yes")
	push("four")
	id = pending()
	as := func(cookie string) string {
		req, _ := http.NewRequest("GET", "http://"+w.httpAddr+"/", nil)
		req.Host = "shop.vops.test"
		if cookie != "" {
			req.Header.Set("Cookie", proxy.CanaryCookie+"="+cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err.Error()
		}
		defer resp.Body.Close()
		b := make([]byte, 64)
		n, _ := resp.Body.Read(b)
		return string(b[:n])
	}
	w.eventually("the canary", func() bool { return as(id+".on") == "four" })
	split := func(what string) {
		t.Helper()
		seen := map[string]int{}
		for range 40 {
			seen[as("")]++
		}
		if seen["two"] == 0 || seen["four"] == 0 || seen["two"]+seen["four"] != 40 || as(id+".on") != "four" || as(id+".off") != "two" {
			t.Fatalf("%s: %v", what, seen)
		}
	}
	split("split")
	// the dashboard's link: the param sets the cookie and redirects without it
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", "http://"+w.httpAddr+"/?"+proxy.CanaryParam+"=on", nil)
	req.Host = "shop.vops.test"
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" || !strings.Contains(resp.Header.Get("Set-Cookie"), proxy.CanaryCookie+"="+id+".on") {
		t.Fatalf("open the canary: %v %+v", err, resp)
	}
	resp.Body.Close()

	// a daemon crash and restart keep the split (labels, routes.json)
	daemon.Process.Signal(syscall.SIGKILL)
	daemon.Wait()
	split("with the daemon dead")
	w.startProc("daemon")
	w.eventually("the proxy in sync again", func() bool { return strings.Contains(w.vops(dev, "status"), "routes)") && !strings.Contains(w.vops(dev, "status"), "out of sync") })
	split("after a daemon restart")

	// killed by hand: the monitor rejects the release and removes the canary
	cs, _ := podman.PS(ctx, "vops.role=canary")
	podman.Run(ctx, "kill", cs[0].ID)
	w.eventually("the canary rejected", func() bool {
		return strings.Contains(w.vops(dev, "releases", "shop"), "canary failed") && len(must(podman.PS(ctx, "vops.role=canary"))) == 0
	})
	if body() != "two" {
		t.Fatalf("after the canary died: %q", body())
	}
}

func must[T any](v T, _ error) T { return v }
