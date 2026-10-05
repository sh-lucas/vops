package notify

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/sysmon"
)

// Monitor runs the detectors in the daemon: podman's event stream (deaths), a reconciliation of restart counts
// (what the stream missed), probes of routed services, and resource samples (host and containers' cgroups).
type Monitor struct {
	N          *Notifier
	Engine     *deploy.Engine
	Sys        *sysmon.Sampler
	CgroupRoot string // /sys/fs/cgroup

	mu      sync.Mutex
	ctrs    map[string]ctrState // by id, as the last reconciliation saw them
	seen    map[string]int      // died events per container since the last reconciliation
	recGen  uint64              // the engine's generation at the last reconciliation
	cg      map[string]cgroup   // previous cgroup sample per container
	held    sustained
	streak  map[string]int // consecutive failed probes per down:/health: key
	oom     uint64         // host oom_kill counter at the last sample
	oomSeen bool
}

type ctrState struct {
	ID, Name, Project, Service string
	Job                        bool
	Status                     string
	RestartCount               int
	FinishedAt                 string
	ExitCode                   int
	OOMKilled                  bool
	Cgroup                     string
}

// Run starts the detectors and the sweep (repeats, expiry, resolution) until ctx is done.
func (m *Monitor) Run(ctx context.Context) {
	m.ctrs, m.seen, m.cg, m.held, m.streak = map[string]ctrState{}, map[string]int{}, map[string]cgroup{}, sustained{}, map[string]int{}
	if m.CgroupRoot == "" {
		m.CgroupRoot = "/sys/fs/cgroup"
	}
	m.Reconcile(ctx)
	go m.events(ctx)
	go every(ctx, func() time.Duration { return sec(m.N.Settings().ProbeInterval) }, func() { m.probe(ctx) })
	go every(ctx, func() time.Duration { return sec(m.N.Settings().SampleEvery) }, func() { m.resources() })
	go every(ctx, func() time.Duration { return 5 * time.Second }, m.N.Sweep)
}

func every(ctx context.Context, interval func() time.Duration, fn func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval()):
		}
		fn()
	}
}

// ---- deaths: podman events, then a few seconds to let vops' own stops show (marks, removal)

type podmanEvent struct {
	ID         string            `json:"ID"`
	Name       string            `json:"Name"`
	Status     string            `json:"Status"`
	ExitCode   int               `json:"ContainerExitCode"`
	Attributes map[string]string `json:"Attributes"`
}

// settle is how long a death waits before it is judged: vops removes what it stops right away.
var settle = 3 * time.Second

func (m *Monitor) events(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := m.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Printf("podman events: %v (reconnecting in %s; reconciliation catches what was missed)", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (m *Monitor) stream(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, podman.Bin(), "events", "--format", "json", "--filter", "type=container", "--filter", "event=died")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ev podmanEvent
		if json.Unmarshal(sc.Bytes(), &ev, json.RejectUnknownMembers(false)) != nil || ev.Status != "died" || ev.Attributes[deploy.LProject] == "" {
			continue
		}
		m.mu.Lock()
		m.seen[ev.ID]++
		m.mu.Unlock()
		go m.died(ctx, ev)
	}
	err = cmd.Wait()
	if err == nil {
		err = fmt.Errorf("stream ended")
	}
	return err
}

func (m *Monitor) died(ctx context.Context, ev podmanEvent) {
	select {
	case <-ctx.Done():
	case <-time.After(settle):
	}
	d := Death{Project: ev.Attributes[deploy.LProject], Service: ev.Attributes[deploy.LService], Name: ev.Name, ExitCode: ev.ExitCode,
		Job: ev.Attributes[deploy.LJob] != "", Expected: m.Engine.Expected(ev.ID, ev.Name), Stopping: ctx.Err() != nil}
	status := ""
	if cs, err := inspect(context.Background(), ev.ID); err != nil || len(cs) == 0 {
		d.Gone = true
	} else {
		d.OOMKilled, status = cs[0].OOMKilled, cs[0].Status
		if cs[0].RestartCount > 0 {
			status += fmt.Sprintf(", %d restarts", cs[0].RestartCount)
		}
	}
	m.raiseDeath(d, status, "")
}

func (m *Monitor) raiseDeath(d Death, status, note string) {
	target := d.Project + "/" + d.Service
	for _, kind := range classify(d) {
		body := fmt.Sprintf("%s %s; OOMKilled: %t; now %s%s", d.Name, d.cause(), d.OOMKilled, cmpOr(status, "gone"), note)
		o := Obs{Kind: kind, Key: kind + ":" + target, Project: d.Project, Service: d.Service, Body: body, URL: projectURL(d.Project, "services")}
		o.Title = target + " restarted"
		if !strings.HasPrefix(status, "running") {
			o.Title = target + " died"
		}
		if kind == "oom" {
			o.Title = target + ": out of memory"
		}
		m.N.Raise(o)
	}
}

