// Package deploy compares git (desired) with podman (actual) and makes podman match, one service at a time.
package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
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
	"sync/atomic"
	"time"

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
	LJob      = "vops.job"
	LNetHash  = "vops.nethash"
)

type Engine struct {
	Repo         string // the git working tree (~/vops)
	DB           *store.DB
	Routes       *proxy.Table
	Registry     *registry.Registry // may be nil
	PullAddr     string             // loopback host:port serving the registry, for pulling our own images
	SnapshotDir  string             // where snapshots live (same btrfs as the data); empty disables snapshots
	SnapshotKeep int                // automatic snapshots kept per project (default 5)
	SnapshotsOff bool
	PullAuthFile string
	PreviewDir   string        // previews' git worktrees (~/.vops/previews), same btrfs as the data
	PreviewMax   int           // previews at once (default 5)
	PreviewTTL   time.Duration // previews not updated for this long are removed (default 3 days)

	// Config returns the config in effect (the host lock); ApplyConfig makes a new one take effect.
	// Both nil means vops.yml is only read for the domain (tests).
	Config      func() config.Config
	ApplyConfig func(config.Config, io.Writer) error // podman authfile (0600) with credentials for PullAddr; not --creds, which shows in ps

	// OnDeploy hears every deploy that changed something (err nil = ok) and every failed rollback or preview (notifications)
	OnDeploy func(project string, err error)

	mu       sync.Mutex    // one apply at a time
	busy     atomic.Bool   // an apply, rollback, restart, preview or snapshot holds mu
	gen      atomic.Uint64 // bumped when such an operation starts and ends
	expected sync.Map      // container id or name -> time.Time: the engine stops it, so its death is no alert
}

type Plan struct {
	Version  string         `json:"version,omitempty"` // the host's vops (the cli checks it: hosts before 1.3 don't check themselves)
	Commit   string         `json:"commit"`
	Domain   string         `json:"domain"`
	Projects []*ProjectPlan `json:"projects"`
	Config   *ConfigChange  `json:"config,omitempty"`
	Warnings []string       `json:"warnings"`
}

// ConfigChange is vops.yml differing from the config in effect.
type ConfigChange struct {
	Changes      []string      `json:"changes"`
	Restart      bool          `json:"restart"`       // the ui listener changes: the daemon restarts itself (containers keep running)
	ProxyRestart bool          `json:"proxy_restart"` // http/https/tls change: the proxy restarts itself (sites blink)
	To           config.Config `json:"to"`
}

type ProjectPlan struct {
	Path     string     `json:"path"`
	Disabled bool       `json:"disabled,omitempty"`
	Gone     bool       `json:"gone,omitempty"` // directory removed from git
	Error    string     `json:"error,omitempty"`
	Actions  []Action   `json:"actions"`
	Pins     []PinState `json:"pins,omitempty"`   // services running a past image (image rollback)
	Limits   string     `json:"limits,omitempty"` // proxy limits from x-vops, when they change: "old -> new"

	limits string // desired limits (json proxy.Limits, '' = none)

	specs   map[string]*desired
	actual  map[string][]podman.Container
	nets    []compose.NetworkDef
	project *compose.Project
}

// PinState is a pin as the plan sees it: in effect, or stale (the next apply drops it and runs what compose says).
type PinState struct {
	store.Pin `json:",inline"`
	Stale     string `json:"stale,omitempty"`
}

type Action struct {
	Service string `json:"service"`
	Kind    string `json:"kind"` // create | update | start | remove | none
	Reason  string `json:"reason,omitempty"`
}

type desired struct {
	spec   *compose.Spec
	hash   string
	image  string // final image ref to run
	pull   string // ref to pull from our registry (loopback), when the image is ours
	own    string // "repo:tag" in our registry, when the image is ours
	repo   string // the repo of own
	digest string // manifest digest in our registry, when the image is ours

	pin     *store.Pin // runs a pinned image instead of compose's (image rollback)
	compose *desired   // pinned: what compose says
}

