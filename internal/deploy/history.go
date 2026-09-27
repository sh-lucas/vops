package deploy

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/store"
)

// Deploy history: every apply that changes a project, and every rollback, is a row in `deploys` with the
// commit, the images that run afterwards (digest-pinned when known) and the snapshot of the data right before
// it. The timeline merges these rows with manual snapshots and previews, newest first.

// historyLimit is how many deploys of a project the timeline shows.
const historyLimit = 100

var pastTense = map[string]string{"create": "created", "update": "updated", "start": "started", "remove": "removed"}

// record finishes a history row and stores it. Failing to record never fails the deploy.
func (e *Engine) record(ctx context.Context, rec store.Deploy, specs map[string]*desired, err error) {
	rec.FinishedAt = time.Now().Unix()
	rec.Result = "ok"
	if err != nil {
		rec.Result, rec.Error = "failed", shortErr(err)
	}
	var prev map[string]store.DeployImage
	if last, _ := e.DB.Deploys(rec.Project, 1); len(last) > 0 {
		prev = last[0].Images
	}
	rec.Images = e.runningImages(ctx, rec.Project, specs, prev)
	e.DB.AddDeploy(rec)
}

func shortErr(err error) string {
	s := strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "\n", "; ")), " ")
	if len(s) > 300 {
		s = s[:297] + "..."
	}
	return s
}

// runningImages is what a project runs now, per service: the desired image when the containers run the
// desired definition (its hash covers our registry's digest), else what the previous row said.
func (e *Engine) runningImages(ctx context.Context, project string, specs map[string]*desired, prev map[string]store.DeployImage) map[string]store.DeployImage {
	out := map[string]store.DeployImage{}
	cs, err := podman.PS(ctx, LProject+"="+project)
	if err != nil {
		return prev
	}
	by := map[string][]podman.Container{}
	for _, c := range cs {
		by[c.Labels[LService]] = append(by[c.Labels[LService]], c)
	}
	for svc, list := range by {
		d := specs[svc]
		if d != nil && !slices.ContainsFunc(list, func(c podman.Container) bool { return c.Labels[LHash] != d.hash }) {
			img := store.DeployImage{Image: d.spec.Image, Digest: d.digest, Built: d.spec.Build != nil}
			if img.Built {
				img.Image = d.image
			}
			if img.Digest == "" && !img.Built {
				img.Digest = imageDigest(ctx, d.image)
			}
			out[svc] = img
		} else if p, ok := prev[svc]; ok {
			out[svc] = p
		} else {
			out[svc] = store.DeployImage{Image: list[0].Labels[LImage]}
		}
	}
	return out
}

// imageDigest is the manifest digest of a pulled image, "" for local ones (it can't be pulled by it).
func imageDigest(ctx context.Context, image string) string {
	if _, digest, ok := strings.Cut(image, "@"); ok {
		return digest
	}
	if strings.HasPrefix(compose.NormalizeImage(image), "localhost/") {
		return ""
	}
	out, err := podman.Run(ctx, "image", "inspect", "--format", "{{.Digest}}", image)
	if err != nil || !strings.HasPrefix(out, "sha256:") {
		return ""
	}
	return out
}

// Timeline is a project's history as the dashboard and `vops history` show it.
type Timeline struct {
	Project   string                       `json:"project"`
	Commit    string                       `json:"commit"` // applied now
	AppliedAt int64                        `json:"applied_at"`
	Images    map[string]store.DeployImage `json:"images"` // what runs now (from the newest row)
	Data      DataState                    `json:"data"`
	Subjects  map[string]string            `json:"subjects"` // commit -> first line of its message
	Nodes     []Node                       `json:"nodes"`    // newest first
}

// Node is one point of the timeline: a deploy, a rollback, or a snapshot not taken by either (manual ones).
type Node struct {
	Kind     string                       `json:"kind"` // deploy | rollback | snapshot
	At       int64                        `json:"at"`
	Deploy   *store.Deploy                `json:"deploy,omitempty"`
	Snapshot *store.Snapshot              `json:"snapshot,omitempty"` // the data right before this event (or the snapshot itself); nil when none or pruned
	Commit   string                       `json:"commit"`             // deploys: the commit deployed; snapshots: the commit running when taken
	Images   map[string]store.DeployImage `json:"images"`
	Changes  []ImageChange                `json:"changes"` // images vs the previous deploy
	Previews []string                     `json:"previews"`
}

// ImageChange is a service whose image differs from the previous deploy (From nil: new service; To nil: gone).
type ImageChange struct {
	Service string             `json:"service"`
	From    *store.DeployImage `json:"from,omitempty"`
	To      *store.DeployImage `json:"to,omitempty"`
}

