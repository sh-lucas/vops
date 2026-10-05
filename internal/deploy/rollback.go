package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/snapshot"
	"github.com/sh-lucas/vops/internal/store"
)

// Rollback returns a project to right before deploy #N: the images that ran then (the previous row's, pinned
// by digest) and the data right before it (#N's snapshot, the same meaning as every row's snapshot_id).
// Images roll back per service with a rolling release; data is always the whole project and stops it while
// it is put back. A pin lasts until a newer version arrives (see ApplyOpts.Unpin and PinState.Stale).

type RollbackOpts struct {
	Project  string   `json:"project"`
	Before   int64    `json:"before"` // right before this deploy; 0 = the newest deploy that isn't a rollback
	Images   bool     `json:"images"` // only these parts; neither = everything available (undoing a rollback: its parts)
	Data     bool     `json:"data"`
	Services []string `json:"services"` // only the images of these services
	Snapshot int64    `json:"snapshot"` // the data of this snapshot instead; images untouched
}

// RollbackPlan is what a rollback would do, shown before it runs (cli, dashboard dialog).
type RollbackPlan struct {
	Project  string          `json:"project"`
	Before   *store.Deploy   `json:"before,omitempty"`    // it goes back to right before this deploy
	From     int64           `json:"from"`                // the deploy whose images it goes back to (0: nothing ran before)
	Undoes   int64           `json:"undoes,omitempty"`    // the rollback it undoes
	Images   []ImageStep     `json:"images"`              // per service
	NoImages string          `json:"no_images,omitempty"` // why images can't roll back at all
	Snapshot *store.Snapshot `json:"snapshot,omitempty"`  // the data it can put back
	NoData   string          `json:"no_data,omitempty"`   // why it can't
	Commit   string          `json:"commit"`              // running now; the data's commit may differ
	Parts    []string        `json:"parts"`               // what it does: images, data
}

// ImageStep is one service of an image rollback.
type ImageStep struct {
	Service  string             `json:"service"`
	Now      *store.DeployImage `json:"now,omitempty"` // runs now
	To       *store.DeployImage `json:"to,omitempty"`  // ran right before
	Do       string             `json:"do"`            // pin | unpin (back to what compose says) | same | skip
	Reason   string             `json:"reason,omitempty"`
	Selected bool               `json:"selected"`
}

func partsOf(d store.Deploy) []string {
	if d.Parts == "" {
		return []string{"data"} // rollbacks before image rollback existed
	}
	return strings.Split(d.Parts, ",")
}

// RollbackPlan computes a rollback without doing it.
func (e *Engine) RollbackPlan(ctx context.Context, o RollbackOpts) (*RollbackPlan, error) {
	rp, _, err := e.rollbackPlan(ctx, o)
	return rp, err
}

