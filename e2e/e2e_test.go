// Package e2e drives the real vops binary end to end. ssh is replaced by a script that runs the
// remote command locally with HOME set to a fake host home; everything else (git over "ssh", the
// daemon, podman, the registry, the proxy) is real.
package e2e

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/snapshot"
	"github.com/sh-lucas/vops/internal/testenv"
)

const fakeSSH = `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in -p|-i|-o|-L|-l|-E|-F) shift 2;; -*) shift;; *) break;; esac
done
shift
cd "$FAKE_HOME" && exec env HOME="$FAKE_HOME" sh -c "$*"
`

type world struct {
	t        *testing.T
	bin      string
	env      []string
	hostHome string
	httpAddr string
	uiAddr   string
	app      string
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func setup(t *testing.T) *world {
	wrapper := testenv.Podman(t)
	app := testenv.AppImage(t)
	testenv.Cleanup(t, "vops.project")
	dir := t.TempDir()
	bin := filepath.Join(dir, "vops")
	build := exec.Command("go", "build", "-o", bin, "../cmd/vops")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ssh := filepath.Join(dir, "fake-ssh")
	os.WriteFile(ssh, []byte(fakeSSH), 0o755)
	hostHome := filepath.Join(dir, "host")
	os.MkdirAll(filepath.Join(hostHome, ".vops"), 0o700)
	w := &world{t: t, bin: bin, hostHome: hostHome, httpAddr: freeAddr(t), uiAddr: freeAddr(t), app: app}
	w.env = append(os.Environ(),
		"VOPS_SSH="+ssh, "FAKE_HOME="+hostHome, "VOPS_PODMAN="+wrapper, "VOPS_NO_SYSTEMD=1",
		"GIT_AUTHOR_NAME=dev", "GIT_AUTHOR_EMAIL=dev@x", "GIT_COMMITTER_NAME=dev", "GIT_COMMITTER_EMAIL=dev@x",
		"HOME="+filepath.Join(dir, "devhome"),
	)
	return w
}

func (w *world) setConfig(dir string, fields map[string]string) {
	w.t.Helper()
	path := filepath.Join(dir, "vops.yml")
	b, _ := os.ReadFile(path)
	b, err := config.SetFields(b, fields)
	if err != nil {
		w.t.Fatal(err)
	}
	os.WriteFile(path, b, 0o644)
}

// vops runs the cli in dir and fails the test on error.
func (w *world) vops(dir string, args ...string) string {
	w.t.Helper()
	out, err := w.try(dir, "", args...)
	if err != nil {
		w.t.Fatalf("vops %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (w *world) try(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command(w.bin, args...)
	cmd.Dir, cmd.Env = dir, w.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (w *world) git(dir string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, w.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (w *world) write(dir string, files map[string]string) {
	for p, c := range files {
		full := filepath.Join(dir, p)
		if c == "" {
			os.RemoveAll(full)
			continue
		}
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(strings.ReplaceAll(strings.ReplaceAll(c, "APP", w.app), "UI", w.uiAddr)), 0o644)
	}
	w.git(dir, "add", "-A")
	w.git(dir, "commit", "-q", "-m", "change")
}

func (w *world) get(host string) (int, string) {
	req, _ := http.NewRequest("GET", "http://"+w.httpAddr+"/", nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (w *world) eventually(what string, fn func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (w *world) startDaemon() {
	cmd := exec.Command(filepath.Join(w.hostHome, ".vops", "bin", "vops"), "daemon")
	cmd.Env = append(w.env, "HOME="+w.hostHome)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
		// read-only snapshots and subvolumes: t.TempDir can't remove them by itself
		snapshot.Delete(context.Background(), filepath.Join(w.hostHome, ".vops", "snapshots"))
		snapshot.Delete(context.Background(), filepath.Join(w.hostHome, ".vops", "previews"))
		snapshot.Delete(context.Background(), filepath.Join(w.hostHome, "vops"))
		if w.t.Failed() {
			w.t.Logf("daemon logs:\n%s", logs.String())
		}
	})
	w.eventually("daemon socket", func() bool {
		_, err := os.Stat(filepath.Join(w.hostHome, ".vops", "vops.sock"))
		return err == nil
	})
}

func TestEndToEnd(t *testing.T) {
	w := setup(t)
	dev := filepath.Join(t.TempDir(), "infra")
	os.MkdirAll(dev, 0o755)

	// init + install
	w.vops(dev, "init", "--domain", "vops.test", "--email", "me@vops.test")
	for _, f := range []string{"vops.yml", "registry/.gitignore", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dev, f)); err != nil {
			t.Fatalf("init did not create %s", f)
		}
	}
	// the whole config lives in the repo: listeners for this test, no acme in here
	w.setConfig(dev, map[string]string{"http": w.httpAddr, "https": "off", "ui": w.uiAddr, "tls": "off"})
	out := w.vops(dev, "install", "dev@fakehost", "--binary", w.bin)
	m := regexp.MustCompile(`dashboard login: admin / (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no admin password in install output:\n%s", out)
	}
	adminPW := m[1]
	if lock, _ := os.ReadFile(filepath.Join(dev, "vops-lock.yml")); !strings.Contains(string(lock), "dev@fakehost") {
		t.Fatalf("lock: %s", lock)
	}
	if again := w.vops(dev, "install", "dev@fakehost", "--binary", w.bin); strings.Contains(again, "dashboard login") {
		t.Fatal("reinstall must not reset the admin password")
	}
	w.startDaemon()

	// first project: env set before the first sync, then sync
	w.vops(dev, "env", "set", "shop", "MSG=hello")
	w.write(dev, map[string]string{"shop/compose.yml": `services:
  web:
    image: APP
    environment: [MSG]
    x-vops: {port: 8080, health: /health}
`})
	out = w.vops(dev, "sync", "--yes")
	if !strings.Contains(out, "+ web") {
		t.Fatalf("sync output has no plan:\n%s", out)
	}
	for _, host := range []string{"shop.vops.test", "web.shop.vops.test"} {
		if code, body := w.get(host); code != 200 || body != "hello" {
			t.Fatalf("%s: %d %q", host, code, body)
		}
	}
	if st := w.vops(dev, "status"); !strings.Contains(st, "shop") || !strings.Contains(st, "1/1 running") {
		t.Fatalf("status:\n%s", st)
	}
	if out := w.vops(dev, "sync", "--yes"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("second sync should be a no-op:\n%s", out)
	}

	// env change + apply
	w.vops(dev, "env", "set", "shop", "MSG=bye")
	if keys := w.vops(dev, "env", "ls", "shop"); !strings.Contains(keys, "MSG") || strings.Contains(keys, "bye") {
		t.Fatalf("env ls must list keys only: %s", keys)
	}
	w.vops(dev, "apply", "--yes")
	if _, body := w.get("shop.vops.test"); body != "bye" {
		t.Fatalf("after env change: %q", body)
	}
	if logs := w.vops(dev, "logs", "shop", "web", "-n", "50"); !strings.Contains(logs, "listening on 8080") {
		t.Fatalf("logs:\n%s", logs)
	}
	// apply without a terminal and without --yes refuses
	w.vops(dev, "env", "set", "shop", "MSG=again")
	if out, err := w.try(dev, "", "apply"); err == nil || !strings.Contains(out, "--yes") {
		t.Fatalf("apply without a tty should refuse: %v\n%s", err, out)
	}
	w.vops(dev, "apply", "--yes")

	// registry: a user for shop/*, push, deploy from it, push again -> redeployed by itself
	out = w.vops(dev, "user", "add", "ci", "--pattern", "shop/.*")
	tm := regexp.MustCompile(`token: (\S+)`).FindStringSubmatch(out)
	if tm == nil {
		t.Fatalf("no token:\n%s", out)
	}
	ctx := context.Background()
	push := func(msg, creds string) error {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+w.app+"\nENV MSG="+msg+"\n"), 0o644)
		ref := w.uiAddr + "/shop/api:v1"
		if _, err := podman.Run(ctx, "build", "-q", "-t", ref, dir); err != nil {
			return err
		}
		_, err := podman.Run(ctx, "push", "-q", "--tls-verify=false", "--creds", creds, ref)
		return err
	}
	if err := push("pushed-1", "ci:"+tm[1]); err != nil {
		t.Fatal(err)
	}
	if err := push("nope", "admin:"+adminPW); err == nil {
		t.Fatal("admin must not push")
	}
	// migrating from another registry: keep the password CI already has
	if out, err := w.try(dev, "legacy-password-123\n", "user", "add", "legacy", "--pattern", "shop/.*", "--token-stdin"); err != nil || strings.Contains(out, "legacy-password-123") {
		t.Fatalf("token-stdin: %v\n%s", err, out)
	}
	if err := push("pushed-1", "legacy:legacy-password-123"); err != nil {
		t.Fatal("chosen token rejected:", err)
	}
	w.write(dev, map[string]string{"api/compose.yml": "services:\n  api:\n    image: UI/shop/api:v1\n    x-vops: {port: 8080}\n"})
	w.vops(dev, "sync", "--yes")
	if code, body := w.get("api.vops.test"); code != 200 || body != "pushed-1" {
		t.Fatalf("api: %d %q", code, body)
	}
	if err := push("pushed-2", "ci:"+tm[1]); err != nil {
		t.Fatal(err)
	}
	w.eventually("redeploy after push", func() bool { _, body := w.get("api.vops.test"); return body == "pushed-2" })
	if ls := w.vops(dev, "registry", "ls"); !strings.Contains(ls, "shop/api:v1") {
		t.Fatalf("registry ls:\n%s", ls)
	}

	// a second developer clones from the host, changes something and syncs; the first one pulls it on sync
	dev2 := filepath.Join(t.TempDir(), "infra2")
	cmd := exec.Command("git", "clone", "-q", "ssh://dev@fakehost/~/vops", dev2)
	cmd.Env = append(w.env, "GIT_SSH_COMMAND="+filepath.Join(filepath.Dir(w.bin), "fake-ssh"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	w.vops(dev2, "install", "dev@fakehost", "--binary", w.bin) // links the clone (lock + remote)
	w.write(dev2, map[string]string{"shop/compose.yml": `services:
  web:
    image: APP
    environment: [MSG]
    x-vops: {port: 8080, health: /health, replicas: 2}
`})
	w.vops(dev2, "sync", "--yes")
	if st := w.vops(dev, "status"); !strings.Contains(st, "2/2 running") {
		t.Fatalf("replicas from dev2:\n%s", st)
	}
	w.write(dev, map[string]string{"README.md": "my infra\n"})
	w.vops(dev, "sync", "--yes")
	if log := w.git(dev, "log", "--oneline"); strings.Count(log, "\n") < 5 {
		t.Fatalf("dev did not merge dev2's commit:\n%s", log)
	}
	if b, _ := os.ReadFile(filepath.Join(dev, "shop", "compose.yml")); !strings.Contains(string(b), "replicas: 2") {
		t.Fatal("dev's working tree lacks dev2's change")
	}

	// dashboard api: login, csrf header, env values never readable
	client := &http.Client{}
	login, _ := http.NewRequest("POST", "http://"+w.uiAddr+"/api/login", strings.NewReader(`{"password":"`+adminPW+`"}`))
	login.Header.Set("X-Vops", "1")
	resp, err := client.Do(login)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login: %v %v", err, resp.Status)
	}
	cookie := resp.Cookies()[0]
	call := func(method, path, body string, csrf bool) (int, string) {
		req, _ := http.NewRequest(method, "http://"+w.uiAddr+path, strings.NewReader(body))
		req.AddCookie(cookie)
		if csrf {
			req.Header.Set("X-Vops", "1")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := call("GET", "/api/status", "", false)
	var st struct {
		Projects []struct{ Path string } `json:"projects"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || len(st.Projects) != 2 {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, body := call("GET", "/api/audit", "", false); code != 200 || !strings.Contains(body, `"tbl":"env"`) {
		t.Fatalf("audit api: %d %s", code, body)
	}
	if code, _ := call("POST", "/api/env", `{"project":"shop","key":"X","value":"y"}`, false); code != 403 {
		t.Fatalf("post without X-Vops: %d", code)
	}
	if _, body := call("GET", "/api/env?project=shop", "", false); strings.Contains(body, "again") || !strings.Contains(body, "MSG") {
		t.Fatalf("env api leaked or lost values: %s", body)
	}
	if code, _ := call("POST", "/api/admin/password", `{"password":"xxxxxxxxxxxxxxxx"}`, true); code != 404 && code != 405 {
		t.Fatalf("admin password must only be settable over the socket: %d", code)
	}
	if code, body := call("GET", "/", "", false); code != 200 || !strings.Contains(body, "vops") {
		t.Fatalf("ui index: %d", code)
	}
	req, _ := http.NewRequest("GET", "http://"+w.uiAddr+"/api/status", nil)
	if resp, _ := client.Do(req); resp.StatusCode != 401 {
		t.Fatalf("status without session: %d", resp.StatusCode)
	}

	// data safety from the cli: a project with a volume, a manual snapshot, a deploy (pre-deploy snapshot), a rollback
	notes := "services:\n  notes:\n    image: APP\n    environment: {V: \"%s\"}\n    volumes: [\"data:/data:U\"]\nvolumes:\n  data:\n"
	w.write(dev, map[string]string{"notes/compose.yml": fmt.Sprintf(notes, "1")})
	w.vops(dev, "sync", "--yes")
	execNotes := func(args ...string) string {
		cs, _ := podman.PS(ctx, "vops.project=notes")
		out, err := podman.Run(ctx, append([]string{"exec", cs[0].ID, "/app"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	execNotes("write", "/data/f", "before")
	if out := w.vops(dev, "snapshot", "create", "notes", "-m", "by hand"); !strings.Contains(out, "(manual)") {
		t.Fatalf("snapshot create:\n%s", out)
	}
	w.write(dev, map[string]string{"notes/compose.yml": fmt.Sprintf(notes, "2")})
	if out := w.vops(dev, "sync", "--yes"); !strings.Contains(out, "(pre-deploy)") {
		t.Fatalf("sync should snapshot first:\n%s", out)
	}
	execNotes("write", "/data/f", "after")
	if ls := w.vops(dev, "snapshot", "ls", "notes"); !strings.Contains(ls, "protected: vops-notes-data") || !strings.Contains(ls, "pre-deploy") || !strings.Contains(ls, "by hand") {
		t.Fatalf("snapshot ls:\n%s", ls)
	}
	if out := w.vops(dev, "rollback", "notes", "--yes"); !strings.Contains(out, "data restored to #") || !strings.Contains(out, "undo with: vops rollback notes") {
		t.Fatalf("rollback:\n%s", out)
	}
	if got := execNotes("read", "/data/f"); got != "before" {
		t.Fatalf("after rollback: %q", got)
	}
	if audit := w.vops(dev, "audit"); !strings.Contains(audit, "snapshots insert notes") || !strings.Contains(audit, "env") || strings.Contains(audit, "again") {
		t.Fatalf("audit:\n%s", audit)
	}

	// removing a project from git removes its containers
	w.write(dev, map[string]string{"api": ""})
	w.vops(dev, "sync", "--yes")
	if code, _ := w.get("api.vops.test"); code != 404 {
		t.Fatalf("removed project still served: %d", code)
	}
	cs, _ := podman.PS(ctx, "vops.project=api")
	if len(cs) != 0 {
		t.Fatalf("removed project still has %d containers", len(cs))
	}
	if ev := w.vops(dev, "events"); !strings.Contains(ev, "push") || !strings.Contains(ev, "shop") {
		t.Fatalf("events:\n%s", ev)
	}

	// vops.yml is desired state too: a change shows in the plan, apply writes the host lock,
	// and new listeners restart the daemon in place (containers keep serving)
	lock := filepath.Join(w.hostHome, ".vops", "config.yml")
	if b, _ := os.ReadFile(lock); !strings.Contains(string(b), w.httpAddr) || !strings.Contains(string(b), "vops.test") {
		t.Fatalf("install did not seed the lock from vops.yml:\n%s", b)
	}
	newUI := freeAddr(t)
	w.setConfig(dev, map[string]string{"ui": newUI, "snapshot_keep": "3"})
	w.git(dev, "commit", "-qam", "config")
	out = w.vops(dev, "sync", "--yes")
	if !strings.Contains(out, "~ ui: "+w.uiAddr+" -> "+newUI) || !strings.Contains(out, "~ snapshot_keep: 5 -> 3") || !strings.Contains(out, "daemon restarts") {
		t.Fatalf("config change not planned:\n%s", out)
	}
	if b, _ := os.ReadFile(lock); !strings.Contains(string(b), newUI) || !strings.Contains(string(b), "snapshot_keep: 3") {
		t.Fatalf("lock not updated:\n%s", b)
	}
	w.eventually("dashboard on the new port", func() bool {
		resp, err := http.Get("http://" + newUI + "/")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil && resp.StatusCode == 200
	})
	if code, body := w.get("shop.vops.test"); code != 200 || body != "again" {
		t.Fatalf("site after daemon restart: %d %q", code, body)
	}
	if out := w.vops(dev, "plan"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("config should be in sync:\n%s", out)
	}
	// a broken vops.yml never reaches the daemon: plan warns and keeps the config in effect
	w.setConfig(dev, map[string]string{"tls": "maybe"})
	w.git(dev, "commit", "-qam", "broken config")
	if out, err := w.try(dev, "", "sync", "--yes"); err != nil || !strings.Contains(out, "keeping the config in effect") {
		t.Fatalf("broken config: %v\n%s", err, out)
	}
}
