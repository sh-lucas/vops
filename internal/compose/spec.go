package compose

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// NormalizeImage makes short names explicit the way docker and compose read them: "postgres:16" is
// docker.io/library/postgres:16, "user/app" is docker.io/user/app. podman refuses short names unless
// the host configured search registries, so a compose file that works with docker would fail here.
func NormalizeImage(ref string) string {
	name := ref
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}
	first, _, hasSlash := strings.Cut(name, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return ref
	}
	if !hasSlash {
		return "docker.io/library/" + ref
	}
	return "docker.io/" + ref
}

// Slug is the project path as used in podman object names: "Shop/api" -> "shop.api", preview "shop@pr-1" -> "shop--pr-1".
func Slug(path string) string {
	return strings.ToLower(strings.NewReplacer("/", ".", "@", "--").Replace(path))
}

// DNSName is the project path reversed as a dns name: "shop/api" -> "api.shop", preview "shop/api@pr-1" -> "pr-1.api.shop".
func DNSName(path string) string {
	base, preview, _ := strings.Cut(path, "@")
	parts := strings.Split(strings.ToLower(strings.ReplaceAll(base, "_", "-")), "/")
	slices.Reverse(parts)
	if preview != "" {
		parts = append([]string{preview}, parts...)
	}
	return strings.Join(parts, ".")
}

func dnsLabel(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "-")) }

// NetworkName is the per-project network.
func NetworkName(path string) string { return "vops-" + Slug(path) }

// SharedNetwork joins every vops container so projects can reach each other.
const SharedNetwork = "vops"

// VolumeName is the podman volume for a named compose volume. Previews ignore `name:` (it would be production's volume).
func (p *Project) VolumeName(name string) string {
	if IsPreview(p.Path) {
		return "vops-" + Slug(p.Path) + "-" + name
	}
	if v := p.Volumes[name]; v != nil && v.External {
		if v.Name != "" {
			return v.Name
		}
		return name
	}
	if v := p.Volumes[name]; v != nil && v.Name != "" {
		return v.Name
	}
	return "vops-" + Slug(p.Path) + "-" + name
}

// Spec is everything deploy needs to run a service. Args + Cmd + image define the service; their hash decides redeploys.
type Spec struct {
	Project     string
	Dir         string // the project dir on disk (in the repo, or in a preview's worktree)
	Service     string
	Image       string // empty when Build is set; deploy builds and fills it
	Build       *Build
	Args        []string // podman run flags that come from the compose file
	Cmd         []string
	Networks    []SpecNet // empty with network_mode
	Job         bool      // runs to completion; ready = exited 0
	DependsOn   []string
	Port        int
	Domains     []string
	Health      string
	Healthcheck bool
	Replicas    int
	Strategy    string
	Watch       bool
	Timeout     time.Duration
	StopWait    time.Duration
	Volumes     []string // podman volumes to create first
	Binds       []string // bind mount sources to create first
}

// Specs builds the specs of every service in dependency order. rootDomain may be empty (no routing domains).
func (p *Project) Specs(rootDomain string, env map[string]string) ([]*Spec, error) {
	order, err := p.Order()
	if err != nil {
		return nil, err
	}
	routed := 0
	for _, s := range p.Services {
		if s.Vops.Port > 0 {
			routed++
		}
	}
	var out []*Spec
	for _, name := range order {
		s := p.Services[name]
		spec, err := p.spec(s, rootDomain, routed == 1, env)
		if err != nil {
			return nil, fmt.Errorf("%s: service %s: %w", p.Path, name, err)
		}
		out = append(out, spec)
	}
	return out, nil
}

