package compose

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Slug is the project path as used in podman object names: "Shop/api" -> "shop.api".
func Slug(path string) string { return strings.ToLower(strings.ReplaceAll(path, "/", ".")) }

// DNSName is the project path reversed as a dns name: "shop/api" -> "api.shop".
func DNSName(path string) string {
	parts := strings.Split(strings.ToLower(strings.ReplaceAll(path, "_", "-")), "/")
	slices.Reverse(parts)
	return strings.Join(parts, ".")
}

func dnsLabel(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "-")) }

// NetworkName is the per-project network.
func NetworkName(path string) string { return "vops-" + Slug(path) }

// SharedNetwork joins every vops container so projects can reach each other.
const SharedNetwork = "vops"

// VolumeName is the podman volume for a named compose volume.
func (p *Project) VolumeName(name string) string {
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
	Service     string
	Image       string // empty when Build is set; deploy builds and fills it
	Build       *Build
	Args        []string // podman run flags that come from the compose file
	Cmd         []string
	Networks    bool // join the project + shared networks (false with network_mode)
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
		Project: p.Path, Service: s.Name, Image: s.Image, Build: s.Build, Cmd: s.Command,
		Networks: s.NetworkMode == "", Port: s.Vops.Port, Health: s.Vops.Health,
		Replicas: s.Vops.Replicas, Strategy: s.Vops.Strategy, Watch: s.Vops.Watch == nil || *s.Vops.Watch,
		Timeout: time.Duration(s.Vops.Timeout), StopWait: 10 * time.Second,
		Healthcheck: s.Healthcheck != nil && !s.Healthcheck.Disable && len(s.Healthcheck.Test) > 0 && s.Healthcheck.Test[0] != "NONE",
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

	// environment: env_file first, then environment; bare keys come from the project env
	envs := map[string]string{}
	for _, f := range s.EnvFile {
		if !filepath.IsAbs(f) {
			f = filepath.Join(p.Dir, f)
		}
		kv, err := readEnvFile(f)
		if err != nil {
			return nil, err
		}
		for k, v := range kv {
			envs[k] = v
		}
	}
	for _, e := range s.Environment {
		if e.Unset {
			if v, ok := projectEnv[e.Key]; ok {
				envs[e.Key] = v
			}
			continue
		}
		envs[e.Key] = e.Value
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
