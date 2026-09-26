// Package deploy compares git (desired) with podman (actual) and makes podman match, one service at a time.
package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/store"
)

// Labels vops puts on everything it creates.
const (
	LProject  = "vops.project"
	LService  = "vops.service"
	LHash     = "vops.hash"
	LHostPort = "vops.hostport"
	LDomains  = "vops.domains"
	LImage    = "vops.image"
)

type Engine struct {
	Repo         string // the git working tree (~/vops)
	DB           *store.DB
	Routes       *proxy.Table
	Registry     *registry.Registry // may be nil
	PullAddr     string             // loopback host:port serving the registry, for pulling our own images
	PullAuthFile string             // podman authfile (0600) with credentials for PullAddr; not --creds, which shows in ps

	mu sync.Mutex // one apply at a time
}

type Plan struct {
	Commit   string         `json:"commit"`
	Domain   string         `json:"domain"`
	Projects []*ProjectPlan `json:"projects"`
	Warnings []string       `json:"warnings"`
}

type ProjectPlan struct {
	Path     string   `json:"path"`
	Disabled bool     `json:"disabled,omitempty"`
	Gone     bool     `json:"gone,omitempty"` // directory removed from git
	Error    string   `json:"error,omitempty"`
	Actions  []Action `json:"actions"`

	specs  map[string]*desired
	actual map[string][]podman.Container
}

type Action struct {
	Service string `json:"service"`
	Kind    string `json:"kind"` // create | update | start | remove | none
	Reason  string `json:"reason,omitempty"`
}

type desired struct {
	spec  *compose.Spec
	hash  string
	image string // final image ref to run
	pull  string // ref to pull from our registry (loopback), when the image is ours
	own   string // "repo:tag" in our registry, when the image is ours
}

// Changes reports whether the plan does anything.
func (p *Plan) Changes() bool {
	for _, pp := range p.Projects {
		if pp.Error != "" {
			return true
		}
		for _, a := range pp.Actions {
			if a.Kind != "none" {
				return true
			}
		}
	}
	return false
}

