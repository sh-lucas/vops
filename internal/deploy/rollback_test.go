package deploy

import (
	"context"
	"encoding/base64"
	"io"
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
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/snapshot"
	"github.com/sh-lucas/vops/internal/store"
)

// Image rollback against the vops registry: push v1, push v2, roll back images only (under load, 0 failed
// requests) and web runs v1's digest, pinned, with nothing pending; registry gc keeps the pinned digest; a push
// of v3 ends the pin; rolling back again pins what ran before and undoing it unpins; a compose image change drops a pin;
// images and data together; a built image is skipped with the reason; gc keeps the image_keep newest deploys.
func TestImageRollback(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	reg, err := registry.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	all := func(string) bool { return true }
	reg.Auth = func(user, pass string) *registry.Perm { return &registry.Perm{Name: user, Pull: all, Push: all} }
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	v.e.Registry, v.e.PullAddr = reg, addr
	v.e.PullAuthFile = filepath.Join(t.TempDir(), "auth.json")
	os.WriteFile(v.e.PullAuthFile, []byte(`{"auths":{"`+addr+`":{"auth":"`+base64.StdEncoding.EncodeToString([]byte("x:y"))+`"}}}`), 0o600)

	p := v.ns + "/shop"
	repo := v.ns + "/web"
	host := "web.shop." + v.ns + "." + v.ns + ".test"
	push := func(tag, msg string) string {
		t.Helper()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+v.app+"\nENV MSG="+msg+"\n"), 0o644)
		ref := addr + "/" + repo + ":" + tag
		if _, err := podman.Run(ctx, "build", "-q", "-t", ref, dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { podman.Run(context.Background(), "rmi", "-f", ref) })
		if _, err := podman.Run(ctx, "push", "-q", "--tls-verify=false", "--creds", "x:y", ref); err != nil {
			t.Fatal(err)
		}
		return reg.Resolve(repo, tag)
	}
	// what the daemon does on a push: redeploy the services running that tag, ending their pins
	pushed := func(tag string) {
		t.Helper()
		plan, err := v.e.Plan(ctx)
		if err != nil {
			t.Fatal(err)
		}
		keys := plan.Watching(repo, tag)
		if len(keys) == 0 {
			t.Fatalf("nothing watches %s:%s", repo, tag)
		}
		if _, out, err := v.apply(ApplyOpts{Services: keys, Trigger: "push", Unpin: repo + ":" + tag + " pushed"}); err != nil {
			t.Fatalf("push deploy: %v\n%s", err, out)
		}
	}
	compose := func(tag, api string) map[string]string {
		return map[string]string{"vops.yml": "domain: " + v.ns + ".test\n", p + "/api/Containerfile": "FROM APP\nENV MSG=" + api + "\n", p + "/compose.yml": `services:
  web:
    image: ` + addr + `/` + repo + `:` + tag + `
    x-vops: {port: 8080}
  api:
    build: ./api
  db:
    image: APP
    user: "999"
    volumes: ["data:/data:U"]
volumes:
  data:
`}
	}
	history := func() []store.Deploy { ds, _ := v.e.DB.Deploys(p, 100); return ds }
	body := func() string { _, b := v.get(host); return b }
	webDigest := func() string {
		cs, _ := podman.PS(ctx, LProject+"="+p, LService+"=web")
		if len(cs) != 1 {
			t.Fatalf("web has %d containers", len(cs))
		}
		return imageDigest(ctx, cs[0].Labels[LImage])
	}
	pinOf := func(svc string) *store.Pin {
		pins, _ := v.e.DB.Pins(p)
		for _, pin := range pins {
			if pin.Service == svc {
				return &pin
			}
		}
		return nil
	}
	rollback := func(o RollbackOpts) string {
		t.Helper()
		o.Project = p
		var buf strings.Builder
		if err := v.e.Rollback(ctx, &buf, o); err != nil {
			t.Fatalf("rollback %+v: %v\n%s", o, err, buf.String())
		}
		return buf.String()
	}

	v1 := push("latest", "v1")
	v.commit(compose("latest", "a1"))
	v.mustApply()
	d1 := history()[0]
	v2 := push("latest", "v2")
	pushed("latest")
	d2 := history()[0]
	if body() != "v2" || d2.Trigger != "push" || d2.Images["web"].Digest != v2 || d1.Images["web"].Digest != v1 {
		t.Fatalf("setup: %q %+v %+v", body(), d1, d2)
	}

	// images only, under load: web back to v1's digest with a rolling release
	rp, err := v.e.RollbackPlan(ctx, RollbackOpts{Project: p, Images: true})
	if err != nil || rp.Before.ID != d2.ID || rp.From != d1.ID || strings.Join(rp.Parts, ",") != "images" {
		t.Fatalf("plan: %v %+v", err, rp)
	}
	var steps []string
	for _, st := range rp.Images {
		steps = append(steps, st.Service+":"+st.Do)
	}
	if strings.Join(steps, " ") != "api:same db:same web:pin" {
		t.Fatalf("steps: %v", steps)
	}
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
	time.Sleep(200 * time.Millisecond)
	out := rollback(RollbackOpts{Images: true})
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if fails.Load() > 0 {
		t.Fatalf("%d of %d requests failed during the image rollback\n%s", fails.Load(), total.Load(), out)
	}
	r1 := history()[0]
	if body() != "v1" || webDigest() != v1 || r1.Trigger != "rollback" || r1.BeforeID != d2.ID || r1.Parts != "images" || r1.Images["web"].Digest != v1 || r1.RestoredID != 0 {
		t.Fatalf("after image rollback: %q %s %+v\n%s", body(), webDigest(), r1, out)
	}
	if pin := pinOf("web"); pin == nil || pin.Digest != v1 || pin.DeployID != d1.ID || pin.ComposeImage != addr+"/"+repo+":latest" {
		t.Fatalf("pin: %+v", pin)
	}
	plan, _ := v.e.Plan(ctx)
	var text strings.Builder
	plan.Print(&text)
	if plan.Changes() || !strings.Contains(text.String(), "@ web pinned to #") || !strings.Contains(text.String(), "compose says "+addr+"/"+repo+":latest") {
		t.Fatalf("a pin is not a pending change, and shows:\n%s", text.String())
	}
	for _, pp := range plan.Projects {
		for _, s := range pp.Services() {
			if pp.Path == p && (s.Name == "web") != (s.Pin != nil) {
				t.Fatalf("service state pin: %+v", s)
			}
		}
	}

	// gc keeps the pinned digest even though latest moved on
	keep, err := v.e.KeepDigests(10)
	if err != nil || !keep[v1] || !keep[v2] {
		t.Fatalf("keep set: %v %v", err, keep)
	}
	if _, err := reg.GC(0, keep); err != nil || reg.Resolve(repo, v1) == "" {
		t.Fatalf("gc dropped the pinned digest: %v", err)
	}

	// a push of the tag web runs is a newer version: it ends the pin
	v3 := push("latest", "v3")
	pushed("latest")
	d3 := history()[0]
	if body() != "v3" || pinOf("web") != nil || d3.Trigger != "push" {
		t.Fatalf("after pushing v3: %q %+v", body(), pinOf("web"))
	}

	// roll back again (no id: right before the v3 push, when the first rollback had v1 running): v1 pinned; undoing it unpins
	rollback(RollbackOpts{})
	r2 := history()[0]
	if body() != "v1" || pinOf("web") == nil || pinOf("web").Digest != v1 || r2.BeforeID != d3.ID || r2.Undoes != 0 {
		t.Fatalf("second rollback: %q %+v", body(), r2)
	}
	rollback(RollbackOpts{Before: r2.ID})
	if u := history()[0]; body() != "v3" || pinOf("web") != nil || u.Undoes != r2.ID || u.Parts != r2.Parts || r2.Parts != "images,data" {
		t.Fatalf("undo: %q %+v %+v", body(), pinOf("web"), u)
	}

	// a compose image change drops the pin (env and other changes wouldn't)
	rollback(RollbackOpts{Before: d3.ID, Services: []string{"web"}})
	if body() != "v1" {
		t.Fatalf("pinned again: %q", body())
	}
	push("stable", "stable")
	v.commit(compose("stable", "a1"))
	plan, _ = v.e.Plan(ctx)
	text.Reset()
	plan.Print(&text)
	if !plan.Changes() || !strings.Contains(text.String(), "pin to #") || !strings.Contains(text.String(), "compose says "+addr+"/"+repo+":stable now") {
		t.Fatalf("stale pin not planned:\n%s", text.String())
	}
	v.mustApply()
	if body() != "stable" || pinOf("web") != nil {
		t.Fatalf("after the compose change: %q %+v", body(), pinOf("web"))
	}
	if evs, _ := v.e.DB.Events(p, 50); !slices.ContainsFunc(evs, func(e store.Event) bool { return strings.Contains(e.Message, "removed: compose says") }) {
		t.Fatalf("no event for the dropped pin: %+v", evs)
	}

	// a built image can't roll back without git: skipped with the reason, and asking for it fails
	v.commit(compose("stable", "a2"))
	v.mustApply()
	rp, err = v.e.RollbackPlan(ctx, RollbackOpts{Project: p})
	if err != nil || rp.Images[0].Service != "api" || rp.Images[0].Do != "skip" || !strings.Contains(rp.Images[0].Reason, "built from the code") {
		t.Fatalf("built service: %v %+v", err, rp.Images)
	}
	if _, err := v.e.RollbackPlan(ctx, RollbackOpts{Project: p, Services: []string{"api"}}); err == nil || !strings.Contains(err.Error(), "built from the code") {
		t.Fatalf("asking for a built service: %v", err)
	}

	// images and data together: stop, restore, start on the pinned image
	if snapshot.Supported(v.e.SnapshotDir) {
		dbExec := func(args ...string) string {
			t.Helper()
			cs, _ := podman.PS(ctx, LProject+"="+p, LService+"=db")
			out, err := podman.Run(ctx, append([]string{"exec", cs[0].ID, "/app"}, args...)...)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		dbExec("write", "/data/f", "X")
		push("stable", "v4")
		pushed("stable")
		d4 := history()[0]
		dbExec("write", "/data/f", "Y")
		out := rollback(RollbackOpts{})
		r := history()[0]
		if body() != "stable" || dbExec("read", "/data/f") != "X" || r.Parts != "images,data" || r.BeforeID != d4.ID || r.RestoredID != d4.SnapshotID || r.SnapshotID == 0 {
			t.Fatalf("images and data: %q %+v\n%s", body(), r, out)
		}
		if s, _ := v.e.DB.Snapshot(r.SnapshotID); s.Reason != "pre-rollback" {
			t.Fatalf("undo point: %+v", s)
		}
		rollback(RollbackOpts{Before: r.ID})
		if body() != "v4" || dbExec("read", "/data/f") != "Y" || pinOf("web") != nil {
			t.Fatalf("undo of images and data: %q", body())
		}
	}

	// gc keeps the images of the newest image_keep deploys only; older ones show as gone and can't roll back
	keep, _ = v.e.KeepDigests(1)
	if _, err := reg.GC(0, keep); err != nil {
		t.Fatal(err)
	}
	if reg.Resolve(repo, v1) != "" || reg.Resolve(repo, v2) != "" || reg.Resolve(repo, v3) == "" {
		t.Fatal("gc kept digests of old deploys, or dropped a tagged one")
	}
	tl, _ := v.e.Timeline(ctx, p)
	if n := tl.Nodes[len(tl.Nodes)-1]; n.Deploy.ID != d1.ID || !slices.Contains(n.Gone, "web") {
		t.Fatalf("old deploy's image not marked gone: %+v", n)
	}
	rp, _ = v.e.RollbackPlan(ctx, RollbackOpts{Project: p, Before: d2.ID})
	if i := slices.IndexFunc(rp.Images, func(st ImageStep) bool { return st.Service == "web" }); rp.Images[i].Do != "skip" || !strings.Contains(rp.Images[i].Reason, "gone") {
		t.Fatalf("gone image: %+v", rp.Images)
	}
	if err := v.e.Rollback(ctx, io.Discard, RollbackOpts{Project: p, Before: d2.ID, Images: true}); err == nil {
		t.Fatal("rolled back to a gone image")
	}
}