func (e *Engine) rollbackPlan(ctx context.Context, o RollbackOpts) (*RollbackPlan, *ProjectPlan, error) {
	flags, err := e.DB.Projects()
	if err != nil {
		return nil, nil, err
	}
	rp := &RollbackPlan{Project: o.Project, Images: []ImageStep{}, Parts: []string{}, Commit: flags[o.Project].Commit}
	if o.Snapshot != 0 {
		if o.Before != 0 || o.Images || len(o.Services) > 0 {
			return nil, nil, errors.New("a snapshot restores data only: no deploy id, images or services with it")
		}
		s, err := e.snapshotFor(o.Project, o.Snapshot)
		if err != nil {
			return nil, nil, err
		}
		rp.Snapshot, rp.Parts, rp.NoImages = &s, []string{"data"}, "a snapshot restores data only"
		// restoring the undo point of a rollback undoes it
		recent, _ := e.DB.Deploys(o.Project, historyLimit)
		if i := slices.IndexFunc(recent, func(d store.Deploy) bool { return d.Trigger == "rollback" && d.SnapshotID == s.ID }); i >= 0 {
			rp.Undoes = recent[i].ID
		}
		return rp, nil, nil
	}

	var before store.Deploy
	if o.Before == 0 {
		recent, err := e.DB.Deploys(o.Project, historyLimit)
		if err != nil {
			return nil, nil, err
		}
		i := slices.IndexFunc(recent, func(d store.Deploy) bool { return d.Trigger != "rollback" })
		if i < 0 {
			return nil, nil, fmt.Errorf("%s has no deploy to roll back (vops history %s)", o.Project, o.Project)
		}
		before = recent[i]
	} else {
		d, found, err := e.DB.Deploy(o.Before)
		if err != nil {
			return nil, nil, err
		}
		if !found || d.Project != o.Project {
			return nil, nil, fmt.Errorf("no deploy #%d of %s (vops history %s)", o.Before, o.Project, o.Project)
		}
		before = d
	}
	rp.Before = &before
	if before.Trigger == "rollback" {
		rp.Undoes = before.ID
	}
	strict := o.Images || o.Data || len(o.Services) > 0
	wantImages, wantData := o.Images || len(o.Services) > 0, o.Data
	if !strict {
		wantImages, wantData = true, true
		if rp.Undoes != 0 { // undoing a rollback: the same parts it used
			wantImages, wantData = slices.Contains(partsOf(before), "images"), slices.Contains(partsOf(before), "data")
		}
	}

	// data: the snapshot right before #N
	switch {
	case !e.snapshotsOn():
		rp.NoData = "snapshots are off (snapshots: off in vops.yml)"
	case before.SnapshotID == 0:
		rp.NoData = fmt.Sprintf("no snapshot right before #%d: no data then, or snapshots were off", before.ID)
	default:
		s, err := e.snapshotFor(o.Project, before.SnapshotID)
		if err != nil {
			rp.NoData = err.Error()
		} else {
			rp.Snapshot = &s
		}
	}

	// images: what the deploy before #N left running, against what compose asks for now
	plan, err := e.Plan(ctx)
	if err != nil {
		return nil, nil, err
	}
	var pp *ProjectPlan
	for _, x := range plan.Projects {
		if x.Path == o.Project {
			pp = x
		}
	}
	prev, found, err := e.DB.PreviousDeploy(o.Project, before.ID)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case pp == nil || pp.Gone:
		rp.NoImages = o.Project + " is not in git"
	case pp.Disabled:
		rp.NoImages = o.Project + " is disabled"
	case pp.Error != "":
		rp.NoImages = "fix the compose file first: " + pp.Error
	case !found:
		rp.NoImages = fmt.Sprintf("nothing ran before #%d", before.ID)
	default:
		rp.From = prev.ID
		names := map[string]bool{}
		for s := range pp.specs {
			names[s] = true
		}
		for s := range prev.Images {
			names[s] = true
		}
		for _, svc := range sortedKeys(names) {
			st := ImageStep{Service: svc}
			d := pp.specs[svc]
			to, had := prev.Images[svc]
			if had {
				st.To = &to
			}
			if d != nil {
				now := imageOf(ctx, d)
				st.Now = &now
			}
			switch {
			case d == nil:
				st.Do, st.Reason = "skip", "not in compose anymore"
			case !had:
				st.Do, st.Reason = "skip", fmt.Sprintf("didn't run before #%d (remove it in git)", before.ID)
			case sameImage(to, imageOf(ctx, cmpOrDesired(d.compose, d))):
				st.Do = map[bool]string{true: "unpin", false: "same"}[d.pin != nil]
			case d.pin != nil && d.pin.Digest == to.Digest:
				st.Do = "same"
			case to.Built:
				st.Do, st.Reason = "skip", "built from the code: rolling it back needs git"
			case to.Digest == "":
				st.Do, st.Reason = "skip", "no digest recorded (a local image): it can't be pulled back"
			case !e.available(to):
				st.Do, st.Reason = "skip", "gone from the vops registry (older than image_keep)"
			default:
				st.Do = "pin"
			}
			st.Selected = wantImages && (st.Do == "pin" || st.Do == "unpin") && (len(o.Services) == 0 || slices.Contains(o.Services, svc))
			rp.Images = append(rp.Images, st)
		}
	}
	for _, svc := range o.Services {
		i := slices.IndexFunc(rp.Images, func(st ImageStep) bool { return st.Service == svc })
		switch {
		case rp.NoImages != "":
			return nil, nil, fmt.Errorf("images can't roll back: %s", rp.NoImages)
		case i < 0:
			return nil, nil, fmt.Errorf("%s has no service %s", o.Project, svc)
		case rp.Images[i].Do == "skip":
			return nil, nil, fmt.Errorf("%s can't roll back: %s", svc, rp.Images[i].Reason)
		}
	}
	if wantImages && slices.ContainsFunc(rp.Images, func(st ImageStep) bool { return st.Selected }) {
		rp.Parts = append(rp.Parts, "images")
	}
	if wantData && rp.Snapshot != nil {
		rp.Parts = append(rp.Parts, "data")
	}
	if o.Data && rp.Snapshot == nil {
		return nil, nil, fmt.Errorf("no data to put back: %s", rp.NoData)
	}
	return rp, pp, nil
}