// Print writes a human readable plan.
func (p *Plan) Print(w io.Writer) {
	short := p.Commit
	if len(short) > 12 {
		short = short[:12]
	}
	fmt.Fprintf(w, "commit %s\n", short)
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "  ! %s\n", warn)
	}
	for _, pp := range p.Projects {
		label := pp.Path
		switch {
		case pp.Gone:
			label += " (removed from git)"
		case pp.Disabled:
			label += " (disabled)"
		}
		fmt.Fprintf(w, "%s\n", label)
		if pp.Error != "" {
			fmt.Fprintf(w, "  ✗ %s\n", pp.Error)
		}
		for _, a := range pp.Actions {
			sym := map[string]string{"create": "+", "update": "~", "start": ">", "remove": "-", "none": "="}[a.Kind]
			line := fmt.Sprintf("  %s %s", sym, a.Service)
			if a.Reason != "" {
				line += "  (" + a.Reason + ")"
			}
			fmt.Fprintln(w, line)
		}
	}
	if !p.Changes() {
		fmt.Fprintln(w, "nothing to do")
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// DesiredProjects lists project dirs (repo-relative) and their compose files, from git-tracked files.
func DesiredProjects(ctx context.Context, repo string) (map[string][]string, error) {
	out, err := git(ctx, repo, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	projects := map[string][]string{}
	for f := range strings.SplitSeq(out, "\x00") {
		dir, name := path.Split(f)
		dir = strings.TrimSuffix(dir, "/")
		if dir == "" || !compose.IsComposeFile(name) || strings.HasPrefix(dir, ".") || strings.Contains(dir, "/.") || dir == "registry" || strings.HasPrefix(dir, "registry/") {
			continue
		}
		projects[dir] = append(projects[dir], name)
	}
	for _, files := range projects {
		slices.Sort(files)
	}
	return projects, nil
}

// Plan computes what Apply would do.
func (e *Engine) Plan(ctx context.Context) (*Plan, error) {
	plan := &Plan{Warnings: []string{}}
	plan.Commit, _ = git(ctx, e.Repo, "rev-parse", "HEAD")
	cfg, err := config.ReadRepo(e.Repo)
	if err != nil {
		return nil, err
	}
	plan.Domain = cfg.Domain
	var projects map[string][]string
	if plan.Commit != "" {
		if projects, err = DesiredProjects(ctx, e.Repo); err != nil {
			return nil, err
		}
	}
	flags, err := e.DB.Projects()
	if err != nil {
		return nil, err
	}
	containers, err := podman.PS(ctx, LProject)
	if err != nil {
		return nil, err
	}
	actual := map[string]map[string][]podman.Container{}
	for _, c := range containers {
		p, s := c.Labels[LProject], c.Labels[LService]
		if actual[p] == nil {
			actual[p] = map[string][]podman.Container{}
		}
		actual[p][s] = append(actual[p][s], c)
	}

	paths := slices.Collect(func(yield func(string) bool) {
		for p := range projects {
			yield(p)
		}
		for p := range actual {
			if _, ok := projects[p]; !ok {
				yield(p)
			}
		}
	})
	slices.Sort(paths)
	domains := map[string]string{}
	for _, p := range paths {
		pp := &ProjectPlan{Path: p, Actions: []Action{}, specs: map[string]*desired{}, actual: actual[p]}
		plan.Projects = append(plan.Projects, pp)
		files, inGit := projects[p]
		pp.Gone = !inGit
		pp.Disabled = flags[p].Disabled
		if pp.Gone || pp.Disabled {
			reason := "project removed from git"
			if pp.Disabled && !pp.Gone {
				reason = "project disabled"
			}
			for _, s := range sortedKeys(pp.actual) {
				pp.Actions = append(pp.Actions, Action{s, "remove", reason})
			}
			continue
		}
		if err := e.planProject(ctx, pp, files, cfg.Domain, &plan.Warnings); err != nil {
			pp.Error = err.Error()
			continue
		}
		for _, name := range sortedKeys(pp.specs) {
			for _, d := range pp.specs[name].spec.Domains {
				if other, taken := domains[d]; taken {
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("domain %s is claimed by %s and %s/%s; the first one wins", d, other, p, name))
					continue
				}
				domains[d] = p + "/" + name
			}
		}
	}
	return plan, nil
}

func (e *Engine) planProject(ctx context.Context, pp *ProjectPlan, files []string, domain string, warns *[]string) error {
	env, err := e.DB.Env(pp.Path)
	if err != nil {
		return err
	}
	for k, v := range builtinEnv(pp.Path, domain) {
		if _, set := env[k]; !set {
			env[k] = v
		}
	}
	proj, err := compose.Load(filepath.Join(e.Repo, filepath.FromSlash(pp.Path)), pp.Path, files, env)
	if err != nil {
		return err
	}
	*warns = append(*warns, proj.Warnings...)
	specs, err := proj.Specs(domain, env)
	if err != nil {
		return err
	}
	var order []Action
	for _, sp := range specs {
		d, err := e.resolve(ctx, sp, domain)
		if err != nil {
			return fmt.Errorf("service %s: %w", sp.Service, err)
		}
		pp.specs[sp.Service] = d
		order = append(order, compare(d, pp.actual[sp.Service]))
	}
	for _, s := range sortedKeys(pp.actual) {
		if _, ok := pp.specs[s]; !ok {
			order = append(order, Action{s, "remove", "service removed from compose"})
		}
	}
	pp.Actions = order
	return nil
}

// builtinEnv is available for interpolation in every project.
func builtinEnv(project, domain string) map[string]string {
	env := map[string]string{"VOPS_PROJECT": project, "VOPS_DOMAIN": domain}
	if domain != "" {
		env["VOPS_PROJECT_DOMAIN"] = compose.DNSName(project) + "." + domain
	}
	return env
}

func compare(d *desired, actual []podman.Container) Action {
	a := Action{Service: d.spec.Service}
	if len(actual) == 0 {
		a.Kind = "create"
		return a
	}
	running := 0
	for _, c := range actual {
		if c.Labels[LHash] != d.hash {
			a.Kind, a.Reason = "update", "definition changed"
			return a
		}
		if c.State == "running" {
			running++
		}
	}
	switch {
	case len(actual) != d.spec.Replicas:
		a.Kind, a.Reason = "update", fmt.Sprintf("%d → %d replicas", len(actual), d.spec.Replicas)
	case running < len(actual):
		a.Kind, a.Reason = "start", fmt.Sprintf("%d of %d stopped", len(actual)-running, len(actual))
	default:
		a.Kind = "none"
	}
	return a
}

// resolve fixes the image of a spec and computes its hash.
func (e *Engine) resolve(ctx context.Context, sp *compose.Spec, domain string) (*desired, error) {
	d := &desired{spec: sp, image: sp.Image}
	h := sha256.New()
	w := func(s ...string) {
		for _, x := range s {
			h.Write([]byte(x))
			h.Write([]byte{0})
		}
	}
	w("v1")
	w(sp.Args...)
	w("--cmd")
	w(sp.Cmd...)
	w("--net", strconv.FormatBool(sp.Networks), strconv.Itoa(sp.Port), strconv.Itoa(sp.Replicas))
	w(sp.Domains...)
	switch {
	case sp.Build != nil:
		tree, err := treeHash(ctx, e.Repo, sp.Build.Context)
		if err != nil {
			return nil, fmt.Errorf("build context: %w", err)
		}
		bh := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%v", tree, sp.Build.Dockerfile, sp.Build.Target, sp.Build.Args))
		d.image = "localhost/vops/" + compose.Slug(sp.Project) + "-" + strings.ToLower(sp.Service) + ":" + hex.EncodeToString(bh[:6])
		w("build", d.image)
	default:
		if repo, ref, ok := e.ownImage(sp.Image, domain); ok {
			digest := ref
			if !strings.HasPrefix(ref, "sha256:") {
				digest = e.Registry.Resolve(repo, ref)
				if digest == "" {
					return nil, fmt.Errorf("image %s is not in the vops registry yet (push it first)", sp.Image)
				}
			}
			d.pull = e.PullAddr + "/" + repo + "@" + digest
			d.own = repo + ":" + ref
			w("own", digest)
		}
		w("image", sp.Image)
	}
	d.hash = hex.EncodeToString(h.Sum(nil))[:16]
	return d, nil
}

// ownImage reports whether image lives in our registry, returning repo and tag (or digest).
func (e *Engine) ownImage(image, domain string) (repo, ref string, ok bool) {
	if e.Registry == nil {
		return "", "", false
	}
	host, rest, found := strings.Cut(image, "/")
	if !found {
		return "", "", false
	}
	own := host == e.PullAddr || domain != "" && host == "registry."+domain
	if _, port, err := splitHostPort(e.PullAddr); err == nil && (host == "localhost:"+port || host == "127.0.0.1:"+port) {
		own = true
	}
	if !own {
		return "", "", false
	}
	if r, digest, found := strings.Cut(rest, "@"); found {
		return r, digest, true
	}
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		return rest[:i], rest[i+1:], true
	}
	return rest, "latest", true
}