type inspected struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"`
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		FinishedAt string `json:"FinishedAt"`
		CgroupPath string `json:"CgroupPath"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func inspect(ctx context.Context, ids ...string) ([]ctrState, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	raw, err := podman.Run(ctx, append([]string{"inspect", "--type", "container", "--format", "json"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var cs []inspected
	if err := json.Unmarshal([]byte(raw), &cs, json.RejectUnknownMembers(false)); err != nil {
		return nil, err
	}
	var out []ctrState
	for _, c := range cs {
		l := c.Config.Labels
		out = append(out, ctrState{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Project: l[deploy.LProject], Service: l[deploy.LService], Job: l[deploy.LJob] != "",
			Status: c.State.Status, RestartCount: c.RestartCount, FinishedAt: c.State.FinishedAt, ExitCode: c.State.ExitCode, OOMKilled: c.State.OOMKilled, Cgroup: c.State.CgroupPath})
	}
	return out, nil
}

// Reconcile compares restart counts and finish times with the previous pass: deaths the event stream didn't
// report (it was reconnecting, podman restarted) become alerts. Passes during which the engine moved containers
// only take a new baseline. It also refreshes the containers the resource sampler reads. Housekeeping calls it.
func (m *Monitor) Reconcile(ctx context.Context) {
	gen, busy := m.Engine.Busy()
	ps, err := podman.PS(ctx, deploy.LProject)
	if err != nil {
		return
	}
	var ids []string
	for _, c := range ps {
		ids = append(ids, c.ID)
	}
	cs, err := inspect(ctx, ids...)
	if err != nil {
		return
	}
	m.mu.Lock()
	quiet := len(m.ctrs) > 0 && !busy && gen == m.recGen
	var missed []ctrState
	var counts []int
	next := map[string]ctrState{}
	for _, c := range cs {
		next[c.ID] = c
		prev, ok := m.ctrs[c.ID]
		if !ok || !quiet {
			continue
		}
		deaths := max(c.RestartCount-prev.RestartCount, 0)
		if deaths == 0 && c.FinishedAt != prev.FinishedAt && !strings.HasPrefix(c.FinishedAt, "0001") {
			deaths = 1
		}
		if n := deaths - m.seen[c.ID]; n > 0 {
			missed, counts = append(missed, c), append(counts, n)
		}
	}
	m.ctrs, m.seen, m.recGen = next, map[string]int{}, gen
	m.mu.Unlock()
	for i, c := range missed {
		d := Death{Project: c.Project, Service: c.Service, Name: c.Name, ExitCode: c.ExitCode, OOMKilled: c.OOMKilled, Job: c.Job, Expected: m.Engine.Expected(c.ID, c.Name)}
		m.raiseDeath(d, c.Status, fmt.Sprintf(" (%d death(s) found by reconciliation, missed by podman events)", counts[i]))
	}
}

// ---- probes of routed services (and compose healthchecks)

type outcome struct {
	kind   string // "" ok, down, health
	reason string
}

var probeClient = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func probeOne(ctx context.Context, p deploy.Probe) outcome {
	if len(p.Replicas) == 0 {
		if p.Port == 0 {
			return outcome{}
		}
		return outcome{"down", "no running replica"}
	}
	answered, bad := 0, ""
	for _, r := range p.Replicas {
		if p.Port > 0 {
			addr := "127.0.0.1:" + strconv.Itoa(r.HostPort)
			if p.Health != "" {
				resp, err := probeClient.Get("http://" + addr + p.Health)
				if err == nil {
					resp.Body.Close()
					answered++
					if resp.StatusCode >= 400 {
						bad = fmt.Sprintf("GET %s answered %d (%s)", p.Health, resp.StatusCode, r.Name)
					}
				}
			} else if c, err := net.DialTimeout("tcp", addr, 3*time.Second); err == nil {
				c.Close()
				answered++
			}
		}
		if p.Healthcheck {
			if out, err := podman.Run(ctx, "healthcheck", "run", r.ID); err != nil {
				bad = "healthcheck failed (" + r.Name + "): " + trunc(strings.TrimSpace(out+" "+err.Error()), 300)
			}
		}
	}
	switch {
	case p.Port > 0 && answered == 0:
		what := "TCP connect"
		if p.Health != "" {
			what = "GET " + p.Health
		}
		return outcome{"down", fmt.Sprintf("%s: no answer from any of %d replica(s)", what, len(p.Replicas))}
	case bad != "":
		return outcome{"health", bad}
	}
	return outcome{}
}

func (m *Monitor) probe(ctx context.Context) {
	gen, busy := m.Engine.Busy()
	if busy {
		m.N.Keep("down", "health")
		return
	}
	plan, err := m.Engine.Plan(ctx)
	if err != nil {
		return
	}
	probes := plan.Probes()
	results := make([]outcome, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Go(func() { results[i] = probeOne(ctx, p) })
	}
	wg.Wait()
	if g, b := m.Engine.Busy(); b || g != gen { // a deploy moved things meanwhile: this round says nothing
		m.N.Keep("down", "health")
		return
	}
	s := m.N.Settings()
	m.mu.Lock()
	keep := map[string]bool{}
	var raise []Obs
	for i, p := range probes {
		target := p.Project + "/" + p.Service
		for _, kind := range []string{"down", "health"} {
			key := kind + ":" + target
			keep[key] = true
			if results[i].kind != kind {
				delete(m.streak, key)
				continue
			}
			m.streak[key]++
			if int64(m.streak[key]) >= s.DownAfter {
				title := target + " is down"
				if kind == "health" {
					title = target + " is unhealthy"
				}
				raise = append(raise, Obs{Kind: kind, Key: key, Project: p.Project, Service: p.Service, Title: title,
					Body: fmt.Sprintf("%s (%d checks in a row)", results[i].reason, m.streak[key]), URL: projectURL(p.Project, "services")})
			}
		}
	}
	for k := range m.streak {
		if !keep[k] {
			delete(m.streak, k)
		}
	}
	m.mu.Unlock()
	for _, o := range raise {
		m.N.Raise(o)
	}
}

// ---- resources: host (sysmon) and containers (cgroup v2 files)

func (m *Monitor) resources() {
	if m.Sys == nil {
		return
	}
	st := m.Sys.Read()
	s := m.N.Settings()
	now := time.Now()
	m.mu.Lock()
	var raise []Obs
	for _, c := range hostChecks(st, s) {
		if m.held.check(c.key, c.cond, now, c.d) {
			raise = append(raise, Obs{Kind: "resources", Key: c.key, Title: c.title, Body: c.body, URL: "/#/system"})
		}
	}
	if m.oomSeen && st.OOMKills > m.oom {
		raise = append(raise, Obs{Kind: "oom", Key: "oom:host", Title: "the kernel OOM killer ran",
			Body: fmt.Sprintf("%d process(es) killed for lack of memory since the last check (%d since boot): journalctl -k | grep -i oom", st.OOMKills-m.oom, st.OOMKills), URL: "/#/system"})
	}
	m.oom, m.oomSeen = st.OOMKills, true
	type use struct{ mem, cpu float64 }
	per := map[[2]string]use{}
	cur := map[string]cgroup{}
	for id, c := range m.ctrs {
		if c.Status != "running" || c.Job || c.Cgroup == "" || c.Project == "" || strings.Contains(c.Project, "@") {
			continue
		}
		g, ok := readCgroup(filepath.Join(m.CgroupRoot, c.Cgroup), now)
		if !ok {
			continue
		}
		cur[id] = g
		target := c.Project + "/" + c.Service
		prev, had := m.cg[id]
		if had && g.oomKills > prev.oomKills {
			raise = append(raise, Obs{Kind: "oom", Key: "oom:" + target, Project: c.Project, Service: c.Service, Title: target + ": out of memory",
				Body: fmt.Sprintf("the OOM killer killed %d process(es) in %s (memory limit %s)", g.oomKills-prev.oomKills, c.Name, size(g.memMax)), URL: projectURL(c.Project, "services")})
		}
		mem, cpu := ctrUsage(prev, g, st.CPUs)
		svc := [2]string{c.Project, c.Service}
		u, seen := per[svc]
		if !seen {
			u.mem = -1
		}
		per[svc] = use{max(u.mem, mem), max(u.cpu, cpu)}
	}
	m.cg = cur
	for svc, u := range per {
		project, service := svc[0], svc[1]
		target := project + "/" + service
		for _, k := range []struct {
			key, title string
			cond       bool
			d          time.Duration
		}{
			{"resources:mem:" + target, fmt.Sprintf("%s at %.0f%% of its memory limit", target, u.mem), u.mem >= 0 && u.mem > float64(s.CtrMem), sec(s.CtrMemFor)},
			{"resources:cpu:" + target, fmt.Sprintf("%s at %.0f%% cpu", target, u.cpu), u.cpu > float64(s.CtrCPU), sec(s.CtrCPUFor)},
		} {
			if m.held.check(k.key, k.cond, now, k.d) {
				raise = append(raise, Obs{Kind: "resources", Key: k.key, Project: project, Service: service, Title: k.title,
					Body: "sustained above the threshold (Notifications → Advanced)", URL: projectURL(project, "services")})
			}
		}
	}
	m.mu.Unlock()
	for _, o := range raise {
		m.N.Raise(o)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