func cmpOrDesired(a, b *desired) *desired {
	if a != nil {
		return a
	}
	return b
}

// snapshotFor returns a complete snapshot of a project.
func (e *Engine) snapshotFor(project string, id int64) (store.Snapshot, error) {
	s, err := e.DB.Snapshot(id)
	if err != nil {
		return s, fmt.Errorf("snapshot #%d is gone (pruned by snapshot_keep, or deleted)", id)
	}
	if s.Project != project {
		return s, fmt.Errorf("snapshot #%d belongs to %s, not %s", id, s.Project, project)
	}
	for _, v := range s.Volumes {
		if !snapshot.IsSubvolume(v.Path) {
			return s, fmt.Errorf("snapshot #%d is incomplete: %s is missing", id, v.Path)
		}
	}
	return s, nil
}

// Why says why a plan does nothing.
func (rp *RollbackPlan) Why() string {
	var out []string
	if rp.NoImages != "" {
		out = append(out, "images: "+rp.NoImages)
	} else if !slices.ContainsFunc(rp.Images, func(st ImageStep) bool { return st.Do == "pin" || st.Do == "unpin" }) {
		out = append(out, "images: already as they were")
	}
	if rp.NoData != "" {
		out = append(out, "data: "+rp.NoData)
	}
	return strings.Join(out, "; ")
}

// Summary is the rollback as one line of history.
func (rp *RollbackPlan) Summary() string {
	var out []string
	if rp.Before != nil && rp.Undoes == 0 {
		out = append(out, fmt.Sprintf("back to before #%d", rp.Before.ID))
	}
	for _, st := range rp.Images {
		if st.Selected {
			out = append(out, st.Service+" → "+shortRef(st.To.Pinned()))
		}
	}
	if slices.Contains(rp.Parts, "data") {
		out = append(out, fmt.Sprintf("data restored to #%d", rp.Snapshot.ID))
	}
	s := strings.Join(out, ", ")
	if rp.Undoes != 0 {
		s = fmt.Sprintf("undid #%d: %s", rp.Undoes, s)
	}
	return s
}