func (p *Project) spec(s *Service, rootDomain string, onlyRouted bool, projectEnv map[string]string) (*Spec, error) {
	sp := &Spec{
		Project: p.Path, Dir: p.Dir, Service: s.Name, Image: NormalizeImage(s.Image), Build: s.Build, Cmd: s.Command,
		Job: s.job, DependsOn: s.DependsOn.Names(), Port: s.Vops.Port, Health: s.Vops.Health,
		Replicas: s.Vops.Replicas, Strategy: s.Vops.Strategy, Watch: s.Vops.Watch == nil || *s.Vops.Watch,
		Timeout: time.Duration(s.Vops.Timeout), StopWait: 10 * time.Second,
		Healthcheck: s.Healthcheck != nil && !s.Healthcheck.Disable && len(s.Healthcheck.Test) > 0 && s.Healthcheck.Test[0] != "NONE",
	}
	for _, key := range sortedKeys(s.Networks) {
		o := s.Networks[key]
		if o == nil {
			o = &NetOptions{}
		}
		n := SpecNet{Name: p.netName(key), IP: o.IPv4Address, IP6: o.IPv6Address}
		if key == SharedKey {
			n.Aliases = append(n.Aliases, dnsLabel(s.Name)+"."+DNSName(p.Path))
		} else {
			n.Aliases = append(n.Aliases, strings.ToLower(s.Name))
			n.Hash = p.networkDef(key).Hash()
		}
		for _, a := range o.Aliases {
			if !slices.Contains(n.Aliases, a) {
				n.Aliases = append(n.Aliases, a)
			}
		}
		sp.Networks = append(sp.Networks, n)
	}
	if s.Build != nil {
		sp.Image = ""
		b := *s.Build
		if !filepath.IsAbs(b.Context) {
			b.Context = filepath.Join(p.Dir, b.Context)
		}
		sp.Build = &b
	}
	if sp.Port > 0 {
		sp.Domains = append(sp.Domains, s.Vops.Domains...)
		if rootDomain != "" {
			base := DNSName(p.Path) + "." + rootDomain
			if onlyRouted {
				sp.Domains = append(sp.Domains, base)
			}
			sp.Domains = append(sp.Domains, dnsLabel(s.Name)+"."+base)
		}
	}
	var a []string
	add := func(xs ...string) { a = append(a, xs...) }

	envs, _, err := p.serviceEnv(s, projectEnv)
	if err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(envs) {
		add("-e", k+"="+envs[k])
	}
	for _, l := range s.Labels {
		if strings.HasPrefix(l.Key, "vops.") {
			return nil, fmt.Errorf("label %q: vops.* labels are reserved", l.Key)
		}
		add("--label", l.Key+"="+l.Value)
	}
	if len(s.Entrypoint) > 0 {
		add("--entrypoint", jsonList(s.Entrypoint))
	}
	for _, port := range s.Ports {
		add("-p", port.String())
	}
	for _, m := range s.Volumes {
		switch m.Type {
		case "bind":
			sp.Binds = append(sp.Binds, m.Source)
			add("-v", joinNonEmpty(":", m.Source, m.Target, m.Options))
		case "volume":
			if m.Source == "" {
				add("-v", m.Target)
				continue
			}
			v := p.VolumeName(m.Source)
			if p.Volumes[m.Source] == nil || !p.Volumes[m.Source].External {
				sp.Volumes = append(sp.Volumes, v)
			}
			add("-v", joinNonEmpty(":", v, m.Target, m.Options))
		case "tmpfs":
			add("--tmpfs", m.Target)
		}
	}
	if h := s.Healthcheck; h != nil {
		if h.Disable || len(h.Test) > 0 && h.Test[0] == "NONE" {
			add("--no-healthcheck")
		} else if len(h.Test) > 0 {
			switch h.Test[0] {
			case "CMD":
				add("--health-cmd", jsonList(h.Test[1:]))
			case "CMD-SHELL":
				add("--health-cmd", strings.Join(h.Test[1:], " "))
			default:
				add("--health-cmd", strings.Join(h.Test, " "))
			}
			if h.Interval > 0 {
				add("--health-interval", h.Interval.String())
			}
			if h.Timeout > 0 {
				add("--health-timeout", h.Timeout.String())
			}
			if h.Retries > 0 {
				add("--health-retries", strconv.Itoa(h.Retries))
			}
			if h.StartPeriod > 0 {
				add("--health-start-period", h.StartPeriod.String())
			}
		}
	}
	add("--restart", s.Restart)
	opt := func(flag, v string) {
		if v != "" {
			add(flag, v)
		}
	}
	opt("--user", s.User)
	opt("--workdir", s.WorkingDir)
	opt("--shm-size", s.ShmSize)
	opt("--stop-signal", s.StopSignal)
	opt("--hostname", s.Hostname)
	opt("--memory", s.MemLimit)
	opt("--platform", s.Platform)
	if s.NetworkMode != "" {
		add("--network", s.NetworkMode)
	}
	if s.StopGracePeriod > 0 {
		sp.StopWait = time.Duration(s.StopGracePeriod)
		add("--stop-timeout", strconv.Itoa(int(sp.StopWait.Seconds())))
	}
	if s.Cpus > 0 {
		add("--cpus", strconv.FormatFloat(s.Cpus, 'f', -1, 64))
	}
	if d := s.Deploy; d != nil {
		opt("--memory", d.Resources.Limits.Memory)
		if d.Resources.Limits.Cpus > 0 {
			add("--cpus", strconv.FormatFloat(float64(d.Resources.Limits.Cpus), 'f', -1, 64))
		}
		if d.Resources.Limits.Pids > 0 {
			add("--pids-limit", strconv.Itoa(d.Resources.Limits.Pids))
		}
	}
	for _, c := range s.CapAdd {
		add("--cap-add", c)
	}
	for _, c := range s.CapDrop {
		add("--cap-drop", c)
	}
	for _, d := range s.Devices {
		add("--device", d)
	}
	for _, t := range s.Tmpfs {
		add("--tmpfs", t)
	}
	for _, d := range s.DNS {
		add("--dns", d)
	}
	for _, h := range s.ExtraHosts {
		if h.Unset { // "host:ip" list form
			add("--add-host", h.Key)
		} else {
			add("--add-host", h.Key+":"+h.Value)
		}
	}
	for _, o := range s.SecurityOpt {
		add("--security-opt", o)
	}
	for _, sc := range s.Sysctls {
		add("--sysctl", sc.Key+"="+sc.Value)
	}
	for _, k := range sortedKeys(s.Ulimits) {
		u := s.Ulimits[k]
		add("--ulimit", fmt.Sprintf("%s=%d:%d", k, u.Soft, u.Hard))
	}
	if s.ReadOnly {
		add("--read-only")
	}
	if s.Init {
		add("--init")
	}
	if s.Privileged {
		add("--privileged")
	}
	if s.PullPolicy == "always" {
		add("--pull", "always")
	}
	sp.Args = a
	return sp, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func joinNonEmpty(sep string, parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, sep)
}