// Text is a change for the cli: "web: :v1 → :v2".
func (c ImageChange) Text() string {
	switch {
	case c.From == nil:
		return c.Service + ": + " + c.To.Image
	case c.To == nil:
		return c.Service + ": removed"
	case c.From.Image == c.To.Image:
		return c.Service + ": " + shortDigest(c.From.Digest) + " → " + shortDigest(c.To.Digest)
	}
	return c.Service + ": " + c.From.Image + " → " + c.To.Image
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return "@" + d
}

func imageChanges(old, cur map[string]store.DeployImage) []ImageChange {
	out := []ImageChange{}
	names := map[string]bool{}
	for s := range old {
		names[s] = true
	}
	for s := range cur {
		names[s] = true
	}
	for _, s := range sortedKeys(names) {
		o, had := old[s]
		n, has := cur[s]
		switch {
		case !had:
			out = append(out, ImageChange{Service: s, To: &n})
		case !has:
			out = append(out, ImageChange{Service: s, From: &o})
		case o.Image != n.Image || o.Digest != n.Digest && o.Digest != "" && n.Digest != "":
			out = append(out, ImageChange{Service: s, From: &o, To: &n})
		}
	}
	return out
}

// Timeline merges a project's deploys, rollbacks, manual snapshots and previews, newest first.
func (e *Engine) Timeline(ctx context.Context, project string) (Timeline, error) {
	t := Timeline{Project: project, Images: map[string]store.DeployImage{}, Subjects: map[string]string{}, Nodes: []Node{}}
	flags, err := e.DB.Projects()
	if err != nil {
		return t, err
	}
	t.Commit, t.AppliedAt = flags[project].Commit, flags[project].AppliedAt
	deploys, err := e.DB.Deploys(project, historyLimit+1)
	if err != nil {
		return t, err
	}
	snaps, err := e.DB.Snapshots(project)
	if err != nil {
		return t, err
	}
	previews, err := e.DB.Previews(project)
	if err != nil {
		return t, err
	}
	t.Data = e.DataState(ctx, project)
	if len(deploys) > 0 {
		t.Images = deploys[0].Images
	}
	byID := map[int64]*store.Snapshot{}
	for i := range snaps {
		byID[snaps[i].ID] = &snaps[i]
	}
	used := map[int64]bool{}
	for i, d := range deploys {
		if i == historyLimit {
			break
		}
		n := Node{Kind: "deploy", At: d.StartedAt, Deploy: &deploys[i], Snapshot: byID[d.SnapshotID], Commit: d.Commit, Images: d.Images}
		if d.Trigger == "rollback" {
			n.Kind = "rollback"
		}
		if i+1 < len(deploys) {
			n.Changes = imageChanges(deploys[i+1].Images, d.Images)
		} else {
			n.Changes = imageChanges(nil, d.Images)
		}
		used[d.SnapshotID] = true
		t.Nodes = append(t.Nodes, n)
	}
	for i, s := range snaps {
		if used[s.ID] {
			continue
		}
		n := Node{Kind: "snapshot", At: s.CreatedAt, Snapshot: &snaps[i], Commit: s.Commit, Images: map[string]store.DeployImage{}, Changes: []ImageChange{}}
		for _, d := range deploys { // newest first: the first one started before it ran then
			if d.StartedAt <= s.CreatedAt {
				n.Images = d.Images
				break
			}
		}
		t.Nodes = append(t.Nodes, n)
	}
	// newest first; at the same second a deploy comes after (above) a snapshot: snapshots never run during one
	slices.SortStableFunc(t.Nodes, func(a, b Node) int {
		if a.At != b.At {
			return int(b.At - a.At)
		}
		return strings.Compare(a.Kind, b.Kind) // deploy, rollback < snapshot
	})
	for i := range t.Nodes {
		n := &t.Nodes[i]
		n.Previews = []string{}
		for _, pv := range previews {
			if pv.DeployID != 0 && n.Deploy != nil && n.Deploy.ID == pv.DeployID ||
				pv.DeployID == 0 && pv.SnapshotID != 0 && n.Snapshot != nil && n.Snapshot.ID == pv.SnapshotID {
				n.Previews = append(n.Previews, pv.Name)
			}
		}
	}
	shas := []string{}
	add := func(c string) {
		if c != "" && !slices.Contains(shas, c) {
			shas = append(shas, c)
		}
	}
	add(t.Commit)
	for _, n := range t.Nodes {
		add(n.Commit)
		if n.Snapshot != nil {
			add(n.Snapshot.Commit)
		}
	}
	if len(shas) > 0 {
		out, _ := git(ctx, e.Repo, append([]string{"log", "--no-walk", "--ignore-missing", "--format=%H%x00%s"}, shas...)...)
		for line := range strings.SplitSeq(out, "\n") {
			if sha, subject, ok := strings.Cut(line, "\x00"); ok {
				t.Subjects[sha] = subject
			}
		}
	}
	return t, nil
}