// Print writes the plan for humans (the cli shows it before asking).
func (rp *RollbackPlan) Print(w io.Writer) {
	if rp.Before != nil {
		fmt.Fprintf(w, "roll %s back to right before #%d (%s at %s, commit %.12s)\n", rp.Project, rp.Before.ID, rp.Before.Trigger, stamp(rp.Before.StartedAt), rp.Before.Commit)
	} else {
		fmt.Fprintf(w, "restore %s data to snapshot #%d\n", rp.Project, rp.Snapshot.ID)
	}
	if rp.Undoes != 0 {
		fmt.Fprintf(w, "  this undoes rollback #%d\n", rp.Undoes)
	}
	if rp.NoImages != "" && rp.Before != nil {
		fmt.Fprintf(w, "  images: %s\n", rp.NoImages)
	}
	for _, st := range rp.Images {
		now, to := "-", "-"
		if st.Now != nil {
			now = imageText(*st.Now)
		}
		if st.To != nil {
			to = imageText(*st.To)
		}
		switch {
		case st.Selected && st.Do == "unpin":
			fmt.Fprintf(w, "  ~ %s: %s → %s (unpinned: what compose says)\n", st.Service, now, to)
		case st.Selected:
			fmt.Fprintf(w, "  ~ %s: %s → %s\n", st.Service, now, to)
		case st.Do == "skip":
			fmt.Fprintf(w, "  ! %s: skipped, %s\n", st.Service, st.Reason)
		case st.Do == "same":
			fmt.Fprintf(w, "  = %s: %s (unchanged)\n", st.Service, now)
		default:
			fmt.Fprintf(w, "  = %s: %s (not selected)\n", st.Service, now)
		}
	}
	if slices.Contains(rp.Parts, "data") {
		s := rp.Snapshot
		var vols []string
		for _, v := range s.Volumes {
			vols = append(vols, v.Name)
		}
		fmt.Fprintf(w, "  data: snapshot #%d (%s, %s): %s\n", s.ID, s.Reason, stamp(s.CreatedAt), strings.Join(vols, ", "))
		fmt.Fprintln(w, "  the project's containers stop while the data is put back (seconds); the current data is snapshotted first, so this can be undone.")
		if s.Commit != "" && rp.Commit != "" && s.Commit != rp.Commit {
			fmt.Fprintf(w, "  this data belongs to commit %.12s, the project runs %.12s. to run that code too:\n", s.Commit, rp.Commit)
			fmt.Fprintf(w, "    git restore --source=%.12s --staged --worktree -- %s/ && git commit -m \"%s: back to %.8s\" && vops sync\n", s.Commit, rp.Project, rp.Project, s.Commit)
		}
	} else if rp.NoData != "" {
		fmt.Fprintf(w, "  data: not restored (%s)\n", rp.NoData)
	} else if rp.Snapshot != nil {
		fmt.Fprintf(w, "  data: not restored (snapshot #%d is available: --data)\n", rp.Snapshot.ID)
	}
	if slices.Contains(rp.Parts, "images") && !slices.Contains(rp.Parts, "data") {
		fmt.Fprintln(w, "  images roll out one service at a time (rolling), no downtime; compose and code are not touched.")
	}
	if slices.Contains(rp.Parts, "images") {
		fmt.Fprintln(w, "  pinned services keep these images until a newer version is pushed, compose changes their image, or vops unpin.")
	}
	if len(rp.Parts) == 0 {
		fmt.Fprintf(w, "nothing to roll back: %s\n", rp.Why())
	}
}

// imageText is an image for humans: "registry.x/shop/web:v1 (sha256:0123456789ab)".
func imageText(i store.DeployImage) string {
	s, _, _ := strings.Cut(i.Image, "@")
	if i.Digest != "" {
		s += fmt.Sprintf(" (%.19s)", i.Digest)
	}
	return s
}

func stamp(unix int64) string { return time.Unix(unix, 0).Format("2006-01-02 15:04") }

// fetch pulls an image by digest, so a rollback fails before touching anything when an image is gone.
func (e *Engine) fetch(ctx context.Context, img store.DeployImage) error {
	ref := img.Pinned()
	if repo, digest, ok := e.ownImage(ref, e.domain()); ok {
		if e.Registry.Resolve(repo, digest) == "" {
			return fmt.Errorf("%s is gone from the vops registry", shortRef(ref))
		}
		_, err := podman.Run(ctx, "pull", "-q", "--tls-verify=false", "--authfile", e.PullAuthFile, e.PullAddr+"/"+repo+"@"+digest)
		return err
	}
	if podman.ImageID(ctx, ref) != "" {
		return nil
	}
	_, err := podman.Run(ctx, "pull", "-q", ref)
	return err
}

