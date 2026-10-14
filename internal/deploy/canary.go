package deploy

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/store"
)

// Canary releases (x-vops.release: canary). A push to such a project runs its jobs at once and gives every
// routed rolling service in the push set one canary container (whatever its replicas) with the pushed digest;
// the proxy sends it Weight% of the clients, sticky by cookie. The services themselves stay held like a manual
// release. Canary containers carry vops.role=canary, the release and the weight, so the routes are rebuilt from
// podman; they are not replicas (compare ignores them) and have no network alias (internal traffic never hits
// them). Approve = the usual rolling release to that digest, then the canary goes; Reject, a failed readiness, a
// crash or a restart of the canary remove it. A compose change of a held service rebuilds its canary.

// canaryable: services that get a canary; the others of a canary release just wait (like manual).
func canaryable(sp *compose.Spec) bool { return sp.Port > 0 && sp.Strategy == "rolling" && !sp.Job }

// CanaryState is a service's canary as the plan sees it: what should run and what runs.
type CanaryState struct {
	Service    string             `json:"service"`
	Release    int64              `json:"release"`
	Weight     int                `json:"weight"`
	Digest     string             `json:"digest"`
	Action     string             `json:"action"` // create | update | remove | none
	Since      int64              `json:"since,omitempty"`
	Containers []podman.Container `json:"containers"`

	want *desired
	rel  *store.Release
}

// Text is the plan's line for a canary.
func (c CanaryState) Text() string {
	switch c.Action {
	case "remove":
		return c.Service + " canary: remove (its release is over)"
	case "none":
		return fmt.Sprintf("%s canary of #%d: %d%% of clients on %s", c.Service, c.Release, c.Weight, shortDigest(c.Digest))
	}
	return fmt.Sprintf("%s canary of #%d: %s (%d%% of clients on %s)", c.Service, c.Release, c.Action, c.Weight, shortDigest(c.Digest))
}

func (pp *ProjectPlan) canaryStates() {
	pp.Canaries = nil
	names := map[string]bool{}
	for n, d := range pp.specs {
		if d.canary != nil {
			names[n] = true
		}
	}
	for n := range pp.canaries {
		names[n] = true
	}
	for _, n := range sortedKeys(names) {
		have := pp.canaries[n]
		c := CanaryState{Service: n, Containers: have, Action: "none"}
		if c.Containers == nil {
			c.Containers = []podman.Container{}
		}
		for _, x := range have {
			if c.Since == 0 || x.Created < c.Since {
				c.Since = x.Created
			}
		}
		d := pp.specs[n]
		if d == nil || d.canary == nil || pp.Error != "" {
			c.Action = "remove"
			if len(have) > 0 {
				c.Release, _ = strconv.ParseInt(have[0].Labels[LRelease], 10, 64)
			}
			pp.Canaries = append(pp.Canaries, c)
			continue
		}
		c.want, c.rel = d.canary, d.release
		c.Release, c.Weight, c.Digest = d.release.ID, int(d.release.Weight), d.canary.digest
		id := strconv.FormatInt(c.Release, 10)
		switch {
		case len(have) == 0:
			c.Action = "create"
		case len(have) > 1 || have[0].Labels[LHash] != d.canary.hash || have[0].Labels[LRelease] != id || have[0].State != "running":
			c.Action = "update"
		}
		pp.Canaries = append(pp.Canaries, c)
	}
}

// startCanary runs a canary release's jobs, then its canaries. A failure rejects it.
func (e *Engine) startCanary(ctx context.Context, w io.Writer, r store.Release, keys []proxy.Key) {
	plan, err := e.plan(ctx, nil)
	if err != nil {
		e.canaryFailed(ctx, w, r, err.Error())
		return
	}
	var jobs []proxy.Key
	for _, pp := range plan.Projects {
		for _, k := range keys {
			if pp.Path == k.Project && pp.specs[k.Service] != nil && pp.specs[k.Service].spec.Job {
				jobs = append(jobs, k)
			}
		}
	}
	if len(jobs) > 0 {
		if plan, err = e.apply(ctx, w, ApplyOpts{Services: jobs, Trigger: "push"}); err != nil {
			e.canaryFailed(ctx, w, r, "its jobs failed: "+shortErr(err))
			return
		}
		if plan, err = e.plan(ctx, nil); err != nil {
			e.canaryFailed(ctx, w, r, err.Error())
			return
		}
	}
	for _, pp := range plan.Projects {
		if pp.Path == r.Project {
			e.syncCanaries(ctx, w, pp, func(svc string) bool { return slices.Contains(keys, proxy.Key{Project: r.Project, Service: svc}) })
		}
	}
	e.RefreshRoutes(ctx)
}

