package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/store"
	"github.com/sh-lucas/vops/internal/testenv"
)

type env struct {
	t     *testing.T
	e     *Engine
	repo  string
	app   string
	proxy *httptest.Server
	ns    string // unique project prefix so parallel runs don't collide
}

func newEnv(t *testing.T) *env {
	testenv.Podman(t)
	app := testenv.AppImage(t)
	Drain = 300 * time.Millisecond
	dir := t.TempDir()
	repo := filepath.Join(dir, "vops")
	os.MkdirAll(repo, 0o755)
	run(t, repo, "git", "init", "-q", "-b", "main")
	run(t, repo, "git", "config", "user.email", "t@t")
	run(t, repo, "git", "config", "user.name", "t")
	db, err := store.Open(filepath.Join(dir, "vops.db"), filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ns := fmt.Sprintf("t%d", time.Now().UnixNano()%1e6)
	testenv.Cleanup(t, LProject)
	e := &Engine{Repo: repo, DB: db, Routes: proxy.NewTable()}
	srv := httptest.NewServer(proxy.Handler(e.Routes))
	t.Cleanup(srv.Close)
	return &env{t: t, e: e, repo: repo, app: app, proxy: srv, ns: ns}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

// commit writes files (path -> content, "" deletes) and commits them.
func (v *env) commit(files map[string]string) {
	v.t.Helper()
	for p, c := range files {
		full := filepath.Join(v.repo, p)
		if c == "" {
			os.RemoveAll(full)
			continue
		}
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(strings.ReplaceAll(c, "APP", v.app)), 0o644)
	}
	run(v.t, v.repo, "git", "add", "-A")
	run(v.t, v.repo, "git", "commit", "-q", "--allow-empty", "-m", "c")
}

func (v *env) apply(opts ApplyOpts) (*Plan, string, error) {
	v.t.Helper()
	var buf bytes.Buffer
	plan, err := v.e.Apply(context.Background(), &buf, opts)
	return plan, buf.String(), err
}

func (v *env) mustApply() *Plan {
	v.t.Helper()
	plan, out, err := v.apply(ApplyOpts{})
	if err != nil {
		v.t.Fatalf("apply: %v\n%s", err, out)
	}
	return plan
}

func (v *env) get(host string) (int, string) {
	req, _ := http.NewRequest("GET", v.proxy.URL, nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (v *env) containers(project string) []podman.Container {
	cs, err := podman.PS(context.Background(), LProject+"="+project)
	if err != nil {
		v.t.Fatal(err)
	}
	return cs
}

func actions(p *Plan) string {
	var out []string
	for _, pp := range p.Projects {
		for _, a := range pp.Actions {
			out = append(out, pp.Path+"/"+a.Service+":"+a.Kind)
		}
		if pp.Error != "" {
			out = append(out, pp.Path+":error")
		}
	}
	return strings.Join(out, " ")
}

// The whole lifecycle of a routed project: create, no-op, rolling update under load with zero failed
// requests, a failed rollout that keeps the old version serving, env changes, disable, and removal.
func TestRollingLifecycle(t *testing.T) {
	v := newEnv(t)
	p := v.ns + "/web"
	host := "web." + v.ns + ".test"
	compose := `services:
  app:
    image: APP
    environment: [MSG]
    x-vops: {port: 8080, health: /health, domains: [` + host + `]}
`
	v.e.DB.SetEnv(p, "MSG", "v1")
	v.commit(map[string]string{p + "/compose.yml": compose})

	plan, err := v.e.Plan(context.Background())
	if err != nil || actions(plan) != p+"/app:create" {
		t.Fatalf("plan: %v %s", err, actions(plan))
	}
	v.mustApply()
	if code, body := v.get(host); code != 200 || body != "v1" {
		t.Fatalf("after create: %d %q", code, body)
	}
	if plan := v.mustApply(); plan.Changes() {
		t.Fatalf("second apply is not a no-op: %s", actions(plan))
	}

	// rolling update under load: every request must succeed, and switch from v1 to v2
	v.e.DB.SetEnv(p, "MSG", "v2")
	if plan, _ := v.e.Plan(context.Background()); actions(plan) != p+"/app:update" {
		t.Fatalf("env change not detected: %s", actions(plan))
	}
	var fails, total atomic.Int64
	seen := sync.Map{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				code, body := v.get(host)
				total.Add(1)
				if code != 200 {
					fails.Add(1)
					t.Logf("failed request: %d %s", code, body)
				}
				seen.Store(body, true)
			}
		})
	}
	time.Sleep(200 * time.Millisecond)
	v.mustApply()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if fails.Load() > 0 {
		t.Fatalf("%d of %d requests failed during the rollout", fails.Load(), total.Load())
	}
	t.Logf("%d requests during rollout, 0 failed", total.Load())
	if _, ok := seen.Load("v2"); !ok {
		t.Fatal("never saw v2")
	}
	if code, body := v.get(host); body != "v2" {
		t.Fatalf("after rollout: %d %q", code, body)
	}
	if cs := v.containers(p); len(cs) != 1 {
		t.Fatalf("old replica not removed: %d containers", len(cs))
	}

	// a version that never becomes ready: rollout fails, v2 keeps serving, the broken replica is gone
	v.e.DB.SetEnv(p, "NEVER_READY", "1")
	old := v.containers(p)[0].ID
	v.commit(map[string]string{p + "/compose.yml": strings.NewReplacer("environment: [MSG]", "environment: [MSG, NEVER_READY]", "health: /health", "health: /health, timeout: 2s").Replace(compose)})
	_, out, err := v.apply(ApplyOpts{})
	if err == nil || !strings.Contains(out, "not ready") || !strings.Contains(out, "old replicas kept serving") {
		t.Fatalf("expected a failed rollout, got %v\n%s", err, out)
	}
	if code, body := v.get(host); code != 200 || body != "v2" {
		t.Fatalf("old version not serving after failed rollout: %d %q", code, body)
	}
	if cs := v.containers(p); len(cs) != 1 || cs[0].ID != old {
		t.Fatalf("expected only the old replica, got %d", len(cs))
	}

	// fix it and scale to 2 replicas; both serve
	v.commit(map[string]string{p + "/compose.yml": strings.Replace(compose, "health: /health", "health: /health, replicas: 2", 1)})
	v.mustApply()
	if cs := v.containers(p); len(cs) != 2 {
		t.Fatalf("replicas: %d", len(cs))
	}
	if r := v.e.Routes.Routes(); len(r) != 1 || len(r[0].Backends) != 2 {
		t.Fatalf("routes: %+v", r)
	}

	// routes survive a daemon restart (rebuilt from podman labels)
	v.e.Routes.Replace(nil)
	if code, _ := v.get(host); code != 404 {
		t.Fatal("expected 404 with empty table")
	}
	v.e.RefreshRoutes(context.Background())
	if code, body := v.get(host); code != 200 || body != "v2" {
		t.Fatalf("after refresh: %d %q", code, body)
	}

	// containers stopped behind our back (reboot): plan says start, StartStopped fixes it
	for _, c := range v.containers(p) {
		podman.Run(context.Background(), "stop", "-t", "0", c.ID)
	}
	if plan, _ := v.e.Plan(context.Background()); actions(plan) != p+"/app:start" {
		t.Fatalf("stopped: %s", actions(plan))
	}
	v.e.StartStopped(context.Background(), io.Discard)
	if code, _ := v.get(host); code != 200 {
		t.Fatalf("after StartStopped: %d", code)
	}

	// disable: containers removed and route gone; enable: back
	v.e.DB.SetDisabled(p, true)
	v.mustApply()
	if len(v.containers(p)) != 0 {
		t.Fatal("disabled project still has containers")
	}
	if code, _ := v.get(host); code != 404 {
		t.Fatalf("disabled project still routed: %d", code)
	}
	v.e.DB.SetDisabled(p, false)
	v.mustApply()
	if code, body := v.get(host); code != 200 || body != "v2" {
		t.Fatalf("re-enabled: %d %q", code, body)
	}

	// removing the directory from git removes the project
	v.commit(map[string]string{p: ""})
	if plan, _ := v.e.Plan(context.Background()); !strings.Contains(actions(plan), p+"/app:remove") || !plan.Projects[0].Gone {
		t.Fatalf("gone: %s", actions(plan))
	}
	v.mustApply()
	if len(v.containers(p)) != 0 {
		t.Fatal("removed project still has containers")
	}
	if plan := v.mustApply(); len(plan.Projects) != 0 {
		t.Fatalf("project still planned: %s", actions(plan))
	}
}

