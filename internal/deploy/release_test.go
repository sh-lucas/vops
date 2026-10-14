package deploy

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/store"
)

// ownRegistry gives the engine a vops registry and returns its address and a push of repo:tag answering msg.
func ownRegistry(t *testing.T, v *env) (*registry.Registry, string, func(repo, tag, msg string) registry.Push) {
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
	push := func(repo, tag, msg string) registry.Push {
		t.Helper()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+v.app+"\nENV MSG="+msg+"\n"), 0o644)
		ref := addr + "/" + repo + ":" + tag
		previous := reg.Resolve(repo, tag)
		if _, err := podman.Run(ctx, "build", "-q", "-t", ref, dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { podman.Run(context.Background(), "rmi", "-f", ref) })
		if _, err := podman.Run(ctx, "push", "-q", "--tls-verify=false", "--creds", "ci:y", ref); err != nil {
			t.Fatal(err)
		}
		return registry.Push{Repo: repo, Tag: tag, Digest: reg.Resolve(repo, tag), Previous: previous, User: "ci"}
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
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
