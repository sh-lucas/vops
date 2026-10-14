package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/store"
)

// Release modes (compose's top-level x-vops.release). In a gated project (manual, canary) a push doesn't deploy:
// it records a pending release and the services keep running what they run. The hold doesn't depend on that row
// existing: a watched service on one of our tags moves to the tag's digest only when the newest release of that
// tag approved it; otherwise it runs the digest its newest deploy row recorded for the same image ref. So an apply
// racing the push, a rejected release or a failed approval never ships an unapproved digest. A different image ref
// in compose than the row's is an explicit change in git (shown in the plan like any other): no hold.

// Hold is a service kept on what it runs while its tag points at a digest nobody approved.
type Hold struct {
	Service string         `json:"service"`
	Image   string         `json:"image"`             // compose's tag
	Running string         `json:"running"`           // the digest it keeps running ("" = none: it never ran)
	Pushed  string         `json:"pushed"`            // the digest the tag points at
	Release *store.Release `json:"release,omitempty"` // the newest release of the tag, when it is about Pushed
}

// Text is the plan's line for a hold.
func (h Hold) Text() string {
	img := shortRef(h.Image) + " " + shortDigest(h.Pushed)
	run := "runs " + shortDigest(h.Running)
	if h.Running == "" {
		run = "not running yet"
	}
	switch {
	case h.Release == nil:
		return fmt.Sprintf("%s: held, %s was pushed but not approved (%s)", h.Service, img, run)
	case h.Release.Status == "pending":
		return fmt.Sprintf("%s: waiting approval of #%d, %s (%s)", h.Service, h.Release.ID, img, run)
	}
	return fmt.Sprintf("%s: #%d %s, %s (%s until a newer push)", h.Service, h.Release.ID, h.Release.Status, img, run)
}

// holdRelease applies the release rules to a resolved own-registry service (see the top of this file).
func (e *Engine) holdRelease(ctx context.Context, src source, mode compose.Release, d *desired, domain string) (*desired, error) {
	sp := d.spec
	rel, hasRel := src.releases[d.own]
	about := hasRel && rel.Digest == d.digest // the newest release of the tag is about the digest it points at
	if a := src.approve; a != nil && a.Repo+":"+a.Tag == d.own {
		if d.digest == a.Digest {
			return d, nil
		}
		return e.resolveAt(ctx, src.root, sp, domain, a.Digest)
	}
	if !mode.Gated() || !sp.Watch {
		return d, nil
	}
	ran, hasRan := src.ran[sp.Service]
	switch {
	case hasRan && ran.Image != sp.Image:
		return d, nil // compose changed the image, or it was pinned: explicit
	case hasRan && ran.Digest == d.digest:
		return d, nil // runs what the tag says
	case about && (rel.Status == "approved" || rel.Status == "failed"):
		return d, nil
	case about && rel.Status == "pending" && rel.Mode == "canary" && sp.Job:
		return d, nil // a canary's jobs (migrations) run at push
	}
	d.gate = "held"
	if !hasRan {
		d.gate = "new"
		if !about || rel.Status != "pending" && rel.Status != "rejected" {
			return d, nil // a service compose just added: its first deploy is an apply's
		}
	}
	h := &Hold{Service: sp.Service, Image: sp.Image, Running: ran.Digest, Pushed: d.digest}
	if about {
		h.Release = &rel
	}
	held := d
	if h.Running != "" {
		var err error
		if held, err = e.resolveAt(ctx, src.root, sp, domain, h.Running); err != nil {
			return nil, err
		}
	}
	held.hold, held.gate = h, d.gate
	if about && rel.Status == "pending" && rel.Mode == "canary" && canaryable(sp) && h.Running != "" {
		held.canary, held.release = d, &rel
	}
	return held, nil
}

// ReleaseEvent tells the daemon about a release: held (a push waits), approved, rejected, failed.
type ReleaseEvent struct {
	Release store.Release
	What    string
	Detail  string
}

