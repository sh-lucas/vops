package deploy

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/store"
)

// ownRegistry gives the engine a vops registry and returns its address and a push of repo:tag answering msg.
func ownRegistry(t *testing.T, v *env) (*registry.Registry, string, func(repo, tag, msg string, env ...string) registry.Push) {
	reg, err := registry.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	all := func(string) bool { return true }
	reg.Auth = func(_ *http.Request, user, pass string) (*registry.Perm, error) {
		return &registry.Perm{Name: user, Pull: all, Push: all}, nil
	}
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	v.e.Registry, v.e.PullAddr = reg, addr
	v.e.PullAuthFile = filepath.Join(t.TempDir(), "auth.json")
	os.WriteFile(v.e.PullAuthFile, []byte(`{"auths":{"`+addr+`":{"auth":"`+base64.StdEncoding.EncodeToString([]byte("x:y"))+`"}}}`), 0o600)
	ctx := context.Background()
	push := func(repo, tag, msg string, env ...string) registry.Push {
		t.Helper()
		dir := t.TempDir()
		file := "FROM " + v.app + "\nENV MSG=" + msg + "\n"
		for _, e := range env {
			file += "ENV " + e + "\n"
		}
		os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(file), 0o644)
		ref := addr + "/" + repo + ":" + tag
		if _, err := podman.Run(ctx, "build", "-q", "-t", ref, dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { podman.Run(context.Background(), "rmi", "-f", ref) })
		if _, err := podman.Run(ctx, "push", "-q", "--tls-verify=false", "--creds", "ci:y", ref); err != nil {
			t.Fatal(err)
		}
		return registry.Push{Repo: repo, Tag: tag, Digest: reg.Resolve(repo, tag), User: "ci"}
	}
	return reg, addr, push
}

