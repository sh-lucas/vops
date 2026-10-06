package deploy

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/snapshot"
	"github.com/sh-lucas/vops/internal/store"
)

func running(cs []podman.Container) []string {
	var ids []string
	for _, c := range cs {
		if c.State == "running" {
			ids = append(ids, c.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// A deploy that fails leaves no replica a later RefreshRoutes could route, and a rolling release the proxy
// never confirms keeps the old replicas serving.
func TestFailedDeploysLeaveNothingBehind(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()

	// first deploy, never ready: nothing stays, nothing is routed
	p, host := v.ns+"/broken", "broken."+v.ns+".test"
	v.e.DB.SetEnv(p, "NEVER_READY", "1")
	v.commit(map[string]string{p + "/compose.yml": "services:\n  app:\n    image: APP\n    environment: [NEVER_READY]\n    x-vops: {port: 8080, health: /health, timeout: 2s, domains: [" + host + "]}\n"})
	if _, out, err := v.apply(ApplyOpts{}); err == nil || !strings.Contains(out, "not ready") {
		t.Fatalf("expected a failed deploy: %v\n%s", err, out)
	}
	if cs := v.containers(p); len(cs) != 0 {
		t.Fatalf("a replica that failed readiness stayed: %d containers", len(cs))
	}
	v.e.RefreshRoutes(ctx)
	if code, _ := v.get(host); code != 404 {
		t.Fatalf("failed replica routed: %d", code)
	}
	v.commit(map[string]string{p: ""})
	v.mustApply()

	// rolling release, the proxy doesn't confirm: old routes back, new replicas gone, the deploy fails
	p, host = v.ns+"/web", "web."+v.ns+".test"
	v.e.DB.SetEnv(p, "MSG", "v1")
	v.commit(map[string]string{p + "/compose.yml": "services:\n  app:\n    image: APP\n    environment: [MSG]\n    x-vops: {port: 8080, health: /health, domains: [" + host + "]}\n"})
	v.mustApply()
	old := running(v.containers(p))
	before := v.e.Routes.Routes()
	ConfirmWait = time.Second
	t.Cleanup(func() { ConfirmWait = 10 * time.Second; v.e.Routes.OnChange = nil })
	pushes := 0
	v.e.Routes.OnChange = func() error { pushes++; return errors.New("proxy.sock: connection refused") }
	v.e.DB.SetEnv(p, "MSG", "v2")
	_, out, err := v.apply(ApplyOpts{})
	if err == nil || !strings.Contains(out, "proxy did not confirm the new routes") || !strings.Contains(out, "old replicas kept serving") {
		t.Fatalf("expected the rollout to fail: %v\n%s", err, out)
	}
	if pushes < 2 {
		t.Fatalf("the push was not retried: %d", pushes)
	}
	v.e.Routes.OnChange = nil
	if now := running(v.containers(p)); !slices.Equal(now, old) || len(v.containers(p)) != 1 {
		t.Fatalf("containers after the unconfirmed rollout: %v, want %v", now, old)
	}
	if after := v.e.Routes.Routes(); proxy.Hash(after) != proxy.Hash(before) {
		t.Fatalf("routes: %+v, want %+v", after, before)
	}
	if code, body := v.get(host); code != 200 || body != "v1" {
		t.Fatalf("old version not serving: %d %q", code, body)
	}
	v.mustApply() // the proxy is back: the same release goes through
	if _, body := v.get(host); body != "v2" {
		t.Fatalf("after the proxy is back: %q", body)
	}
}

// Data operations that fail halfway never leave mixed data running: pre-deploy snapshots before network
// changes remove anything, a pause that fails aborts the snapshot, a restore that fails puts back what it did.
func TestDataFailuresAreConsistent(t *testing.T) {
	v := newEnv(t)
	if !snapshot.Supported(v.e.SnapshotDir) {
		t.Skip("not on btrfs")
	}
	ctx := context.Background()
	p := v.ns + "/db"
	compose := func(label string) string {
		return `services:
  db:
    image: APP
    user: "999"
    networks: [back]
    volumes: ["data:/data:U", "./files:/files:U"]
volumes:
  data:
networks:
  back: {labels: {v: "` + label + `"}}
`
	}
	v.commit(map[string]string{p + "/compose.yml": compose("1")})
	v.mustApply()
	exec := func(args ...string) string {
		t.Helper()
		ids := running(v.containers(p))
		if len(ids) == 0 {
			t.Fatal("db not running")
		}
		out, err := podman.Run(ctx, append([]string{"exec", ids[0], "/app"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	write := func(s string) { exec("write", "/data/f", s); exec("write", "/files/f", s) }
	read := func() string { return exec("read", "/data/f") + "|" + exec("read", "/files/f") }
	write("A")
	ids := running(v.containers(p))

	// a container that can't be paused: no snapshot, nothing left paused
	vols, _, users, err := v.e.projectData(ctx, p)
	if err != nil || len(vols) != 2 || len(users) != 1 {
		t.Fatalf("project data: %v %d volumes %d users", err, len(vols), len(users))
	}
	if _, err := v.e.snap(ctx, io.Discard, store.Snapshot{Project: p, Reason: "manual", Volumes: vols}, append(users, "vops-no-such-container")); err == nil || !strings.Contains(err.Error(), "pause") {
		t.Fatalf("snapshot with a pause failure: %v", err)
	}
	if cs := v.containers(p); cs[0].State != "running" {
		t.Fatalf("left %s", cs[0].State)
	}
	if snaps, _ := v.e.DB.Snapshots(p); len(snaps) != 0 {
		t.Fatalf("a snapshot was recorded: %+v", snaps)
	}

	// a network change whose pre-deploy snapshot fails: nothing was removed
	v.commit(map[string]string{p + "/compose.yml": compose("2")})
	takeSnapshot = func(context.Context, string, string) error { return errors.New("no space left on device") }
	_, out, err := v.apply(ApplyOpts{})
	takeSnapshot = snapshot.Take
	if err == nil || !strings.Contains(out, "pre-deploy snapshot failed") {
		t.Fatalf("expected the snapshot to fail: %v\n%s", err, out)
	}
	if now := running(v.containers(p)); !slices.Equal(now, ids) {
		t.Fatalf("containers after a failed snapshot: %v, want %v", now, ids)
	}
	if got := read(); got != "A|A" {
		t.Fatalf("data: %q", got)
	}
	v.mustApply()
	snaps, _ := v.e.DB.Snapshots(p)
	if len(snaps) != 1 || snaps[0].Reason != "pre-deploy" {
		t.Fatalf("snapshots: %+v", snaps)
	}
	target := snaps[0]
	write("B")

	// the second volume fails to restore: the first is put back, the project runs on its data from before
	calls := 0
	restoreVolume = func(ctx context.Context, snap, target string) error {
		if calls++; calls == 2 {
			return errors.New("injected")
		}
		return snapshot.Restore(ctx, snap, target)
	}
	t.Cleanup(func() { restoreVolume = snapshot.Restore })
	var buf strings.Builder
	if err := v.e.Rollback(ctx, &buf, RollbackOpts{Project: p, Snapshot: target.ID}); err == nil || !strings.Contains(err.Error(), "as it was before this rollback") {
		t.Fatalf("rollback with a failed volume: %v\n%s", err, buf.String())
	}
	if got := read(); got != "B|B" {
		t.Fatalf("data after a failed restore: %q\n%s", got, buf.String())
	}

	// putting it back fails too: the project stays stopped and the error says how to recover
	calls = 0
	restoreVolume = func(ctx context.Context, snap, target string) error {
		if calls++; calls >= 2 {
			return errors.New("injected")
		}
		return snapshot.Restore(ctx, snap, target)
	}
	buf.Reset()
	err = v.e.Rollback(ctx, &buf, RollbackOpts{Project: p, Snapshot: target.ID})
	restoreVolume = snapshot.Restore
	if err == nil || !strings.Contains(err.Error(), "is stopped") || !strings.Contains(err.Error(), "--snapshot") {
		t.Fatalf("rollback with a failed undo: %v\n%s", err, buf.String())
	}
	if ids := running(v.containers(p)); len(ids) != 0 {
		t.Fatal("a project with mixed data was started")
	}
	latest, _ := v.e.DB.Snapshots(p)
	if latest[0].Reason != "pre-rollback" {
		t.Fatalf("no undo point: %+v", latest[0])
	}
	if err := v.e.Rollback(ctx, io.Discard, RollbackOpts{Project: p, Snapshot: latest[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Restart(ctx, p, ""); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "B|B" {
		t.Fatalf("after recovering: %q", got)
	}
}