// Recreate strategy, dependency order, dns between services and between projects, volumes, bad compose.
func TestProjectsTalkToEachOther(t *testing.T) {
	v := newEnv(t)
	api, front := v.ns+"/api", v.ns+"/front"
	v.commit(map[string]string{
		api + "/compose.yml": `services:
  db:
    image: APP
    environment: {MSG: from-db, PORT: "9000"}
    volumes: [data:/data, ./files:/files]
  server:
    image: APP
    depends_on: [db]
    environment: {MSG: from-api}
volumes:
  data:
`,
		front + "/compose.yml": `services:
  web:
    image: APP
    command: ["sleep"]
`,
		v.ns + "/broken/compose.yml": "services:\n  x:\n    image: APP\n    networks: [nope]\n",
	})
	plan, out, err := v.apply(ApplyOpts{})
	if err == nil || !strings.Contains(err.Error(), v.ns+"/broken") {
		t.Fatalf("broken project should fail alone: %v\n%s", err, out)
	}
	if !strings.Contains(actions(plan), api+"/db:create "+api+"/server:create") {
		t.Fatalf("dependency order: %s", actions(plan))
	}
	if _, err := os.Stat(filepath.Join(v.repo, api, "files")); err != nil {
		t.Fatal("bind mount dir not created")
	}
	ctx := context.Background()
	exec := func(project, service string, args ...string) (string, error) {
		cs, _ := podman.PS(ctx, LProject+"="+project, LService+"="+service)
		if len(cs) == 0 {
			t.Fatalf("no container for %s/%s", project, service)
		}
		return podman.Run(ctx, append([]string{"exec", cs[0].ID, "/app"}, args...)...)
	}
	// same project: by service name
	if out, err := exec(api, "server", "get", "http://db:9000/"); err != nil || out != "from-db" {
		t.Fatalf("server -> db: %q %v", out, err)
	}
	// other project: <service>.<project reversed>
	dbName := "db.api." + v.ns
	if out, err := exec(front, "web", "get", "http://"+dbName+":9000/"); err != nil || out != "from-db" {
		t.Fatalf("front -> %s: %q %v", dbName, out, err)
	}
	// but not by short name from another project
	if _, err := exec(front, "web", "lookup", "db"); err == nil {
		t.Fatal("short names must not leak across projects")
	}

	// recreate: new container id, volume kept
	before := v.containers(api)
	v.commit(map[string]string{api + "/compose.yml": strings.Replace(mustRead(t, filepath.Join(v.repo, api, "compose.yml")), "from-db", "from-db-2", 1)})
	plan, out, err = v.apply(ApplyOpts{Projects: []string{api}})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if actions(plan) == "" || !strings.Contains(out, "stopping 1 old replica") {
		t.Fatalf("expected recreate: %s", out)
	}
	after := v.containers(api)
	if len(after) != 2 || after[0].ID == before[0].ID && after[1].ID == before[1].ID {
		t.Fatal("db not recreated")
	}
	if _, err := podman.Run(ctx, "volume", "exists", "vops-"+strings.ToLower(v.ns)+".api-data"); err != nil {
		t.Fatal("volume missing")
	}
}

