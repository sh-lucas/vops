package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/snapshot"
	"github.com/sh-lucas/vops/internal/store"
)

// Deploy history: every apply that changes a project is a row with its commit, the images that run and the
// snapshot of the data right before it; a failed deploy records what still runs; a rollback is a row, and
// restoring its undo point marks the undo; a preview made from a history node runs that node's commit and
// images on that node's data, and shows up on the node.
func TestHistoryRollbackAndBranch(t *testing.T) {
	v := newEnv(t)
	if !snapshot.Supported(v.e.SnapshotDir) {
		t.Skip("not on btrfs")
	}
	ctx := context.Background()
	p := v.ns + "/app"
	domain := v.ns + ".test"
	host := "web.app." + v.ns + "." + domain
	image := func(tag string) string {
		t.Helper()
		name := "localhost/" + v.ns + "-web:" + tag
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+v.app+"\nENV MSG=img-"+tag+"\n"), 0o644)
		if _, err := podman.Run(ctx, "build", "-q", "-t", name, dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { podman.Run(context.Background(), "rmi", "-f", name) })
		return name
	}
	compose := func(web string) map[string]string {
		return map[string]string{"vops.yml": "domain: " + domain + "\n", p + "/compose.yml": `services:
  db:
    image: APP
    user: "999"
    volumes: ["data:/data:U"]
  web:
    image: ` + web + `
    x-vops: {port: 8080, preview: {copy: [db]}}
volumes:
  data:
`}
	}
	exec := func(project string, args ...string) string {
		t.Helper()
		cs, _ := podman.PS(ctx, LProject+"="+project, LService+"=db")
		cs = slices.DeleteFunc(cs, func(c podman.Container) bool { return c.State != "running" })
		if len(cs) == 0 {
			t.Fatalf("no db running in %s", project)
		}
		out, err := podman.Run(ctx, append([]string{"exec", cs[0].ID, "/app"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	head := func() string { return strings.TrimSpace(run(t, v.repo, "git", "rev-parse", "HEAD")) }
	history := func() []store.Deploy {
		t.Helper()
		ds, err := v.e.DB.Deploys(p, 100)
		if err != nil {
			t.Fatal(err)
		}
		return ds
	}
	apply := func(trigger string) {
		t.Helper()
		if _, out, err := v.apply(ApplyOpts{Trigger: trigger}); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
	}
	v1, v2, v3 := image("v1"), image("v2"), image("v3")

	// first deploy: nothing to snapshot yet
	v.commit(compose(v1))
	c1 := head()
	apply("sync")
	ds := history()
	if len(ds) != 1 || ds[0].Trigger != "sync" || ds[0].Commit != c1 || ds[0].Result != "ok" || ds[0].SnapshotID != 0 ||
		ds[0].Images["web"].Image != v1 || ds[0].Images["db"].Image != v.app || !strings.Contains(ds[0].Summary, "web created") {
		t.Fatalf("first deploy: %+v", ds)
	}
	exec(p, "write", "/data/f", "A")
	apply("apply")
	if len(history()) != 1 {
		t.Fatal("a no-op apply was recorded")
	}

	// an image change: a second row, with the pre-deploy snapshot of data A
	v.commit(compose(v2))
	c2 := head()
	apply("apply")
	ds = history()
	d2 := ds[0]
	if len(ds) != 2 || d2.Commit != c2 || d2.Images["web"].Image != v2 || d2.SnapshotID == 0 || d2.Trigger != "apply" {
		t.Fatalf("second deploy: %+v", ds)
	}
	if s, _ := v.e.DB.Snapshot(d2.SnapshotID); s.Reason != "pre-deploy" || s.Commit != c1 {
		t.Fatalf("snapshot of the second deploy: %+v", s)
	}
	exec(p, "write", "/data/f", "B")
	v.commit(compose(v3))
	apply("ui")
	exec(p, "write", "/data/f", "C")
	if _, body := v.get(host); body != "img-v3" {
		t.Fatalf("prod: %q", body)
	}

	// a failed deploy records the failure and what still runs (v3 kept serving)
	v.commit(compose("localhost/" + v.ns + "-nope:x"))
	if _, out, err := v.apply(ApplyOpts{}); err == nil {
		t.Fatalf("expected a failed deploy:\n%s", out)
	}
	if f := history()[0]; f.Result != "failed" || f.Error == "" || f.Images["web"].Image != v3 || !strings.Contains(f.Summary, "web failed") {
		t.Fatalf("failed deploy: %+v", f)
	}
	v.commit(compose(v3)) // back to what runs: nothing to do, nothing recorded
	c5 := head()
	apply("apply")
	if len(history()) != 4 {
		t.Fatalf("history: %+v", history())
	}

	// rollback to right before the second deploy: data A; web ran a local image then (no digest), so only data
	var buf strings.Builder
	rp, err := v.e.RollbackPlan(ctx, RollbackOpts{Project: p, Before: d2.ID})
	if err != nil || strings.Join(rp.Parts, ",") != "data" || rp.From != ds[1].ID || len(rp.Images) != 2 || rp.Images[1].Do != "skip" || !strings.Contains(rp.Images[1].Reason, "no digest") {
		t.Fatalf("rollback plan: %v %+v", err, rp)
	}
	if err := v.e.Rollback(ctx, &buf, RollbackOpts{Project: p, Before: d2.ID}); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if got := exec(p, "read", "/data/f"); got != "A" {
		t.Fatalf("after rollback: %q", got)
	}
	r1 := history()[0]
	if r1.Trigger != "rollback" || r1.RestoredID != d2.SnapshotID || r1.BeforeID != d2.ID || r1.Parts != "data" || r1.SnapshotID == 0 || r1.Undoes != 0 || r1.Result != "ok" || r1.Commit != c5 || r1.Images["web"].Image != v3 {
		t.Fatalf("rollback row: %+v", r1)
	}
	if last, _ := v.e.DB.LatestDeploys(); last[p].ID != r1.ID {
		t.Fatalf("latest deploy should be the rollback: %+v", last[p])
	}
	if err := v.e.Rollback(ctx, io.Discard, RollbackOpts{Project: p, Before: r1.ID}); err != nil {
		t.Fatal(err)
	}
	if got := exec(p, "read", "/data/f"); got != "C" {
		t.Fatalf("after undo: %q", got)
	}
	if r2 := history()[0]; r2.Undoes != r1.ID || r2.RestoredID != r1.SnapshotID || r2.Parts != "data" {
		t.Fatalf("undo row: %+v", r2)
	}
	manual, err := v.e.Snapshot(ctx, io.Discard, p, "keep")
	if err != nil {
		t.Fatal(err)
	}

	// the timeline: rows newest first with their snapshots and image changes, the manual snapshot as its own node
	tl, err := v.e.Timeline(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var node2, manualNode *Node
	for i, n := range tl.Nodes {
		kinds = append(kinds, n.Kind)
		if n.Deploy != nil && n.Deploy.ID == d2.ID {
			node2 = &tl.Nodes[i]
		}
		if n.Kind == "snapshot" && n.Snapshot.ID == manual.ID {
			manualNode = &tl.Nodes[i]
		}
	}
	if strings.Count(strings.Join(kinds, " "), "deploy") != 4 || strings.Count(strings.Join(kinds, " "), "rollback") != 2 || manualNode == nil || node2 == nil {
		t.Fatalf("timeline kinds: %v", kinds)
	}
	if node2.Snapshot == nil || node2.Snapshot.ID != d2.SnapshotID || len(node2.Changes) != 1 || node2.Changes[0].From.Image != v1 || node2.Changes[0].To.Image != v2 {
		t.Fatalf("node of the second deploy: %+v", node2)
	}
	if manualNode.Images["web"].Image != v3 || tl.Commit != c5 || tl.Subjects[c2] != "c" || tl.Images["web"].Image != v3 {
		t.Fatalf("timeline: %+v", tl)
	}

	// a preview from the second deploy: its commit, its images, the data right before it
	overrides := map[string]string{}
	for svc, img := range d2.Images {
		if !img.Built {
			overrides[svc] = img.Pinned()
		}
	}
	name := fmt.Sprintf("at-%d", d2.ID)
	if err := v.e.PreviewUp(ctx, &buf, PreviewOpts{Project: p, Name: name, Ref: d2.Commit, Images: overrides, From: d2.SnapshotID, Deploy: d2.ID}); err != nil {
		t.Fatalf("preview: %v\n%s", err, buf.String())
	}
	path := PreviewPath(p, name)
	if _, body := v.get("web." + name + ".app." + v.ns + "." + domain); body != "img-v2" {
		t.Fatalf("preview web: %q", body)
	}
	if got := exec(path, "read", "/data/f"); got != "A" {
		t.Fatalf("preview data: %q", got)
	}
	if wt := strings.TrimSpace(run(t, v.e.previewRoot(path), "git", "rev-parse", "HEAD")); wt != c2 {
		t.Fatalf("preview runs %s, not %s", wt, c2)
	}
	if _, body := v.get(host); body != "img-v3" || exec(p, "read", "/data/f") != "C" {
		t.Fatalf("production changed: %q", body)
	}
	if pv, _, _ := v.e.DB.Preview(p, name); pv.DeployID != d2.ID || pv.SnapshotID != d2.SnapshotID || pv.Commit != c2 {
		t.Fatalf("preview origin: %+v", pv)
	}
	tl, _ = v.e.Timeline(ctx, p)
	for _, n := range tl.Nodes {
		if want := n.Deploy != nil && n.Deploy.ID == d2.ID; want != slices.Equal(n.Previews, []string{name}) {
			t.Fatalf("preview chip on the wrong node: %+v", n)
		}
	}
	if err := v.e.PreviewUp(ctx, io.Discard, PreviewOpts{Project: p, Name: name, Deploy: d2.ID}); err == nil {
		t.Fatal("a second preview from a node with a taken name was accepted")
	}
	if err := v.e.PreviewRm(ctx, io.Discard, p, name); err != nil {
		t.Fatal(err)
	}
}