// Changes reports whether the plan does anything.
func (p *Plan) Changes() bool {
	if p.Config != nil {
		return true
	}
	for _, pp := range p.Projects {
		if pp.Error != "" || pp.Limits != "" || slices.ContainsFunc(pp.Pins, func(p PinState) bool { return p.Stale != "" }) {
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
	if c := p.Config; c != nil {
		fmt.Fprintln(w, "vops.yml")
		for _, ch := range c.Changes {
			fmt.Fprintf(w, "  ~ %s\n", ch)
		}
		if c.ProxyRestart {
			fmt.Fprintln(w, "  (the proxy restarts to apply it: sites blink for a moment; containers keep running)")
		}
		if c.Restart {
			fmt.Fprintln(w, "  (the daemon restarts to apply it; containers keep running)")
		}
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
		if pp.Limits != "" {
			fmt.Fprintf(w, "  ~ limits: %s\n", pp.Limits)
		}
		for _, pin := range pp.Pins {
			if pin.Stale != "" {
				fmt.Fprintf(w, "  @ %s: pin to #%d dropped (%s)\n", pin.Service, pin.DeployID, pin.Stale)
			} else {
				fmt.Fprintf(w, "  @ %s pinned to #%d (%s), compose says %s\n", pin.Service, pin.DeployID, shortRef(store.PinRef(pin.Pin)), pin.ComposeImage)
			}
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
	if e.Config != nil {
		current := e.Config()
		switch {
		case err != nil:
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%v: keeping the config in effect", err))
			cfg = current
		case !cfg.Equal(current):
			plan.Config = &ConfigChange{Changes: current.Diff(cfg), Restart: current.UIListener(cfg), ProxyRestart: current.ProxyListeners(cfg), To: cfg}
		}
	} else if err != nil {
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
	allPins, err := e.DB.Pins("")
	if err != nil {
		return nil, err
	}
	pins := map[string]map[string]store.Pin{}
	for _, pin := range allPins {
		if pins[pin.Project] == nil {
			pins[pin.Project] = map[string]store.Pin{}
		}
		pins[pin.Project][pin.Service] = pin
	}
	containers, err := podman.PS(ctx, LProject)
	if err != nil {
		return nil, err
	}
	actual := map[string]map[string][]podman.Container{}
	for _, c := range containers {
		p, s := c.Labels[LProject], c.Labels[LService]
		if compose.IsPreview(p) {
			continue // previews live next to git, not in it (see preview.go)
		}
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
			for _, s := range sortedKeys(pins[p]) {
				pp.Pins = append(pp.Pins, PinState{Pin: pins[p][s], Stale: map[bool]string{true: "project removed from git"}[pp.Gone]})
			}
			continue
		}
		env, err := e.DB.Env(p)
		if err != nil {
			pp.Error = err.Error()
			continue
		}
		if err := e.planProject(ctx, pp, source{root: e.Repo, files: files, env: env, pins: pins[p]}, cfg.Domain, &plan.Warnings); err != nil {
			pp.Error = err.Error()
			continue
		}
		if old := flags[p].Limits; old != pp.limits {
			pp.Limits = LimitsOf(old).String() + " -> " + LimitsOf(pp.limits).String()
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
	claimNames(plan.Projects)
	return plan, nil
}

// claimNames fails a project whose podman names another one already has: they are the project's slug plus a
// key, so "Shop" and "shop", or "shop" with network api and "shop-api", would share networks and volumes.
// The first project (by path) keeps them; renaming existing objects instead would orphan real volumes.
func claimNames(projects []*ProjectPlan) {
	owners := map[string]string{}
	for _, pp := range projects {
		if pp.project == nil || pp.Error != "" {
			continue
		}
		names := map[string]string{"project " + compose.Slug(pp.Path): "project"} // name -> what it is; slugs only clash with slugs
		for _, n := range pp.nets {
			if def := pp.project.Networks[n.Key]; !n.External && (def == nil || def.Name == "") {
				names[n.Name] = "network"
			}
		}
		for key, v := range pp.project.Volumes {
			if v == nil || v.Name == "" && !v.External {
				names[pp.project.VolumeName(key)] = "volume"
			}
		}
		for _, n := range sortedKeys(names) {
			if other, taken := owners[n]; taken {
				pp.Error = fmt.Sprintf("%s name %s is also %s's (podman names are the project dir plus the key): rename the %s or the dir", names[n], n, other, names[n])
				if names[n] == "project" {
					pp.Error = fmt.Sprintf("%s and %s differ only in case, so they would share podman names: rename one dir", other, pp.Path)
				}
				pp.Actions = []Action{}
				break
			}
		}
		if pp.Error == "" {
			for n := range names {
				owners[n] = pp.Path
			}
		}
	}
}

// source is where a project's definition comes from: the repo, or a preview's worktree with its own env and images.
type source struct {
	root     string               // the git working tree (e.Repo or a preview worktree)
	files    []string             // compose files in the project dir
	env      map[string]string    // interpolation and pass-through variables
	images   map[string]string    // preview image overrides: service -> image
	services []string             // what a preview is for (empty = every service with x-vops.preview)
	pins     map[string]store.Pin // image rollback: service -> pinned image (projects only, previews ignore pins)
}

func (e *Engine) planProject(ctx context.Context, pp *ProjectPlan, src source, domain string, warns *[]string) error {
	env := src.env
	for k, v := range builtinEnv(pp.Path, domain) {
		if _, set := env[k]; !set {
			env[k] = v
		}
	}
	base, _, _ := strings.Cut(pp.Path, "@")
	proj, err := compose.Load(filepath.Join(src.root, filepath.FromSlash(base)), pp.Path, src.files, env)
	if err != nil {
		return err
	}
	if compose.IsPreview(pp.Path) {
		var roots []string // left out by COMPOSE_PROFILES = nothing to run
		for _, svc := range src.services {
			if !slices.Contains(proj.Inactive, svc) {
				roots = append(roots, svc)
			}
		}
		if err := proj.PreviewRun(roots); err != nil {
			return err
		}
	}
	*warns = append(*warns, proj.Warnings...)
	for _, svc := range sortedKeys(src.images) {
		if s := proj.Services[svc]; s != nil { // else a pin (preview from a timeline node) of what doesn't run
			s.Image, s.Build = src.images[svc], nil
		}
	}
	pp.project = proj
	pp.limits = limitsJSON(proj.Limits)
	specs, err := proj.Specs(domain, env)
	if err != nil {
		return err
	}
	pp.nets = proj.NetworkDefs()
	for _, n := range pp.nets {
		if !n.External {
			if old, exists := networkHash(ctx, n.Name); exists && old != n.Hash() && !(old == "" && n.Plain()) {
				*warns = append(*warns, fmt.Sprintf("%s: network %s changed and will be recreated: its containers restart", pp.Path, n.Name))
			}
		}
	}
	var order []Action
	for _, sp := range specs {
		d, err := e.resolve(ctx, src.root, sp, domain)
		if err != nil {
			return fmt.Errorf("service %s: %w", sp.Service, err)
		}
		if pin, ok := src.pins[sp.Service]; ok {
			ps := PinState{Pin: pin}
			if now := composeRef(d); now != pin.ComposeImage {
				ps.Stale = "compose says " + now + " now"
			} else {
				pinned, compose := *sp, d
				pinned.Image, pinned.Build = store.PinRef(pin), nil
				if d, err = e.resolve(ctx, src.root, &pinned, domain); err != nil {
					return fmt.Errorf("service %s: pinned image: %w", sp.Service, err)
				}
				d.pin, d.compose = &pin, compose
			}
			pp.Pins = append(pp.Pins, ps)
		}
		pp.specs[sp.Service] = d
		order = append(order, compare(d, pp.actual[sp.Service]))
	}
	for _, s := range sortedKeys(pp.actual) {
		if _, ok := pp.specs[s]; !ok {
			order = append(order, Action{s, "remove", "service removed from compose"})
		}
	}
	for _, s := range sortedKeys(src.pins) {
		if _, ok := pp.specs[s]; !ok {
			pp.Pins = append(pp.Pins, PinState{Pin: src.pins[s], Stale: "service not in compose anymore"})
		}
	}
	pp.Actions = order
	return nil
}

// networkHash returns the vops.nethash label of a network and whether it exists.
func networkHash(ctx context.Context, name string) (string, bool) {
	out, err := podman.Run(ctx, "network", "inspect", "--format", `{{index .Labels "`+LNetHash+`"}}`, name)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(strings.ReplaceAll(out, "<no value>", "")), true
}

// builtinEnv is available for interpolation in every project.
func builtinEnv(project, domain string) map[string]string {
	env := map[string]string{"VOPS_PROJECT": project, "VOPS_DOMAIN": domain}
	if _, name, ok := strings.Cut(project, "@"); ok {
		env["VOPS_PREVIEW"] = name
	}
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
		if d.spec.Job && finished(c.State) && c.ExitCode != 0 {
			a.Kind, a.Reason = "update", fmt.Sprintf("last run failed (exit %d)", c.ExitCode)
			return a
		}
		if c.State == "running" || d.spec.Job && finished(c.State) {
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

// resolve fixes the image of a spec and computes its hash. root is the git working tree the spec comes from.
func (e *Engine) resolve(ctx context.Context, root string, sp *compose.Spec, domain string) (*desired, error) {
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
	for _, n := range sp.Networks {
		w("--net", n.Flag(), n.Hash)
	}
	w("--job", strconv.FormatBool(sp.Job), strconv.Itoa(sp.Port), strconv.Itoa(sp.Replicas))
	w(sp.Domains...)
	switch {
	case sp.Build != nil:
		tree, err := treeHash(ctx, root, sp.Build.Context)
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
			d.own, d.repo, d.digest = repo+":"+ref, repo, digest
			if ref == digest {
				d.image = d.pull // a digest can't be a local tag: run the pulled ref itself
			}
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
// A preview tag (preview-*) is for previews only (PreviewTargets), never production.
func (p *Plan) Watching(repo, tag string) []proxy.Key {
	var out []proxy.Key
	if _, preview := compose.PreviewName(tag); preview {
		return nil
	}
	for _, pp := range p.Projects {
		if pp.Disabled || pp.Gone || pp.Error != "" {
			continue
		}
		for name, d := range pp.specs {
			if d.compose != nil {
				d = d.compose // a push of the tag compose names is a newer version: it ends the pin (ApplyOpts.Unpin)
			}
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
	Pin        *store.Pin         `json:"pin,omitempty"`     // runs a past image (image rollback), not what compose says
	Preview    bool               `json:"preview,omitempty"` // has x-vops.preview: it can get a preview
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
			if st.Image == "" || st.Image == d.pull {
				st.Image = d.spec.Image
			}
			if st.Domains == nil {
				st.Domains = []string{}
			}
			st.Pin = d.pin
			if pp.project != nil && pp.project.Services[n] != nil {
				st.Preview = pp.project.Services[n].Vops.Preview != nil
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

// finished: podman reports a container that ran to completion as exited, or stopped once cleaned up.
func finished(state string) bool { return state == "exited" || state == "stopped" }

// composeRef is the image compose asks for: its image, or the tag vops builds it as.
func composeRef(d *desired) string {
	if d.spec.Build != nil {
		return d.image
	}
	return d.spec.Image
}

// shortRef shortens the digest of an image ref for humans: "r/shop/web@sha256:0123456789ab".
func shortRef(ref string) string {
	if i := strings.Index(ref, "@sha256:"); i >= 0 && len(ref) > i+20 {
		return ref[:i+20]
	}
	return ref
}

func limitsJSON(l compose.Limits) string {
	if l == (compose.Limits{}) {
		return ""
	}
	b, _ := json.Marshal(proxy.Limits(l))
	return string(b)
}

// LimitsOf reads a project's applied limits (store.Project.Limits).
func LimitsOf(s string) *proxy.Limits {
	var l proxy.Limits
	if s != "" {
		json.Unmarshal([]byte(s), &l)
	}
	return &l
}