// Images built from the repo are rebuilt only when their context changes.
func TestBuildContext(t *testing.T) {
	v := newEnv(t)
	p := v.ns + "/built"
	v.commit(map[string]string{
		p + "/compose.yml":       "services:\n  s:\n    build: ./ctx\n    x-vops: {port: 8080, domains: [b." + v.ns + ".test]}\n",
		p + "/ctx/Containerfile": "FROM APP\nENV MSG=built-1\n",
	})
	v.mustApply()
	if code, body := v.get("b." + v.ns + ".test"); code != 200 || body != "built-1" {
		t.Fatalf("%d %q", code, body)
	}
	// unrelated change: nothing to do
	v.commit(map[string]string{p + "/README.md": "hi"})
	if plan, _ := v.e.Plan(context.Background()); plan.Changes() {
		t.Fatalf("unrelated change triggers deploy: %s", actions(plan))
	}
	v.commit(map[string]string{p + "/ctx/Containerfile": "FROM APP\nENV MSG=built-2\n"})
	v.mustApply()
	if _, body := v.get("b." + v.ns + ".test"); body != "built-2" {
		t.Fatalf("not rebuilt: %q", body)
	}
	t.Cleanup(func() {
		podman.Run(context.Background(), "image", "prune", "-f", "--filter", "reference=localhost/vops/*")
	})
}

func mustRead(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
