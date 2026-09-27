package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/store"
)

// Drain is how long old replicas keep serving in-flight requests after traffic moved away.
var Drain = 2 * time.Second

type ApplyOpts struct {
	Commit   string      // refuse if HEAD is not this commit (what the user confirmed)
	Projects []string    // only these projects (all if empty)
	Services []proxy.Key // only these services (all if empty); used by registry push triggers
	Trigger  string      // for the deploy history: sync | apply | ui | push (default apply)
}

// Triggers are what can start a deploy, as the history records them.
var Triggers = []string{"sync", "apply", "ui", "push", "rollback"}

func (o ApplyOpts) wantsProject(project string) bool {
	if len(o.Projects) > 0 && !slices.Contains(o.Projects, project) {
		return false
	}
	return len(o.Services) == 0 || slices.ContainsFunc(o.Services, func(k proxy.Key) bool { return k.Project == project })
}

func (o ApplyOpts) wants(project, service string) bool {
	if len(o.Projects) > 0 && !slices.Contains(o.Projects, project) {
		return false
	}
	if len(o.Services) > 0 && !slices.Contains(o.Services, proxy.Key{Project: project, Service: service}) {
		return false
	}
	return true
}

// Apply makes podman match git. Progress goes to w. Projects fail independently.
func (e *Engine) Apply(ctx context.Context, w io.Writer, opts ApplyOpts) (*Plan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w = &syncWriter{w: w}
	plan, err := e.Plan(ctx)
	if err != nil {
		return nil, err
	}
	if opts.Commit != "" && plan.Commit != opts.Commit {
		return plan, fmt.Errorf("the host is at %.12s, not %.12s: someone pushed in between, run sync again", plan.Commit, opts.Commit)
	}
	var failed []string
	for _, pp := range plan.Projects {
		if err := e.applyProject(ctx, w, plan, pp, opts); err != nil {
			fmt.Fprintf(w, "%s: ✗ %v\n", pp.Path, err)
			e.DB.Event(pp.Path, "error", "%v", err)
			failed = append(failed, pp.Path)
		}
	}
	if plan.Config != nil && len(opts.Projects) == 0 && len(opts.Services) == 0 && e.ApplyConfig != nil {
		fmt.Fprintf(w, "vops.yml: %s\n", strings.Join(plan.Config.Changes, ", "))
		if err := e.ApplyConfig(plan.Config.To, w); err != nil {
			fmt.Fprintf(w, "vops.yml: ✗ %v\n", err)
			failed = append(failed, "vops.yml")
		}
	}
	if err := e.RefreshRoutes(ctx); err != nil {
		fmt.Fprintf(w, "routes: %v\n", err)
	}
	if len(failed) > 0 {
		return plan, fmt.Errorf("failed: %s", strings.Join(failed, ", "))
	}
	return plan, nil
}

// applyProject applies one project and records it in the deploy history when it changed something
// (previews have their own row in the previews table instead).
func (e *Engine) applyProject(ctx context.Context, w io.Writer, plan *Plan, pp *ProjectPlan, opts ApplyOpts) error {
	if !opts.wantsProject(pp.Path) {
		return nil
	}
	rec := store.Deploy{Project: pp.Path, Commit: plan.Commit, Trigger: cmpOr(opts.Trigger, "apply"), StartedAt: time.Now().Unix()}
	err := e.deployProject(ctx, w, plan, pp, opts, &rec)
	if compose.IsPreview(pp.Path) || pp.Error == "" && !slices.ContainsFunc(pp.Actions, func(a Action) bool { return a.Kind != "none" && opts.wants(pp.Path, a.Service) }) {
		return err
	}
	e.record(ctx, rec, pp.specs, err)
	return err
}