func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("no port in %q", s)
	}
	return s[:i], s[i+1:], nil
}

// treeHash identifies a build context: the git tree hash when it is inside the repo, else a hash of the files.
func treeHash(ctx context.Context, repo, dir string) (string, error) {
	if rel, err := filepath.Rel(repo, dir); err == nil && !strings.HasPrefix(rel, "..") {
		spec := "HEAD:" + filepath.ToSlash(rel)
		if rel == "." {
			spec = "HEAD^{tree}"
		}
		if h, err := git(ctx, repo, "rev-parse", spec); err == nil {
			return h, nil
		}
	}
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() && de.Name() == ".git" {
			return filepath.SkipDir
		}
		if de.Type().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			rel, _ := filepath.Rel(dir, p)
			fmt.Fprintf(h, "%s\x00", rel)
			io.Copy(h, f)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Watching lists services that run repo:tag from our registry and want to be redeployed when it is pushed.
func (p *Plan) Watching(repo, tag string) []proxy.Key {
	var out []proxy.Key
	for _, pp := range p.Projects {
		if pp.Disabled || pp.Gone || pp.Error != "" {
			continue
		}
		for name, d := range pp.specs {
			if d.own == repo+":"+tag && d.spec.Watch {
				out = append(out, proxy.Key{Project: pp.Path, Service: name})
			}
		}
	}
	return out
}

// ServiceState is a service as the ui shows it: what git wants, what podman runs, what apply would do.
type ServiceState struct {
	Name       string             `json:"name"`
	Image      string             `json:"image"`
	Domains    []string           `json:"domains"`
	Port       int                `json:"port,omitempty"`
	Pending    string             `json:"pending,omitempty"` // create | update | start | remove
	Reason     string             `json:"reason,omitempty"`
	Containers []podman.Container `json:"containers"`
}

func (pp *ProjectPlan) Services() []ServiceState {
	names := map[string]bool{}
	for n := range pp.specs {
		names[n] = true
	}
	for n := range pp.actual {
		names[n] = true
	}
	out := []ServiceState{}
	for _, n := range sortedKeys(names) {
		st := ServiceState{Name: n, Domains: []string{}, Containers: pp.actual[n]}
		if st.Containers == nil {
			st.Containers = []podman.Container{}
		}
		if d := pp.specs[n]; d != nil {
			st.Image, st.Domains, st.Port = d.image, d.spec.Domains, d.spec.Port
			if st.Image == "" {
				st.Image = d.spec.Image
			}
			if st.Domains == nil {
				st.Domains = []string{}
			}
		} else if len(st.Containers) > 0 {
			st.Image = st.Containers[0].Image
		}
		for _, a := range pp.Actions {
			if a.Service == n && a.Kind != "none" {
				st.Pending, st.Reason = a.Kind, a.Reason
			}
		}
		out = append(out, st)
	}
	return out
}
