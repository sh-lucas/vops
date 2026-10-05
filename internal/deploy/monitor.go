package deploy

import (
	"strconv"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/podman"
)

// What the notifications need from the engine: which container deaths it caused, whether it is busy, what to probe.

func (e *Engine) lock() {
	e.mu.Lock()
	e.busy.Store(true)
	e.gen.Add(1)
}

func (e *Engine) unlock() {
	e.gen.Add(1)
	e.busy.Store(false)
	e.mu.Unlock()
}

// Busy reports whether an operation that starts or stops containers is running, and a generation that changes
// whenever one starts or ends: a probe that saw the same generation before and after ran while nothing moved.
func (e *Engine) Busy() (uint64, bool) { return e.gen.Load(), e.busy.Load() }

// expect marks containers (ids or names) the engine is about to stop, remove or restart: their deaths are not alerts.
// The mark outlives the operation, since podman's events arrive after it.
func (e *Engine) expect(refs ...string) {
	until := time.Now().Add(10 * time.Minute)
	for _, r := range refs {
		if r != "" {
			e.expected.Store(r, until)
		}
	}
}

func (e *Engine) expectAll(cs []podman.Container) {
	for _, c := range cs {
		e.expect(c.ID, c.Name())
	}
}

// Expected reports whether the engine stopped this container (by id or name) recently, and forgets the mark:
// one stop is one death.
func (e *Engine) Expected(refs ...string) bool {
	now := time.Now()
	found := false
	e.expected.Range(func(k, v any) bool {
		if now.After(v.(time.Time)) {
			e.expected.Delete(k)
		}
		return true
	})
	for _, r := range refs {
		if _, ok := e.expected.LoadAndDelete(r); ok && r != "" {
			found = true
		}
	}
	return found
}

func (e *Engine) deployed(project string, err error) {
	if e.OnDeploy != nil {
		e.OnDeploy(project, err)
	}
}

// Probe is a deployed service of an enabled project the monitor watches.
type Probe struct {
	Project     string
	Service     string
	Port        int    // routed: the container port (0 = not routed)
	Health      string // x-vops.health path
	Healthcheck bool   // compose healthcheck
	Replicas    []Replica
}

// Replica is a running container of a probed service; HostPort is its routed 127.0.0.1 port.
type Replica struct {
	ID, Name string
	HostPort int
}

// Probes lists what to monitor: services of enabled, valid, non-preview projects that were deployed (have containers)
// and are routed or have a healthcheck. Jobs and services removed from compose are left out.
func (p *Plan) Probes() []Probe {
	var out []Probe
	for _, pp := range p.Projects {
		if pp.Disabled || pp.Gone || pp.Error != "" || compose.IsPreview(pp.Path) {
			continue
		}
		for _, name := range sortedKeys(pp.specs) {
			d, actual := pp.specs[name], pp.actual[name]
			if d.spec.Job || len(actual) == 0 || d.spec.Port == 0 && !d.spec.Healthcheck {
				continue
			}
			pr := Probe{Project: pp.Path, Service: name, Port: d.spec.Port, Health: d.spec.Health, Healthcheck: d.spec.Healthcheck}
			for _, c := range actual {
				if c.State == "running" {
					port, _ := strconv.Atoi(c.Labels[LHostPort])
					pr.Replicas = append(pr.Replicas, Replica{c.ID, c.Name(), port})
				}
			}
			out = append(out, pr)
		}
	}
	return out
}

// ProjectRepos lists, per project, the repos of our registry its services run (who may see the project's alerts).
func (p *Plan) ProjectRepos() map[string][]string {
	out := map[string][]string{}
	for _, pp := range p.Projects {
		for _, name := range sortedKeys(pp.specs) {
			if r := pp.specs[name].repo; r != "" {
				out[pp.Path] = append(out[pp.Path], r)
			}
		}
	}
	return out
}