// Pushed is the registry push trigger: services watching repo:tag in auto projects are deployed (ending their
// pins); in gated projects the push becomes a pending release (superseding the previous one of that tag).
func (e *Engine) Pushed(ctx context.Context, w io.Writer, p registry.Push) error {
	e.lock()
	defer e.unlock()
	w = &syncWriter{w: w}
	if e.Registry.Resolve(p.Repo, p.Tag) != p.Digest {
		return nil // pushed again since: that push handles it
	}
	plan, err := e.plan(ctx, nil)
	if err != nil {
		return err
	}
	var deploy []proxy.Key
	for _, pp := range plan.Projects {
		keys := slices.DeleteFunc(plan.Watching(p.Repo, p.Tag), func(k proxy.Key) bool { return k.Project != pp.Path })
		if len(keys) == 0 {
			continue
		}
		if !pp.project.Release.Gated() {
			for _, k := range keys {
				e.unpin(w, k.Project, k.Service, p.Repo+":"+p.Tag+" pushed")
			}
			deploy = append(deploy, keys...)
			continue
		}
		if r := e.holdPush(w, pp, keys, p); r != nil && r.Mode == "canary" {
			e.startCanary(ctx, w, *r, keys)
		}
	}
	if len(deploy) == 0 {
		return nil
	}
	_, err = e.apply(ctx, w, ApplyOpts{Services: deploy, Trigger: "push"})
	return err
}

// holdPush records a push to a gated project as a pending release, unless none of its services would change.
func (e *Engine) holdPush(w io.Writer, pp *ProjectPlan, keys []proxy.Key, p registry.Push) *store.Release {
	var waits []string
	mode := "manual"
	for _, k := range keys {
		if d := cmpOrDesired(pp.specs[k.Service].compose, pp.specs[k.Service]); d.gate != "" {
			waits = append(waits, k.Service)
			if pp.project.Release.Mode == "canary" && d.gate == "held" && canaryable(d.spec) {
				mode = "canary" // else (nothing routed with rolling) it is a manual release, jobs wait too
			}
		}
	}
	pending, _ := e.DB.PendingReleases()
	old := slices.IndexFunc(pending, func(r store.Release) bool { return r.Project == pp.Path && r.Repo == p.Repo && r.Tag == p.Tag })
	if len(waits) == 0 {
		if old >= 0 {
			e.decide(w, pending[old], "superseded", "", p.Repo+":"+p.Tag+" points at what runs again")
		}
		return nil
	}
	r := store.Release{Project: pp.Path, Repo: p.Repo, Tag: p.Tag, Digest: p.Digest, Mode: mode, PushedBy: p.User}
	if mode == "canary" {
		r.Weight = int64(pp.project.Release.Weight)
	}
	if old >= 0 {
		e.decide(w, pending[old], "superseded", "", "a newer push of "+p.Repo+":"+p.Tag)
	}
	id, err := e.DB.AddRelease(r)
	if err != nil {
		fmt.Fprintf(w, "%s: release: %v\n", pp.Path, err)
		e.DB.Event(pp.Path, "error", "release of %s:%s: %v", p.Repo, p.Tag, err)
		return nil
	}
	r, _, _ = e.DB.GetRelease(id)
	msg := fmt.Sprintf("#%d %s:%s %s pushed by %s: waiting approval (%s)", id, p.Repo, p.Tag, shortDigest(p.Digest), cmpOr(p.User, "?"), strings.Join(waits, ", "))
	fmt.Fprintf(w, "%s: %s\n", pp.Path, msg)
	e.DB.Event(pp.Path, "release", "%s", msg)
	e.released(r, "held", strings.Join(waits, ", "))
	return &r
}

func (e *Engine) released(r store.Release, what, detail string) {
	if e.OnRelease != nil {
		e.OnRelease(ReleaseEvent{r, what, detail})
	}
}

// decide closes a pending release (superseded, rejected) with an event.
func (e *Engine) decide(w io.Writer, r store.Release, status, by, note string) bool {
	ok, err := e.DB.DecideRelease(r.ID, status, by, note)
	if err != nil || !ok {
		return false
	}
	msg := fmt.Sprintf("#%d %s:%s %s %s", r.ID, r.Repo, r.Tag, shortDigest(r.Digest), status)
	if by != "" {
		msg += " by " + by
	}
	if note != "" {
		msg += ": " + note
	}
	fmt.Fprintf(w, "%s: %s\n", r.Project, msg)
	e.DB.Event(r.Project, "release", "%s", msg)
	r.Status, r.DecidedBy, r.Note = status, by, note
	e.released(r, status, note)
	return true
}

func (e *Engine) pendingRelease(id int64) (store.Release, error) {
	r, found, err := e.DB.GetRelease(id)
	switch {
	case err != nil:
		return r, err
	case !found:
		return r, fmt.Errorf("no release #%d", id)
	case r.Status != "pending":
		return r, fmt.Errorf("release #%d is %s, not pending", id, r.Status)
	}
	return r, nil
}