// Rollback does what RollbackPlan shows: images only is an apply with pins (rolling, no downtime); data stops
// the project, snapshots the current data (pre-rollback, the undo point), restores, then starts it with the
// pinned images. One history row, trigger rollback.
func (e *Engine) Rollback(ctx context.Context, w io.Writer, o RollbackOpts) (err error) {
	e.lock()
	defer e.unlock()
	w = &syncWriter{w: w}
	rp, pp, err := e.rollbackPlan(ctx, o)
	if err != nil {
		return err
	}
	if len(rp.Parts) == 0 {
		return fmt.Errorf("nothing to roll back: %s", rp.Why())
	}
	defer func() {
		if err != nil {
			e.deployed(o.Project, fmt.Errorf("rollback: %w", err))
		}
	}()
	images, data := slices.Contains(rp.Parts, "images"), slices.Contains(rp.Parts, "data")
	var steps []ImageStep
	for _, st := range rp.Images {
		if st.Selected {
			steps = append(steps, st)
		}
	}
	// preflight: every image must be here before anything stops
	for _, st := range steps {
		if st.Do == "pin" {
			fmt.Fprintf(w, "%s/%s: preflight: pulling %s\n", o.Project, st.Service, shortRef(st.To.Pinned()))
			if err := e.fetch(ctx, *st.To); err != nil {
				return fmt.Errorf("%s: %w; nothing was changed", st.Service, err)
			}
		}
	}

	project := o.Project
	rec := store.Deploy{Project: project, Commit: rp.Commit, Trigger: "rollback", Undoes: rp.Undoes, Parts: strings.Join(rp.Parts, ","), Summary: rp.Summary(), StartedAt: time.Now().Unix()}
	if rp.Before != nil {
		rec.BeforeID = rp.Before.ID
	}
	var specs map[string]*desired
	defer func() {
		e.RefreshRoutes(ctx)
		if id := e.record(ctx, rec, specs, err); id != 0 && err == nil {
			fmt.Fprintf(w, "%s: undo with: vops rollback %s %d\n", project, project, id)
		}
	}()

	var stopped []podman.Container
	start := func(skip []proxy.Key) {
		for _, c := range stopped {
			if slices.Contains(skip, proxy.Key{Project: project, Service: c.Labels[LService]}) {
				continue
			}
			if _, gone := podman.Run(ctx, "container", "exists", c.ID); gone != nil {
				continue // replaced by a deploy
			}
			if _, err := podman.Run(ctx, "start", c.ID); err != nil {
				fmt.Fprintf(w, "%s: start %s: %v\n", project, c.Name(), err)
			}
		}
	}
	if data {
		s := rp.Snapshot
		rec.RestoredID = s.ID
		if stopped, err = e.stopProject(ctx, w, project); err != nil {
			start(nil)
			return err
		}
		undo, err := e.restore(ctx, w, project, *s, rp.Commit)
		rec.SnapshotID = undo
		if err != nil {
			start(nil)
			return err
		}
	}

	var keys []proxy.Key
	for _, st := range steps {
		keys = append(keys, proxy.Key{Project: project, Service: st.Service})
		if st.Do == "unpin" {
			e.unpin(w, project, st.Service, fmt.Sprintf("rolled back to before #%d, which ran what compose says", rp.Before.ID))
			continue
		}
		pin := store.Pin{Project: project, Service: st.Service, Image: st.To.Image, Digest: st.To.Digest, ComposeImage: composeRef(cmpOrDesired(pp.specs[st.Service].compose, pp.specs[st.Service])), DeployID: rp.From}
		if err := e.DB.PutPin(pin); err != nil {
			start(nil)
			return err
		}
		e.DB.Event(project, "rollback", "%s pinned to #%d (%s), compose says %s", st.Service, rp.From, shortRef(store.PinRef(pin)), pin.ComposeImage)
	}
	start(keys) // unchanged services come back as they were; the pinned ones are deployed below
	if images {
		plan, err := e.Plan(ctx)
		if err != nil {
			start(nil)
			return err
		}
		for _, x := range plan.Projects {
			if x.Path == project {
				pp = x
			}
		}
		specs = pp.specs
		err = e.deployProject(ctx, w, plan, pp, ApplyOpts{Services: keys, Trigger: "rollback", noSnapshot: data}, &rec)
		start(nil) // a failed deploy left the old replicas: run them rather than nothing
		rec.Summary = rp.Summary()
		if err != nil {
			return err
		}
	}
	msg := fmt.Sprintf("rolled back (%s): %s", strings.Join(rp.Parts, ", "), rp.Summary())
	fmt.Fprintf(w, "%s: %s\n", project, msg)
	e.DB.Event(project, "rollback", "%s", msg)
	return nil
}