// syncCanaries makes the canary containers of a project's services (those wants says) match the plan: one per
// service of a pending canary release, replaced when its definition or release changed, removed otherwise.
func (e *Engine) syncCanaries(ctx context.Context, w io.Writer, pp *ProjectPlan, wants func(string) bool) {
	failed := map[int64]bool{}
	for _, c := range pp.Canaries {
		if c.Action == "none" || !wants(c.Service) || c.rel != nil && failed[c.rel.ID] {
			continue
		}
		key := proxy.Key{Project: pp.Path, Service: c.Service}
		log := func(format string, args ...any) {
			fmt.Fprintf(w, "%s/%s: canary: %s\n", pp.Path, c.Service, fmt.Sprintf(format, args...))
		}
		if c.Action == "remove" {
			e.Routes.SetCanary(key, nil)
			time.Sleep(Drain)
			if err := e.remove(ctx, c.Containers); err != nil {
				log("remove: %v", err)
			} else {
				log("removed")
			}
			continue
		}
		r, err := e.runCanary(ctx, log, c.want, *c.rel)
		if err != nil {
			log("✗ %v", err)
			failed[c.rel.ID] = true
			e.canaryFailed(ctx, w, *c.rel, fmt.Sprintf("%s: %v", c.Service, shortErr(err)))
			continue
		}
		e.Routes.SetCanary(key, &proxy.Canary{ID: c.rel.ID, Weight: int(c.rel.Weight), Backends: []string{"127.0.0.1:" + strconv.Itoa(r.hostPort)}})
		log("ready, %d%% of clients (release #%d)", c.rel.Weight, c.rel.ID)
		if len(c.Containers) > 0 {
			time.Sleep(Drain)
			e.remove(ctx, c.Containers)
		}
	}
}

// runCanary starts a canary container and waits until it is ready; a failed one is removed.
func (e *Engine) runCanary(ctx context.Context, log func(string, ...any), want *desired, rel store.Release) (replica, error) {
	d := *want
	d.image = d.pull // by digest: the shared local tag stays what the service runs
	if err := e.prepare(ctx, log, &d); err != nil {
		return replica{}, err
	}
	r, err := e.startAs(ctx, &d, &rel)
	if err == nil {
		log("started %s, waiting until ready", r.name)
		err = e.waitReady(ctx, d.spec, r)
	}
	if err != nil && r.id != "" {
		if tail, _ := podman.Run(ctx, "logs", "--tail", "15", r.id); tail != "" {
			log("last logs of %s:\n%s", r.name, indent(tail))
		}
		e.expect(r.id, r.name)
		podman.Run(ctx, "rm", "-f", "-t", "0", r.id)
	}
	return r, err
}

// canaryFailed rejects a canary release (vops decided) and removes its canaries; the engine lock is held.
func (e *Engine) canaryFailed(ctx context.Context, w io.Writer, r store.Release, why string) {
	if e.decide(w, r, "rejected", "vops", "canary failed: "+why) {
		e.deployed(r.Project, fmt.Errorf("canary of release #%d (%s:%s) failed and was rejected: %s", r.ID, r.Repo, r.Tag, why))
	}
	e.dropCanaries(ctx, w, r)
}

// dropCanaries removes the canary containers of a release (after it was decided) and their routes.
func (e *Engine) dropCanaries(ctx context.Context, w io.Writer, r store.Release) {
	cs, err := podman.PS(ctx, LProject+"="+r.Project, LRole+"=canary", LRelease+"="+strconv.FormatInt(r.ID, 10))
	if err != nil || len(cs) == 0 {
		return
	}
	for _, c := range cs {
		e.Routes.SetCanary(proxy.Key{Project: r.Project, Service: c.Labels[LService]}, nil)
	}
	time.Sleep(Drain)
	if err := e.remove(ctx, cs); err != nil {
		fmt.Fprintf(w, "%s: removing the canaries of #%d: %v\n", r.Project, r.ID, err)
	}
	e.RefreshRoutes(ctx)
}

// CanaryDied is the monitor telling that a canary container died or restarted without vops stopping it: its
// release is rejected and its canaries removed.
func (e *Engine) CanaryDied(ctx context.Context, project, service, release, why string) {
	e.lock()
	defer e.unlock()
	id, _ := strconv.ParseInt(release, 10, 64)
	r, err := e.pendingRelease(id)
	if err != nil {
		return
	}
	e.canaryFailed(ctx, io.Discard, r, service+" "+why)
}

// CheckCanaries (housekeeping) rejects the releases whose canary isn't running and removes canaries whose
// release is over: the monitor's event stream can miss a death.
func (e *Engine) CheckCanaries(ctx context.Context, w io.Writer) {
	if cs, err := podman.PS(ctx, LRole+"=canary"); err != nil || len(cs) == 0 {
		return
	}
	e.lock()
	defer e.unlock()
	plan, err := e.plan(ctx, nil)
	if err != nil {
		return
	}
	for _, pp := range plan.Projects {
		orphans := false
		for _, c := range pp.Canaries {
			if c.Action == "remove" {
				orphans = true
			}
			if c.rel != nil && slices.ContainsFunc(c.Containers, func(x podman.Container) bool { return x.State != "running" }) {
				e.canaryFailed(ctx, w, *c.rel, c.Service+" is not running")
			}
		}
		if orphans {
			e.syncCanaries(ctx, w, pp, func(string) bool { return true })
		}
	}
}