func (e *Engine) deployProject(ctx context.Context, w io.Writer, plan *Plan, pp *ProjectPlan, opts ApplyOpts, rec *store.Deploy) error {
	if pp.Error != "" {
		return errors.New(pp.Error)
	}
	var errs []error
	if len(pp.specs) > 0 && !pp.Gone && !pp.Disabled {
		if err := e.ensureNetworks(ctx, w, pp, len(opts.Services) == 0); err != nil {
			return err
		}
	}
	// previews are disposable copies: no pre-deploy snapshots of them
	if !compose.IsPreview(pp.Path) && slices.ContainsFunc(pp.Actions, func(a Action) bool {
		return (a.Kind == "create" || a.Kind == "update") && opts.wants(pp.Path, a.Service)
	}) {
		id, err := e.preDeploy(ctx, w, pp, plan.Commit)
		if err != nil {
			return err
		}
		rec.SnapshotID = id
	}
	var done []string // summary for the history: "web updated", "db failed"
	defer func() { rec.Summary = strings.Join(done, ", ") }()
	failed := map[string]bool{}
	for _, a := range pp.Actions {
		if a.Kind == "none" || !opts.wants(pp.Path, a.Service) {
			continue
		}
		log := func(format string, args ...any) {
			fmt.Fprintf(w, "%s/%s: %s\n", pp.Path, a.Service, fmt.Sprintf(format, args...))
		}
		// actions come in dependency order: never deploy on top of a dependency that just failed
		if d := pp.specs[a.Service]; d != nil {
			if i := slices.IndexFunc(d.spec.DependsOn, func(dep string) bool { return failed[dep] }); i >= 0 {
				failed[a.Service] = true
				done = append(done, a.Service+" skipped")
				log("skipped: %s failed", d.spec.DependsOn[i])
				errs = append(errs, fmt.Errorf("%s: skipped, dependency %s failed", a.Service, d.spec.DependsOn[i]))
				continue
			}
		}
		var err error
		switch a.Kind {
		case "remove":
			log("removing (%s)", a.Reason)
			e.Routes.Set(proxy.Key{Project: pp.Path, Service: a.Service}, nil, nil)
			err = e.remove(ctx, pp.actual[a.Service])
		case "start":
			log("starting stopped containers")
			for _, c := range pp.actual[a.Service] {
				if c.State != "running" {
					if _, serr := podman.Run(ctx, "start", c.ID); serr != nil {
						err = serr
					}
				}
			}
		case "create", "update":
			err = e.deploy(ctx, log, pp.specs[a.Service], pp.actual[a.Service])
		}
		if err != nil {
			failed[a.Service] = true
			done = append(done, a.Service+" failed")
			log("✗ %v", err)
			errs = append(errs, fmt.Errorf("%s: %w", a.Service, err))
			continue
		}
		done = append(done, a.Service+" "+pastTense[a.Kind])
		e.DB.Event(pp.Path, a.Kind, "%s: %s", a.Service, strings.TrimSpace(a.Kind+" "+a.Reason))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if len(opts.Services) > 0 {
		return nil // a partial apply doesn't mean the project matches the commit
	}
	// networks this project created and no longer uses (all of them when the project is gone or disabled)
	var keep []string
	if !pp.Gone && !pp.Disabled {
		for _, n := range pp.nets {
			keep = append(keep, n.Name)
		}
	}
	if names, err := podman.Run(ctx, "network", "ls", "--format", "{{.Name}}", "--filter", "label="+LProject+"="+pp.Path); err == nil {
		for _, n := range strings.Fields(names) {
			if !slices.Contains(keep, n) {
				podman.Run(ctx, "network", "rm", n)
			}
		}
	}
	switch {
	case compose.IsPreview(pp.Path):
		return nil // the previews table has its commit
	case pp.Gone:
		e.DB.DeleteProject(pp.Path)
		return nil
	}
	return e.DB.SetApplied(pp.Path, plan.Commit)
}

// ensureNetworks creates the shared network and the project's networks. A network whose definition
// changed is recreated (its containers of this project are removed first; the deploys that follow bring them back).
func (e *Engine) ensureNetworks(ctx context.Context, w io.Writer, pp *ProjectPlan, recreate bool) error {
	if _, err := podman.Run(ctx, "network", "exists", compose.SharedNetwork); err != nil {
		if _, err := podman.Run(ctx, "network", "create", "--label", "vops.shared=1", compose.SharedNetwork); err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}
	for _, n := range pp.nets {
		old, exists := networkHash(ctx, n.Name)
		if n.External {
			if !exists {
				return fmt.Errorf("external network %s does not exist (podman network create %s)", n.Name, n.Name)
			}
			continue
		}
		if exists && (old == n.Hash() || old == "" && n.Plain()) {
			continue
		}
		if exists {
			if !recreate {
				continue
			}
			fmt.Fprintf(w, "%s: network %s changed, recreating it\n", pp.Path, n.Name)
			out, _ := podman.Run(ctx, "ps", "-aq", "--filter", "network="+n.Name, "--filter", "label="+LProject+"="+pp.Path)
			for _, id := range strings.Fields(out) {
				podman.Run(ctx, "rm", "-f", "-t", "10", id)
			}
			if _, err := podman.Run(ctx, "network", "rm", n.Name); err != nil {
				return fmt.Errorf("network %s changed but can't be removed (other containers use it?): %w", n.Name, err)
			}
		}
		args := append([]string{"network", "create", "--label", LProject + "=" + pp.Path, "--label", LNetHash + "=" + n.Hash()}, n.CreateArgs()...)
		if _, err := podman.Run(ctx, append(args, n.Name)...); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) remove(ctx context.Context, cs []podman.Container) error {
	var errs []error
	for _, c := range cs {
		wait := "10"
		if c.Labels["vops.stopwait"] != "" {
			wait = c.Labels["vops.stopwait"]
		}
		// -v: anonymous volumes (an image's VOLUME) belong to the container; named ones are kept
		if _, err := podman.Run(ctx, "rm", "-f", "-v", "-t", wait, c.ID); err != nil {
			// podman sometimes removes the container and then fails cleaning up its network: gone is gone
			if _, exists := podman.Run(ctx, "container", "exists", c.ID); exists != nil {
				continue
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// prepare builds or pulls the image so starting replicas is fast.
func (e *Engine) prepare(ctx context.Context, log func(string, ...any), d *desired) error {
	sp := d.spec
	switch {
	case sp.Build != nil:
		if podman.ImageID(ctx, d.image) != "" {
			return nil
		}
		log("building %s", d.image)
		args := []string{"build", "-q", "-t", d.image}
		if sp.Build.Dockerfile != "" {
			args = append(args, "-f", sp.Build.Dockerfile)
		}
		if sp.Build.Target != "" {
			args = append(args, "--target", sp.Build.Target)
		}
		for _, a := range sp.Build.Args {
			if !a.Unset {
				args = append(args, "--build-arg", a.Key+"="+a.Value)
			}
		}
		_, err := podman.Run(ctx, append(args, sp.Build.Context)...)
		return err
	case d.pull != "":
		log("pulling %s from the vops registry", sp.Image)
		if _, err := podman.Run(ctx, "pull", "-q", "--tls-verify=false", "--authfile", e.PullAuthFile, d.pull); err != nil {
			return err
		}
		if d.image == d.pull {
			return nil
		}
		_, err := podman.Run(ctx, "tag", d.pull, sp.Image)
		return err
	default:
		if podman.ImageID(ctx, d.image) != "" {
			return nil
		}
		log("pulling %s", d.image)
		_, err := podman.Run(ctx, "pull", "-q", d.image)
		return err
	}
}

type replica struct {
	id, name string
	hostPort int
	started  time.Time
}

func (e *Engine) deploy(ctx context.Context, log func(string, ...any), d *desired, old []podman.Container) error {
	sp := d.spec
	if err := e.prepare(ctx, log, d); err != nil {
		return err
	}
	if err := e.ensureData(ctx, log, sp); err != nil {
		return err
	}
	key := proxy.Key{Project: sp.Project, Service: sp.Service}
	rolling := sp.Strategy == "rolling" && len(old) > 0
	if !rolling && len(old) > 0 {
		log("stopping %d old replica(s)", len(old))
		e.Routes.Set(key, nil, nil)
		if err := e.remove(ctx, old); err != nil {
			return err
		}
	}
	var news []replica
	fail := func(err error) error {
		for _, r := range news {
			if tail, _ := podman.Run(ctx, "logs", "--tail", "15", r.id); tail != "" {
				log("last logs of %s:\n%s", r.name, indent(tail))
			}
			if rolling {
				podman.Run(ctx, "rm", "-f", "-t", "0", r.id)
			}
		}
		if rolling {
			return fmt.Errorf("%w (old replicas kept serving)", err)
		}
		return err
	}
	for range sp.Replicas {
		r, err := e.start(ctx, d)
		if r.id != "" {
			news = append(news, r)
		}
		if err != nil {
			return fail(err)
		}
		log("started %s, waiting until ready", r.name)
	}
	// wait for all replicas in parallel
	var wg sync.WaitGroup
	errs := make([]error, len(news))
	for i, r := range news {
		wg.Go(func() { errs[i] = e.waitReady(ctx, sp, r) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return fail(err)
	}
	if sp.Port > 0 {
		var backends []string
		for _, r := range news {
			backends = append(backends, "127.0.0.1:"+strconv.Itoa(r.hostPort))
		}
		e.Routes.Set(key, sp.Domains, backends)
		log("ready, serving %s", strings.Join(sp.Domains, ", "))
	} else {
		log("ready")
	}
	if rolling {
		if sp.Port > 0 {
			time.Sleep(Drain)
		}
		if err := e.remove(ctx, old); err != nil {
			return err
		}
		log("removed %d old replica(s)", len(old))
	}
	return nil
}

func indent(s string) string { return "    " + strings.ReplaceAll(s, "\n", "\n    ") }

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var portMu sync.Mutex

// freePort asks the kernel for a port and skips ones owned by any vops container (even stopped ones).
func freePort(ctx context.Context) (int, error) {
	portMu.Lock()
	defer portMu.Unlock()
	used := map[int]bool{}
	cs, err := podman.PS(ctx, LHostPort)
	if err != nil {
		return 0, err
	}
	for _, c := range cs {
		p, _ := strconv.Atoi(c.Labels[LHostPort])
		used[p] = true
	}
	for range 50 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if !used[p] {
			return p, nil
		}
	}
	return 0, errors.New("no free port")
}

var logDriver struct {
	once sync.Once
	name string
}

func (e *Engine) start(ctx context.Context, d *desired) (replica, error) {
	sp := d.spec
	r := replica{name: fmt.Sprintf("vops-%s-%s-%s", compose.Slug(sp.Project), strings.ToLower(sp.Service), randHex(3))}
	args := []string{"run", "-d", "--name", r.name,
		"--label", LProject + "=" + sp.Project,
		"--label", LService + "=" + sp.Service,
		"--label", LHash + "=" + d.hash,
		"--label", LImage + "=" + d.image,
		"--label", "vops.stopwait=" + strconv.Itoa(int(sp.StopWait.Seconds())),
	}
	if len(sp.Domains) > 0 {
		args = append(args, "--label", LDomains+"="+strings.Join(sp.Domains, ","))
	}
	if sp.Job {
		args = append(args, "--label", LJob+"=1")
	}
	for _, n := range sp.Networks {
		args = append(args, "--network", n.Flag())
	}
	if sp.Port > 0 {
		p, err := freePort(ctx)
		if err != nil {
			return r, err
		}
		r.hostPort = p
		args = append(args, "--label", LHostPort+"="+strconv.Itoa(p), "-p", fmt.Sprintf("127.0.0.1:%d:%d", p, sp.Port))
	}
	// journald keeps history across replicas; some distros default podman to k8s-file
	logDriver.once.Do(func() {
		if _, err := os.Stat("/run/systemd/journal/socket"); err == nil {
			logDriver.name = "journald"
		}
	})
	if logDriver.name == "journald" {
		args = append(args, "--log-driver", "journald", "--log-opt", "tag=vops."+compose.Slug(sp.Project)+"."+sp.Service)
	}
	args = append(args, sp.Args...)
	args = append(args, d.image)
	args = append(args, sp.Cmd...)
	r.started = time.Now()
	id, err := podman.Run(ctx, args...)
	if err != nil {
		// podman may have created the container before failing to start it
		podman.Run(ctx, "rm", "-f", "-t", "0", r.name)
		return replica{}, err
	}
	r.id = id
	return r, nil
}

// waitReady: compose healthcheck > x-vops.health > any http answer on the port > still running after 2s.
func (e *Engine) waitReady(ctx context.Context, sp *compose.Spec, r replica) error {
	deadline := time.Now().Add(sp.Timeout)
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var last string
	for {
		state, err := podman.Run(ctx, "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", r.id)
		if err != nil {
			return err
		}
		status, code, _ := strings.Cut(state, " ")
		if sp.Job && (status == "exited" || status == "stopped") {
			if code == "0" {
				return nil
			}
			return fmt.Errorf("%s failed (exit %s)", r.name, code)
		}
		if status != "running" && !(sp.Job && (status == "created" || status == "initialized")) {
			return fmt.Errorf("%s exited (code %s) before becoming ready", r.name, code)
		}
		switch {
		case sp.Job:
			last = "still running"
		case sp.Healthcheck:
			_, err := podman.Run(ctx, "healthcheck", "run", r.id)
			if err == nil {
				return nil
			}
			last = "healthcheck: " + err.Error()
		case sp.Port > 0:
			url := fmt.Sprintf("http://127.0.0.1:%d%s", r.hostPort, cmpOr(sp.Health, "/"))
			resp, err := client.Get(url)
			if err == nil {
				resp.Body.Close()
				ok := resp.StatusCode < 400
				if sp.Health == "" {
					ok = resp.StatusCode < 502 || resp.StatusCode > 504
				}
				if ok {
					return nil
				}
				last = fmt.Sprintf("GET %s: %d", url, resp.StatusCode)
			} else {
				last = "GET " + url + ": no answer"
			}
		default:
			if time.Since(r.started) >= 2*time.Second {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s: %s", r.name, sp.Timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// RefreshRoutes rebuilds the routing table from running containers.
func (e *Engine) RefreshRoutes(ctx context.Context) error {
	cs, err := podman.PS(ctx, LProject)
	if err != nil {
		return err
	}
	flags, _ := e.DB.Projects()
	all := map[proxy.Key][2][]string{}
	for _, c := range cs {
		p := c.Labels[LProject]
		if c.State != "running" || c.Labels[LHostPort] == "" || c.Labels[LDomains] == "" || flags[p].Disabled {
			continue
		}
		k := proxy.Key{Project: p, Service: c.Labels[LService]}
		v := all[k]
		v[0] = strings.Split(c.Labels[LDomains], ",")
		v[1] = append(v[1], "127.0.0.1:"+c.Labels[LHostPort])
		all[k] = v
	}
	e.Routes.Replace(all)
	return nil
}

// StartStopped starts vops containers of enabled projects that are not running (after a reboot).
func (e *Engine) StartStopped(ctx context.Context, w io.Writer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cs, err := podman.PS(ctx, LProject)
	if err != nil {
		fmt.Fprintln(w, err)
		return
	}
	flags, _ := e.DB.Projects()
	for _, c := range cs {
		if c.State == "running" || flags[c.Labels[LProject]].Disabled || c.Labels[LJob] != "" {
			continue
		}
		fmt.Fprintf(w, "starting %s\n", c.Name())
		if _, err := podman.Run(ctx, "start", c.ID); err != nil {
			fmt.Fprintln(w, err)
		}
	}
	e.RefreshRoutes(ctx)
}

// Restart restarts the containers of a project (or one service of it).
func (e *Engine) Restart(ctx context.Context, project, service string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	filters := []string{LProject + "=" + project}
	if service != "" {
		filters = append(filters, LService+"="+service)
	}
	cs, err := podman.PS(ctx, filters...)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		return fmt.Errorf("no containers for %s %s", project, service)
	}
	for _, c := range cs {
		if _, err := podman.Run(ctx, "restart", c.ID); err != nil {
			return err
		}
	}
	e.DB.Event(project, "restart", "restarted %s", strings.TrimSpace(service+" "))
	return e.RefreshRoutes(ctx)
}

// Lock runs fn while no apply is running.
func (e *Engine) Lock(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn()
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.w.Write(p)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}