// Approve deploys exactly a pending release's digest to the services running its tag (rolling, with the usual
// pre-deploy snapshot and history row, trigger approve), ending their pins.
func (e *Engine) Approve(ctx context.Context, w io.Writer, id int64, by string) error {
	e.lock()
	defer e.unlock()
	w = &syncWriter{w: w}
	r, err := e.pendingRelease(id)
	if err != nil {
		return err
	}
	if now := e.Registry.Resolve(r.Repo, r.Tag); now != r.Digest {
		return fmt.Errorf("%s:%s points at %s now, not %s: reject #%d (a newer push makes its own release)", r.Repo, r.Tag, cmpOr(shortDigest(now), "nothing"), shortDigest(r.Digest), id)
	}
	if flags, _ := e.DB.Projects(); flags[r.Project].Disabled {
		return fmt.Errorf("%s is disabled: enable it first", r.Project)
	}
	if ok, err := e.DB.DecideRelease(id, "approved", by, ""); err != nil || !ok {
		return cmpOrErr(err, fmt.Errorf("release #%d is not pending anymore", id))
	}
	r.Status, r.DecidedBy = "approved", by
	msg := fmt.Sprintf("#%d %s:%s %s approved by %s", id, r.Repo, r.Tag, shortDigest(r.Digest), by)
	fmt.Fprintf(w, "%s: %s\n", r.Project, msg)
	e.DB.Event(r.Project, "release", "%s", msg)
	e.released(r, "approved", "")
	plan, err := e.plan(ctx, &r)
	if err != nil {
		return e.failRelease(r, err)
	}
	keys := slices.DeleteFunc(plan.Watching(r.Repo, r.Tag), func(k proxy.Key) bool { return k.Project != r.Project })
	if len(keys) == 0 {
		fmt.Fprintf(w, "%s: no service runs %s:%s anymore: nothing to deploy\n", r.Project, r.Repo, r.Tag)
		return nil
	}
	for _, k := range keys {
		e.unpin(w, k.Project, k.Service, fmt.Sprintf("release #%d approved", id))
	}
	if _, err := e.apply(ctx, w, ApplyOpts{Services: keys, Trigger: "approve", approve: &r}); err != nil {
		return e.failRelease(r, err)
	}
	return nil
}

func (e *Engine) failRelease(r store.Release, err error) error {
	e.DB.FailRelease(r.ID, shortErr(err))
	r.Status, r.Note = "failed", shortErr(err)
	e.released(r, "failed", r.Note)
	return err
}

// Reject drops a pending release. Its services keep what they run until a newer push of the tag (an apply must
// not deploy what was rejected).
func (e *Engine) Reject(ctx context.Context, w io.Writer, id int64, by, note string) error {
	e.lock()
	defer e.unlock()
	w = &syncWriter{w: w}
	r, err := e.pendingRelease(id)
	if err != nil {
		return err
	}
	if !e.decide(w, r, "rejected", by, note) {
		return fmt.Errorf("release #%d is not pending anymore", id)
	}
	if r.Mode == "canary" {
		e.dropCanaries(ctx, w, r)
	}
	return nil
}

// Schedule approves a pending release at a time (unix seconds, run by the housekeeping tick); 0 cancels.
func (e *Engine) Schedule(id, at int64, by string) error {
	r, err := e.pendingRelease(id)
	if err != nil {
		return err
	}
	if at != 0 && at < time.Now().Add(-time.Minute).Unix() {
		return errors.New("that time has passed: approve it now instead")
	}
	if ok, err := e.DB.ScheduleRelease(id, at, by); err != nil || !ok {
		return cmpOrErr(err, fmt.Errorf("release #%d is not pending anymore", id))
	}
	if at == 0 {
		e.DB.Event(r.Project, "release", "#%d: scheduled approval cancelled by %s", id, by)
	} else {
		e.DB.Event(r.Project, "release", "#%d will be approved at %s (scheduled by %s)", id, time.Unix(at, 0).Format("2006-01-02 15:04"), by)
	}
	return nil
}

// DueReleases approves the pending releases whose scheduled time has come, as whoever scheduled them.
func (e *Engine) DueReleases(ctx context.Context, w io.Writer, now time.Time) {
	pending, err := e.DB.PendingReleases()
	if err != nil {
		return
	}
	for _, r := range pending {
		if r.ApproveAt == 0 || r.ApproveAt > now.Unix() {
			continue
		}
		if err := e.Approve(ctx, w, r.ID, r.DecidedBy); err != nil {
			fmt.Fprintf(w, "%s: scheduled approval of #%d: %v\n", r.Project, r.ID, err)
			e.DB.Event(r.Project, "error", "scheduled approval of #%d: %v", r.ID, err)
			e.DB.ScheduleRelease(r.ID, 0, r.DecidedBy) // once: it stays pending for a human
		}
	}
}

func cmpOrErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