// x-vops.release: manual. A push waits: the plan shows nothing pending, an apply (even one racing the push, before
// the release exists) deploys nothing; Approve deploys exactly that digest to web, its worker and the migration
// job sharing the image; a newer push supersedes; Reject keeps what runs; a scheduled approval runs when due; an
// approval refuses a tag that moved; gc keeps the pending digest.
func TestManualRelease(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	reg, addr, push := ownRegistry(t, v)
	p := v.ns + "/shop"
	repo := v.ns + "/web"
	host := "web.shop." + v.ns + "." + v.ns + ".test"
	image := addr + "/" + repo + ":latest"
	v.commit(map[string]string{"vops.yml": "domain: " + v.ns + ".test\n", p + "/compose.yml": `x-vops: {release: manual}
services:
  web:
    image: ` + image + `
    depends_on: {migrate: {condition: service_completed_successfully}}
    x-vops: {port: 8080}
  worker:
    image: ` + image + `
  migrate:
    image: ` + image + `
    command: [read, /etc/hosts]
`})
	body := func() string { _, b := v.get(host); return b }
	var events []ReleaseEvent
	v.e.OnRelease = func(ev ReleaseEvent) { events = append(events, ev) }
	pushed := func(pu registry.Push) {
		t.Helper()
		var out strings.Builder
		if err := v.e.Pushed(ctx, &out, pu); err != nil {
			t.Fatalf("pushed: %v\n%s", err, out.String())
		}
	}
	quiet := func(what string) *ProjectPlan {
		t.Helper()
		plan, err := v.e.Plan(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		plan.Print(&text)
		if plan.Changes() {
			t.Fatalf("%s: a held release is no pending change:\n%s", what, text.String())
		}
		for _, pp := range plan.Projects {
			if pp.Path == p {
				return pp
			}
		}
		t.Fatalf("no %s in the plan", p)
		return nil
	}
	digestOf := func(svc string) string {
		cs, _ := podman.PS(ctx, LProject+"="+p, LService+"="+svc)
		if len(cs) != 1 {
			t.Fatalf("%s has %d containers", svc, len(cs))
		}
		out, _ := podman.Run(ctx, "image", "inspect", "--format", "{{.RepoDigests}}", cs[0].ImageID)
		return out // every digest it was pulled by
	}
	latest := func() store.Release {
		rs, _ := v.e.DB.Releases(p, 1)
		if len(rs) == 0 {
			t.Fatal("no release")
		}
		return rs[0]
	}

	v1 := push(repo, "latest", "one")
	v.mustApply() // the first deploy is an apply's: nothing ran, nothing to hold
	if body() != "one" {
		t.Fatalf("setup: %q", body())
	}

	// a push waits; the plan shows it as holds, not actions; apply deploys nothing
	v2 := push(repo, "latest", "two")
	pp := quiet("before the release exists") // the push trigger hasn't run yet: held anyway
	if len(pp.Holds) != 3 || pp.Holds[0].Release != nil || pp.Holds[0].Running != v1.Digest {
		t.Fatalf("holds before the release: %+v", pp.Holds)
	}
	pushed(v2)
	r1 := latest()
	if r1.Status != "pending" || r1.Digest != v2.Digest || r1.PushedBy != "ci" || r1.Mode != "manual" || len(events) != 1 || events[0].What != "held" {
		t.Fatalf("release: %+v %+v", r1, events)
	}
	pp = quiet("pending")
	var text strings.Builder
	(&Plan{Projects: []*ProjectPlan{pp}}).Print(&text)
	if !strings.Contains(text.String(), "web: waiting approval of #") || pp.Holds[0].Release == nil {
		t.Fatalf("plan text:\n%s", text.String())
	}
	if _, out, err := v.apply(ApplyOpts{}); err != nil || body() != "one" || !strings.Contains(digestOf("worker"), v1.Digest) {
		t.Fatalf("apply deployed a held release: %v %q %s %s\n%s", err, body(), digestOf("worker"), v1.Digest, out)
	}
	if keep, _ := v.e.KeepDigests(10); !keep[v2.Digest] {
		t.Fatal("gc would drop the pending digest")
	}

	// approve: exactly that digest, everywhere it runs, with a history row
	var out strings.Builder
	if err := v.e.Approve(ctx, &out, r1.ID, "admin"); err != nil {
		t.Fatalf("approve: %v\n%s", err, out.String())
	}
	d, _ := v.e.DB.Deploys(p, 1)
	if body() != "two" || !strings.Contains(digestOf("worker"), v2.Digest) || !strings.Contains(digestOf("migrate"), v2.Digest) || latest().Status != "approved" || d[0].Trigger != "approve" || d[0].Images["web"].Digest != v2.Digest {
		t.Fatalf("after approve: %q %+v %+v\n%s", body(), latest(), d[0], out.String())
	}
	if pp := quiet("approved"); len(pp.Holds) != 0 {
		t.Fatalf("holds after approve: %+v", pp.Holds)
	}
	if err := v.e.Approve(ctx, &out, r1.ID, "admin"); err == nil {
		t.Fatal("approved twice")
	}

	// a newer push supersedes; reject keeps what runs, also through an apply
	pushed(push(repo, "latest", "three"))
	r2 := latest()
	v4 := push(repo, "latest", "four")
	pushed(v4)
	r3 := latest()
	if old, _, _ := v.e.DB.GetRelease(r2.ID); old.Status != "superseded" || r3.Digest != v4.Digest || r3.Status != "pending" {
		t.Fatalf("supersede: %+v %+v", old, r3)
	}
	if err := v.e.Reject(ctx, &out, r3.ID, "admin", "not today"); err != nil {
		t.Fatal(err)
	}
	if pp := quiet("rejected"); len(pp.Holds) != 3 || pp.Holds[0].Release == nil || pp.Holds[0].Release.Status != "rejected" {
		t.Fatalf("holds after reject: %+v", pp.Holds)
	}
	if _, out, err := v.apply(ApplyOpts{}); err != nil || body() != "two" {
		t.Fatalf("apply deployed a rejected release: %v %q\n%s", err, body(), out)
	}

	// scheduled
	v5 := push(repo, "latest", "five")
	pushed(v5)
	r4 := latest()
	// disable and enable while it waits: back on what ran, not the pending digest
	v.e.DB.SetDisabled(p, true)
	if _, out, err := v.apply(ApplyOpts{}); err != nil || len(v.containers(p)) != 0 {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	v.e.DB.SetDisabled(p, false)
	if _, out, err := v.apply(ApplyOpts{}); err != nil || body() != "two" || latest().Status != "pending" {
		t.Fatalf("enable with a pending release: %v %q %+v\n%s", err, body(), latest(), out)
	}
	if err := v.e.Schedule(r4.ID, time.Now().Add(time.Hour).Unix(), "admin"); err != nil {
		t.Fatal(err)
	}
	v.e.DueReleases(ctx, &out, time.Now())
	if body() != "two" || latest().Status != "pending" {
		t.Fatalf("approved before its time: %q %+v", body(), latest())
	}
	v.e.DueReleases(ctx, &out, time.Now().Add(2*time.Hour))
	if body() != "five" || latest().Status != "approved" || latest().DecidedBy != "admin" {
		t.Fatalf("scheduled approval: %q %+v\n%s", body(), latest(), out.String())
	}

	// a tag that moved without a release (the push trigger didn't run yet) can't be approved from an older one
	pushed(push(repo, "latest", "six"))
	r5 := latest()
	push(repo, "latest", "seven")
	if err := v.e.Approve(ctx, &out, r5.ID, "admin"); err == nil || !strings.Contains(err.Error(), "points at") || body() != "five" {
		t.Fatalf("approve of a moved tag: %v %q", err, body())
	}
	if _, err := reg.GC(0, must(v.e.KeepDigests(10))); err != nil || reg.Resolve(repo, r5.Digest) == "" {
		t.Fatalf("gc dropped a pending digest: %v", err)
	}

	// running services whose deploy rows were pruned (host-wide retention): their digest is unknown, they wait
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(v.repo), "vops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM deploys WHERE project = ?", p); err != nil {
		t.Fatal(err)
	}
	if pp := quiet("no rows"); len(pp.Holds) != 3 || pp.Holds[0].Running != "" {
		t.Fatalf("holds without rows: %+v", pp.Holds)
	}
	if _, out, err := v.apply(ApplyOpts{}); err != nil || body() != "five" {
		t.Fatalf("apply without rows deployed a push nobody approved: %v %q\n%s", err, body(), out)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// x-vops.release: canary 50%. A push runs the migration job at once and gives web (2 replicas) one canary
// container with the pushed digest, no alias, routed to its share of clients (sticky by cookie) while the worker
// waits; the split survives a rebuild of the routes from podman (daemon restart); a compose change of web rebuilds
// the canary; Approve rolls web to that digest under load with 0 failed requests and removes the canary; a
// canary that never gets ready, or dies, rejects its release; a newer push replaces the canary.
func TestCanaryRelease(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	_, addr, push := ownRegistry(t, v)
	p := v.ns + "/shop"
	repo := v.ns + "/web"
	host := "web.shop." + v.ns + "." + v.ns + ".test"
	image := addr + "/" + repo + ":latest"
	compose := func(env string) map[string]string {
		return map[string]string{"vops.yml": "domain: " + v.ns + ".test\n", p + "/compose.yml": `x-vops: {release: canary 50%}
services:
  web:
    image: ` + image + `
    environment: {A: "` + env + `"}
    depends_on: {migrate: {condition: service_completed_successfully}}
    x-vops: {port: 8080, health: /health, replicas: 2, timeout: 4s}
  worker:
    image: ` + image + `
  migrate:
    image: ` + image + `
    command: [read, /etc/hosts]
`}
	}
	v.commit(compose("1"))
	var failures []error
	v.e.OnDeploy = func(_ string, err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	pushed := func(pu registry.Push) {
		t.Helper()
		var out strings.Builder
		if err := v.e.Pushed(ctx, &out, pu); err != nil {
			t.Fatalf("pushed: %v\n%s", err, out.String())
		}
	}
	latest := func() store.Release {
		rs, _ := v.e.DB.Releases(p, 1)
		return rs[0]
	}
	canaries := func() []podman.Container {
		cs, _ := podman.PS(ctx, LProject+"="+p, LRole+"=canary")
		return cs
	}
	as := func(cookie string) string {
		req, _ := http.NewRequest("GET", v.proxy.URL, nil)
		req.Host = host
		if cookie != "" {
			req.Header.Set("Cookie", proxy.CanaryCookie+"="+cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	split := func(id int64) {
		t.Helper()
		on, off := fmt.Sprintf("%d.on", id), fmt.Sprintf("%d.off", id)
		seen := map[string]int{}
		for range 40 {
			seen[as("")]++
		}
		if as(on) != "two" || as(off) != "one" || seen["one"] == 0 || seen["two"] == 0 || seen["one"]+seen["two"] != 40 {
			t.Fatalf("split: on %q off %q, without a cookie %v", as(on), as(off), seen)
		}
	}

	push(repo, "latest", "one")
	v.mustApply()
	v2 := push(repo, "latest", "two")
	pushed(v2)
	r := latest()
	cs := canaries()
	if r.Mode != "canary" || r.Weight != 50 || r.Status != "pending" || len(cs) != 1 || cs[0].Labels[LService] != "web" || cs[0].Labels[LRelease] != fmt.Sprint(r.ID) {
		t.Fatalf("canary: %+v %d containers", r, len(cs))
	}
	plan, _ := v.e.Plan(ctx)
	var pp *ProjectPlan
	for _, x := range plan.Projects {
		if x.Path == p {
			pp = x
		}
	}
	var text strings.Builder
	plan.Print(&text)
	if plan.Changes() || len(pp.Canaries) != 1 || len(pp.actual["web"]) != 2 || !strings.Contains(text.String(), "web canary of #") || !strings.Contains(text.String(), "worker: waiting approval") || strings.Contains(text.String(), "migrate: waiting") {
		t.Fatalf("plan with a canary:\n%s", text.String())
	}
	if nets, _ := podman.Run(ctx, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.Aliases}}{{end}}", cs[0].ID); strings.Contains(nets, "web") {
		t.Fatalf("the canary has the service's alias: %s", nets)
	}
	split(r.ID)

	// the routes come back from podman's labels (a daemon restart)
	v.e.Routes.Replace(nil, nil)
	v.e.RefreshRoutes(ctx)
	split(r.ID)

	// a compose change of web rebuilds its canary with the new definition and the same digest
	v.commit(compose("2"))
	v.e.CheckCanaries(ctx, io.Discard)
	if csNow := canaries(); len(csNow) != 1 || csNow[0].ID != cs[0].ID {
		t.Fatal("housekeeping applied a compose change before apply")
	}
	if _, out, err := v.apply(ApplyOpts{}); err != nil || len(canaries()) != 1 || canaries()[0].ID == cs[0].ID {
		t.Fatalf("compose change with a canary: %v %d\n%s", err, len(canaries()), out)
	}
	split(r.ID)

	// approve under load: rolling, then the canary goes
	var fails, total atomic.Int64
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
				if code, b := v.get(host); code != 200 {
					fails.Add(1)
					t.Logf("failed request: %d %s", code, b)
				}
				total.Add(1)
			}
		})
	}
	var out strings.Builder
	err := v.e.Approve(ctx, &out, r.ID, "admin")
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if err != nil || fails.Load() > 0 || len(canaries()) != 0 || as("") != "two" || latest().Status != "approved" {
		t.Fatalf("approve: %v, %d of %d failed, %d canaries, %q\n%s", err, fails.Load(), total.Load(), len(canaries()), as(""), out.String())
	}
	if routes := v.e.Routes.Routes(); len(routes) != 1 || routes[0].Canary != nil {
		t.Fatalf("routes after approve: %+v", routes)
	}

	// a canary that never gets ready is rejected, and the release with it
	pushed(push(repo, "latest", "three", "NEVER_READY=1"))
	if r := latest(); r.Status != "rejected" || r.DecidedBy != "vops" || len(canaries()) != 0 || len(failures) == 0 || as("") != "two" {
		t.Fatalf("failed canary: %+v %d %v %q", r, len(canaries()), failures, as(""))
	}

	// a newer push replaces the canary; its death rejects it
	pushed(push(repo, "latest", "four"))
	first := canaries()
	// Lost confirmations preserve every potentially routed backend, even if the proxy applied the table.
	v.e.Routes.OnChange = func() error { return errors.New("proxy confirmation lost") }
	pushed(push(repo, "latest", "five"))
	second := canaries()
	if len(first) != 1 || len(second) != 2 || !slices.ContainsFunc(second, func(c podman.Container) bool { return c.ID == first[0].ID }) {
		t.Fatalf("failed confirmation removed an old canary: %d %d", len(first), len(second))
	}
	v.e.CheckCanaries(ctx, &out)
	if cs := canaries(); len(cs) != 2 || !slices.ContainsFunc(cs, func(c podman.Container) bool { return c.ID == second[0].ID }) || !slices.ContainsFunc(cs, func(c podman.Container) bool { return c.ID == second[1].ID }) {
		t.Fatalf("failed retry replaced existing canaries: %+v", cs)
	}
	v.e.Routes.OnChange = nil
	v.e.CheckCanaries(ctx, &out)
	second = canaries()
	if len(second) != 1 || first[0].ID == second[0].ID || second[0].Labels[LRelease] != fmt.Sprint(latest().ID) {
		t.Fatalf("newer push: %d %d", len(first), len(second))
	}
	if as(fmt.Sprintf("%d.on", latest().ID)) != "five" || as(fmt.Sprintf("%d.off", latest().ID)) != "two" {
		t.Fatal("retry did not restore the newest canary split")
	}
	v.e.Routes.OnChange = func() error { return errors.New("proxy confirmation lost") }
	v.e.CanaryDied(ctx, p, "web", second[0].Labels[LRelease], "exited with code 2")
	if len(canaries()) != 1 {
		t.Fatal("rejection removed a potentially routed canary without confirmation")
	}
	v.e.RefreshRoutes(ctx)
	if routes := v.e.Routes.Routes(); routes[0].Canary != nil {
		t.Fatal("route refresh restored a rejected canary")
	}
	v.e.Routes.OnChange = nil
	v.e.CheckCanaries(ctx, &out)
	if r := latest(); r.Status != "rejected" || len(canaries()) != 0 || as("") != "two" {
		t.Fatalf("dead canary: %+v %d", r, len(canaries()))
	}
}
