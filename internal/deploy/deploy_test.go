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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/snapshot"
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
	e := &Engine{Repo: repo, DB: db, Routes: proxy.NewTable(), SnapshotDir: filepath.Join(dir, "snapshots"), PreviewDir: filepath.Join(dir, "previews")}
	// subvolumes and snapshots hold subuid-owned files: remove them from inside the user namespace
	t.Cleanup(func() {
		ctx := context.Background()
		snaps, _ := db.Snapshots("")
		for _, s := range snaps {
			for _, v := range s.Volumes {
				snapshot.Delete(ctx, v.Path)
			}
		}
		snapshot.Delete(ctx, repo)
		snapshot.Delete(ctx, e.SnapshotDir)
		snapshot.Delete(ctx, e.PreviewDir)
	})
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
    volumes: ["data:/data:U", "./files:/files:U"]
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

// Custom and internal networks, aliases, a migration job gating the web service, profiles,
// a failing job that blocks its dependents, and a network whose definition changes.
func TestNetworksJobsProfiles(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	p, other := v.ns+"/app", v.ns+"/other"
	host := "app." + v.ns + ".test"
	compose := func(migrate, msg, internal string) string {
		return `services:
  migrate:
    image: APP
    command: ` + migrate + `
    networks: [backend]
  db:
    image: APP
    environment: {MSG: from-db, PORT: "9000"}
    networks:
      backend: {aliases: [database]}
  web:
    image: APP
    environment: {MSG: ` + msg + `}
    networks: [default, backend, vops]
    depends_on:
      migrate: {condition: service_completed_successfully}
      db: {condition: service_healthy}
    x-vops: {port: 8080, domains: [` + host + `]}
  debug:
    image: APP
    profiles: [debug]
networks:
  backend: {internal: ` + internal + `}
`
	}
	v.commit(map[string]string{
		p + "/compose.yml":     compose(`["lookup", "localhost"]`, "web-1", "true"),
		other + "/compose.yml": "services:\n  probe:\n    image: APP\n",
	})
	plan, out, err := v.apply(ApplyOpts{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := actions(plan); !strings.Contains(got, p+"/db:create "+p+"/migrate:create "+p+"/web:create") || strings.Contains(got, "debug") {
		t.Fatalf("order/profiles: %s", got)
	}
	if code, body := v.get(host); code != 200 || body != "web-1" {
		t.Fatalf("web: %d %q", code, body)
	}
	run := func(project, service string, args ...string) (string, error) {
		cs, _ := podman.PS(ctx, LProject+"="+project, LService+"="+service)
		if len(cs) == 0 {
			t.Fatalf("no container for %s/%s", project, service)
		}
		return podman.Run(ctx, append([]string{"exec", cs[0].ID, "/app"}, args...)...)
	}
	if out, err := run(p, "web", "get", "http://database:9000/"); err != nil || out != "from-db" {
		t.Fatalf("web -> database alias: %q %v", out, err)
	}
	if internal, _ := podman.Run(ctx, "network", "inspect", "--format", "{{.Internal}}", "vops-"+v.ns+".app-backend"); internal != "true" {
		t.Fatalf("backend not internal: %q", internal)
	}
	// web joined the shared network explicitly; db didn't, so other projects can't see it
	if _, err := run(other, "probe", "lookup", "web.app."+v.ns); err != nil {
		t.Fatal("web should be reachable from other projects")
	}
	if _, err := run(other, "probe", "lookup", "db.app."+v.ns); err == nil {
		t.Fatal("db must not be reachable from other projects")
	}
	// the job ran once and is done: nothing to do, it is not rerun
	if cs := v.containers(p); len(cs) != 3 {
		t.Fatalf("%d containers", len(cs))
	}
	if plan, _ := v.e.Plan(ctx); plan.Changes() {
		t.Fatalf("finished job must not be rerun: %s", actions(plan))
	}
	// profiles come from COMPOSE_PROFILES in the project env
	v.e.DB.SetEnv(p, "COMPOSE_PROFILES", "debug")
	if plan, _ := v.e.Plan(ctx); actions(plan) != p+"/db:none "+p+"/debug:create "+p+"/migrate:none "+p+"/web:none "+other+"/probe:none" {
		t.Fatalf("profile on: %s", actions(plan))
	}
	v.e.DB.UnsetEnv(p, "COMPOSE_PROFILES")

	// a failing migration: web (also changed) is skipped and the old web keeps serving
	v.commit(map[string]string{p + "/compose.yml": compose(`["exit"]`, "web-2", "true")})
	_, out, err = v.apply(ApplyOpts{Projects: []string{p}})
	if err == nil || !strings.Contains(out, "failed (exit 3)") || !strings.Contains(out, "skipped: migrate failed") {
		t.Fatalf("expected failed job + skipped web: %v\n%s", err, out)
	}
	if _, body := v.get(host); body != "web-1" {
		t.Fatalf("old web should still serve: %q", body)
	}
	if plan, _ := v.e.Plan(ctx); !strings.Contains(actions(plan), p+"/migrate:update") {
		t.Fatalf("failed job should be retried: %s", actions(plan))
	}

	// fix the job and make backend a normal network: it is recreated, everything comes back
	v.commit(map[string]string{p + "/compose.yml": compose(`["lookup", "localhost"]`, "web-3", "false")})
	plan, _ = v.e.Plan(ctx)
	if !strings.Contains(strings.Join(plan.Warnings, " "), "will be recreated") {
		t.Fatalf("no recreate warning: %v", plan.Warnings)
	}
	if _, out, err = v.apply(ApplyOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if internal, _ := podman.Run(ctx, "network", "inspect", "--format", "{{.Internal}}", "vops-"+v.ns+".app-backend"); internal != "false" {
		t.Fatalf("backend still internal: %q", internal)
	}
	if code, body := v.get(host); code != 200 || body != "web-3" {
		t.Fatalf("after network change: %d %q", code, body)
	}
	if out, err := run(p, "web", "get", "http://db:9000/"); err != nil || out != "from-db" {
		t.Fatalf("web -> db after recreate: %q %v", out, err)
	}

	// dropping the custom network from compose removes it
	v.commit(map[string]string{p + "/compose.yml": "services:\n  web:\n    image: APP\n    x-vops: {port: 8080, domains: [" + host + "]}\n"})
	if _, out, err = v.apply(ApplyOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := podman.Run(ctx, "network", "exists", "vops-"+v.ns+".app-backend"); err == nil {
		t.Fatal("unused network not removed")
	}
}

// Data safety: volumes and project binds are subvolumes, every deploy snapshots them first, rollback
// restores them (and can itself be undone), automatic snapshots are pruned, manual ones kept.
func TestSnapshotsAndRollback(t *testing.T) {
	v := newEnv(t)
	if !snapshot.Supported(v.e.SnapshotDir) {
		t.Skip("not on btrfs")
	}
	ctx := context.Background()
	v.e.SnapshotKeep = 2
	p := v.ns + "/db"
	compose := func(version string) string {
		return `services:
  db:
    image: APP
    user: "999"
    environment: {V: "` + version + `"}
    volumes: ["data:/data:U", "./files:/files:U"]
volumes:
  data:
`
	}
	v.commit(map[string]string{p + "/compose.yml": compose("1")})
	if _, out, err := v.apply(ApplyOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if snaps, _ := v.e.DB.Snapshots(p); len(snaps) != 0 {
		t.Fatal("first deploy has no data to snapshot")
	}
	vol := podman.VolumePath(ctx, "vops-"+v.ns+".db-data")
	bind := filepath.Join(v.repo, p, "files")
	if !snapshot.IsSubvolume(vol) || !snapshot.IsSubvolume(bind) {
		t.Fatalf("data is not on subvolumes: %s %s", vol, bind)
	}
	exec := func(args ...string) string {
		t.Helper()
		cs, _ := podman.PS(ctx, LProject+"="+p)
		cs = slices.DeleteFunc(cs, func(c podman.Container) bool { return c.State != "running" })
		if len(cs) == 0 {
			t.Fatal("db not running")
		}
		out, err := podman.Run(ctx, append([]string{"exec", cs[0].ID, "/app"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	write := func(s string) { exec("write", "/data/f", s); exec("write", "/files/f", s) }
	read := func() string { return exec("read", "/data/f") + "|" + exec("read", "/files/f") }

	write("A") // written as uid 999: a subuid on the host, like postgres
	if _, out, err := v.apply(ApplyOpts{}); err != nil || strings.Contains(out, "snapshot") {
		t.Fatalf("no-op apply must not snapshot: %v\n%s", err, out)
	}
	v.commit(map[string]string{p + "/compose.yml": compose("2")})
	_, out, err := v.apply(ApplyOpts{})
	if err != nil || !strings.Contains(out, "(pre-deploy)") {
		t.Fatalf("deploy should snapshot first: %v\n%s", err, out)
	}
	snaps, _ := v.e.DB.Snapshots(p)
	if len(snaps) != 1 || snaps[0].Reason != "pre-deploy" || len(snaps[0].Volumes) != 2 {
		t.Fatalf("snapshots: %+v", snaps)
	}
	preDeploy := snaps[0]
	write("B") // the "migration" of v2

	// rollback (no id: right before the last deploy, v2): data is A again, the db is running, and there is an undo point
	var buf strings.Builder
	if err := v.e.Rollback(ctx, &buf, RollbackOpts{Project: p}); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if got := read(); got != "A|A" {
		t.Fatalf("after rollback: %q\n%s", got, buf.String())
	}
	latest, _ := v.e.DB.Snapshots(p)
	if latest[0].Reason != "pre-rollback" {
		t.Fatalf("no undo point: %+v", latest[0])
	}
	rb, _ := v.e.DB.Deploys(p, 1)
	if rb[0].RestoredID != preDeploy.ID || rb[0].Parts != "data" || rb[0].SnapshotID != latest[0].ID {
		t.Fatalf("rollback row: %+v", rb[0])
	}
	if again, err := v.e.RollbackPlan(ctx, RollbackOpts{Project: p}); err != nil || again.Before.ID != rb[0].BeforeID || again.Snapshot.ID != preDeploy.ID {
		t.Fatalf("a second rollback must target the same deploy, not the undo point: %v %+v", err, again)
	}
	// undo the rollback: right before it
	if err := v.e.Rollback(ctx, &buf, RollbackOpts{Project: p, Before: rb[0].ID}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "B|B" {
		t.Fatalf("after undo: %q", got)
	}
	// still a subvolume, still writable, still snapshottable
	write("C")
	if !snapshot.IsSubvolume(vol) || !snapshot.IsSubvolume(bind) {
		t.Fatal("restore lost the subvolumes")
	}

	// a manual snapshot survives pruning; automatic ones keep the newest 2
	manual, err := v.e.Snapshot(ctx, io.Discard, p, "before the big one")
	if err != nil {
		t.Fatal(err)
	}
	for i := 3; i <= 5; i++ {
		v.commit(map[string]string{p + "/compose.yml": compose(fmt.Sprint(i))})
		if _, out, err := v.apply(ApplyOpts{}); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	all, _ := v.e.DB.Snapshots(p)
	auto := 0
	for _, s := range all {
		if s.Reason != "manual" {
			auto++
		}
	}
	if auto != 2 || !slices.ContainsFunc(all, func(s store.Snapshot) bool { return s.ID == manual.ID }) {
		t.Fatalf("retention: %d automatic, manual kept=%v", auto, slices.ContainsFunc(all, func(s store.Snapshot) bool { return s.ID == manual.ID }))
	}
	if _, err := os.Stat(preDeploy.Volumes[0].Path); !os.IsNotExist(err) {
		t.Fatal("pruned snapshot still on disk")
	}
	// the manual snapshot still restores
	if err := v.e.Rollback(ctx, io.Discard, RollbackOpts{Project: p, Snapshot: manual.ID}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "C|C" {
		t.Fatalf("manual restore: %q", got)
	}
	if err := v.e.DeleteSnapshot(ctx, manual.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manual.Volumes[0].Path); !os.IsNotExist(err) {
		t.Fatal("deleted snapshot still on disk")
	}
	if st := v.e.DataState(ctx, p); !st.Supported || len(st.Protected) != 2 || len(st.Unprotected) != 0 {
		t.Fatalf("data state %+v", st)
	}
}