func jsonList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return "[" + strings.Join(q, ",") + "]"
}

// readEnvFile reads KEY=VALUE lines; # comments, blank lines, "export " and surrounding quotes are handled.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("env_file: %w", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envKeyRe.MatchString(k) {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// ---- networks

// SpecNet is one network a container joins.
type SpecNet struct {
	Name    string // podman network name
	Aliases []string
	IP, IP6 string
	Hash    string // definition hash, so a changed network redeploys its services
}

// Flag is the value of podman's --network.
func (n SpecNet) Flag() string {
	var opts []string
	for _, a := range n.Aliases {
		opts = append(opts, "alias="+a)
	}
	if n.IP != "" {
		opts = append(opts, "ip="+n.IP)
	}
	if n.IP6 != "" {
		opts = append(opts, "ip6="+n.IP6)
	}
	if len(opts) == 0 {
		return n.Name
	}
	return n.Name + ":" + strings.Join(opts, ",")
}

// NetworkDef is a network vops creates, or expects to exist when External.
type NetworkDef struct {
	Key, Name string
	External  bool
	Internal  bool
	IPv6      bool
	Driver    string
	Opts      map[string]string
	Labels    map[string]string
	Subnets   [][3]string // subnet, gateway, ip range
}

// Hash identifies the definition; a different hash means the network must be recreated.
func (d NetworkDef) Hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%v|%v|%v|%s|", d.Name, d.External, d.Internal, d.IPv6, d.Driver)
	for _, k := range sortedKeys(d.Opts) {
		fmt.Fprintf(h, "o:%s=%s|", k, d.Opts[k])
	}
	for _, k := range sortedKeys(d.Labels) {
		fmt.Fprintf(h, "l:%s=%s|", k, d.Labels[k])
	}
	for _, sn := range d.Subnets {
		fmt.Fprintf(h, "s:%v|", sn)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Plain is a default bridge network: what vops created before networks were configurable.
func (d NetworkDef) Plain() bool {
	return !d.External && !d.Internal && !d.IPv6 && (d.Driver == "" || d.Driver == "bridge") && len(d.Opts) == 0 && len(d.Labels) == 0 && len(d.Subnets) == 0
}

// CreateArgs are the podman network create flags (without the name).
func (d NetworkDef) CreateArgs() []string {
	var a []string
	if d.Internal {
		a = append(a, "--internal")
	}
	if d.IPv6 {
		a = append(a, "--ipv6")
	}
	if d.Driver != "" {
		a = append(a, "--driver", d.Driver)
	}
	for _, k := range sortedKeys(d.Opts) {
		a = append(a, "--opt", k+"="+d.Opts[k])
	}
	for _, k := range sortedKeys(d.Labels) {
		a = append(a, "--label", k+"="+d.Labels[k])
	}
	for _, sn := range d.Subnets {
		if sn[0] != "" {
			a = append(a, "--subnet", sn[0])
		}
		if sn[1] != "" {
			a = append(a, "--gateway", sn[1])
		}
		if sn[2] != "" {
			a = append(a, "--ip-range", sn[2])
		}
	}
	return a
}

func (p *Project) netName(key string) string {
	if key == SharedKey {
		return SharedNetwork
	}
	n := p.Networks[key]
	switch {
	case n != nil && n.Name != "" && (n.External || !IsPreview(p.Path)):
		return n.Name
	case n != nil && n.External:
		return key
	case key == "default":
		return NetworkName(p.Path)
	}
	return NetworkName(p.Path) + "-" + key
}

func (p *Project) networkDef(key string) NetworkDef {
	d := NetworkDef{Key: key, Name: p.netName(key)}
	n := p.Networks[key]
	if n == nil {
		return d
	}
	d.External, d.Internal, d.IPv6, d.Driver = n.External, n.Internal, n.EnableIPv6, n.Driver
	if len(n.DriverOpts) > 0 {
		d.Opts = n.DriverOpts
	}
	for _, l := range n.Labels {
		if d.Labels == nil {
			d.Labels = map[string]string{}
		}
		d.Labels[l.Key] = l.Value
	}
	for _, c := range n.IPAM.Config {
		d.Subnets = append(d.Subnets, [3]string{c.Subnet, c.Gateway, c.IPRange})
	}
	return d
}

// NetworkDefs lists the networks the project's services use (not the shared one), sorted by name.
func (p *Project) NetworkDefs() []NetworkDef {
	used := map[string]bool{}
	for _, s := range p.Services {
		for key := range s.Networks {
			if key != SharedKey {
				used[key] = true
			}
		}
	}
	var out []NetworkDef
	for _, key := range sortedKeys(used) {
		out = append(out, p.networkDef(key))
	}
	return out
}

// EnvEntry is one variable a service's containers get (or ask for) and where it comes from. Never its value.
type EnvEntry struct {
	Key    string   `json:"key"`
	Source string   `json:"source"`           // compose | env_file | project (bare key from the project env) | missing
	File   string   `json:"file,omitempty"`   // env_file: the file, as written in compose
	Vars   []string `json:"vars,omitempty"`   // compose and missing: the ${VAR}s its value uses
	Secret bool     `json:"secret,omitempty"` // a literal whose key looks secret: it is committed to git
}

// serviceEnv is a service's environment as podman gets it: env_file first, then environment; bare keys
// (environment: [KEY]) come from the project env. entries says where each key comes from, sorted by key.
func (p *Project) serviceEnv(s *Service, projectEnv map[string]string) (map[string]string, []EnvEntry, error) {
	envs, from := map[string]string{}, map[string]EnvEntry{}
	for _, f := range s.EnvFile {
		path := f
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.Dir, path)
		}
		kv, err := readEnvFile(path)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range kv {
			envs[k] = v
			from[k] = EnvEntry{Key: k, Source: "env_file", File: f}
		}
	}
	for _, e := range s.Environment {
		if e.Unset {
			if v, ok := projectEnv[e.Key]; ok {
				envs[e.Key] = v
				from[e.Key] = EnvEntry{Key: e.Key, Source: "project"}
			} else if _, ok := from[e.Key]; !ok {
				from[e.Key] = EnvEntry{Key: e.Key, Source: "missing"}
			}
			continue
		}
		envs[e.Key] = e.Value
		en := EnvEntry{Key: e.Key, Source: "compose", Vars: p.EnvRefs[s.Name][e.Key]}
		for _, v := range en.Vars {
			if _, set := projectEnv[v]; !set && e.Value == "" {
				en.Source = "missing"
			}
		}
		from[e.Key] = en
	}
	var entries []EnvEntry
	for _, k := range sortedKeys(from) {
		entries = append(entries, from[k])
	}
	return envs, entries, nil
}

// EnvUsage lists, per active service, every env variable its containers get and where it comes from.
func (p *Project) EnvUsage(projectEnv map[string]string) (map[string][]EnvEntry, error) {
	out := map[string][]EnvEntry{}
	for name, s := range p.Services {
		_, entries, err := p.serviceEnv(s, projectEnv)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		if entries == nil {
			entries = []EnvEntry{}
		}
		out[name] = entries
	}
	return out, nil
}