// stopProject stops a project's running containers and returns them.
func (e *Engine) stopProject(ctx context.Context, w io.Writer, project string) ([]podman.Container, error) {
	cs, err := podman.PS(ctx, LProject+"="+project)
	if err != nil {
		return nil, err
	}
	var running []podman.Container
	for _, c := range cs {
		if c.State == "running" {
			running = append(running, c)
		}
	}
	fmt.Fprintf(w, "%s: stopping %d container(s)\n", project, len(running))
	e.expectAll(running)
	for i, c := range running {
		if _, err := podman.Run(ctx, "stop", "-t", cmpOr(c.Labels["vops.stopwait"], "10"), c.ID); err != nil {
			return running[:i+1], err
		}
	}
	return running, nil
}

// restore snapshots the current data of the snapshot's volumes (pre-rollback, the undo point) and puts the
// snapshot back, volume by volume. The project must be stopped. It returns the undo point (0 = none).
func (e *Engine) restore(ctx context.Context, w io.Writer, project string, s store.Snapshot, commit string) (int64, error) {
	var current []store.SnapshotVolume
	for _, v := range s.Volumes {
		src := v.Source
		if v.Kind == "volume" {
			if p := podman.VolumePath(ctx, v.Name); p != "" {
				src = p
			}
		}
		if snapshot.IsSubvolume(src) {
			current = append(current, store.SnapshotVolume{Kind: v.Kind, Name: v.Name, Source: src})
		}
	}
	undo, err := e.snap(ctx, w, store.Snapshot{Project: project, Reason: "pre-rollback", Note: fmt.Sprintf("before rolling back to #%d", s.ID), Commit: commit, Volumes: current}, nil)
	if err != nil && !errors.Is(err, errNoData) {
		return 0, fmt.Errorf("could not snapshot the current data, nothing was restored: %w", err)
	}
	for _, v := range s.Volumes {
		target := v.Source
		if v.Kind == "volume" {
			if podman.VolumePath(ctx, v.Name) == "" {
				if _, err := podman.Run(ctx, "volume", "create", "--label", LProject+"="+project, v.Name); err != nil {
					return undo.ID, err
				}
			}
			target = podman.VolumePath(ctx, v.Name)
		}
		fmt.Fprintf(w, "%s: restoring %s\n", project, v.Name)
		if err := snapshot.Restore(ctx, v.Path, target); err != nil {
			return undo.ID, fmt.Errorf("restore %s: %w (undo with: vops rollback %s --snapshot %d)", v.Name, err, project, undo.ID)
		}
	}
	fmt.Fprintf(w, "%s: data restored to #%d (%s, commit %s)\n", project, s.ID, s.Reason, short(s.Commit))
	return undo.ID, nil
}
