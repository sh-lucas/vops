// Package compose parses the subset of the compose spec that vops supports and turns services into podman args.
// Unsupported keys are errors: silently ignoring config is how production surprises happen.
package compose

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Project is a parsed project directory.
type Project struct {
	Path     string // repo-relative dir, e.g. "shop/api"
	Dir      string // absolute dir on disk
	Services map[string]*Service
	Volumes  map[string]*Volume
	Networks map[string]*Network
	Inactive []string // services left out by COMPOSE_PROFILES
	Skipped  []string // services left out of a preview (x-vops.preview.skip)
	Previews string   // top-level x-vops.previews: registry tags that create previews ("preview-*" when empty, "off")
	Warnings []string
}

type Service struct {
	Name string `yaml:"-"`

	Image           string            `yaml:"image"`
	Build           *Build            `yaml:"build"`
	Command         Words             `yaml:"command"`
	Entrypoint      Words             `yaml:"entrypoint"`
	Environment     Env               `yaml:"environment"`
	EnvFile         Strings           `yaml:"env_file"`
	Ports           []Port            `yaml:"ports"`
	Expose          []any             `yaml:"expose"`
	Volumes         []Mount           `yaml:"volumes"`
	DependsOn       DependsOn         `yaml:"depends_on"`
	Healthcheck     *Healthcheck      `yaml:"healthcheck"`
	Restart         string            `yaml:"restart"`
	User            string            `yaml:"user"`
	WorkingDir      string            `yaml:"working_dir"`
	Labels          Env               `yaml:"labels"`
	CapAdd          []string          `yaml:"cap_add"`
	CapDrop         []string          `yaml:"cap_drop"`
	Devices         []string          `yaml:"devices"`
	ReadOnly        bool              `yaml:"read_only"`
	Tmpfs           Strings           `yaml:"tmpfs"`
	ShmSize         string            `yaml:"shm_size"`
	Init            bool              `yaml:"init"`
	StopSignal      string            `yaml:"stop_signal"`
	StopGracePeriod Duration          `yaml:"stop_grace_period"`
	Hostname        string            `yaml:"hostname"`
	DNS             Strings           `yaml:"dns"`
	ExtraHosts      Env               `yaml:"extra_hosts"`
	SecurityOpt     []string          `yaml:"security_opt"`
	Privileged      bool              `yaml:"privileged"`
	Sysctls         Env               `yaml:"sysctls"`
	Ulimits         map[string]Ulimit `yaml:"ulimits"`
	MemLimit        string            `yaml:"mem_limit"`
	Cpus            float64           `yaml:"cpus"`
	NetworkMode     string            `yaml:"network_mode"`
	Networks        ServiceNetworks   `yaml:"networks"`
	Profiles        []string          `yaml:"profiles"`
	Deploy          *Deploy           `yaml:"deploy"`
	Platform        string            `yaml:"platform"`
	PullPolicy      string            `yaml:"pull_policy"`
	Vops            Vops              `yaml:"x-vops"`

	job bool // a dependency with condition service_completed_successfully: runs to completion
}

// Vops is the `x-vops` block of a service.
type Vops struct {
	Port     int      `yaml:"port"`     // http port inside the container; enables routing
	Domains  []string `yaml:"domains"`  // extra domains
	Health   string   `yaml:"health"`   // http readiness path
	Replicas int      `yaml:"replicas"` // routed services only
	Strategy string   `yaml:"strategy"` // rolling | recreate
	Watch    *bool    `yaml:"watch"`    // redeploy when the tag is pushed to the vops registry (default true)
	Timeout  Duration `yaml:"timeout"`  // readiness timeout (default 60s)
	Preview  struct {
		Skip bool `yaml:"skip"` // not run in previews (workers, crons)
	} `yaml:"preview"`
}

type Build struct {
	Context    string `yaml:"context"`
	Dockerfile string `yaml:"dockerfile"`
	Args       Env    `yaml:"args"`
	Target     string `yaml:"target"`
}

func (b *Build) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		b.Context = n.Value
		return nil
	}
	type plain Build
	return n.Decode((*plain)(b))
}

type Healthcheck struct {
	Test          Words    `yaml:"test"`
	Interval      Duration `yaml:"interval"`
	Timeout       Duration `yaml:"timeout"`
	Retries       int      `yaml:"retries"`
	StartPeriod   Duration `yaml:"start_period"`
	StartInterval Duration `yaml:"start_interval"`
	Disable       bool     `yaml:"disable"`
}

type Deploy struct {
	Replicas  int `yaml:"replicas"`
	Resources struct {
		Limits struct {
			Memory string `yaml:"memory"`
			Cpus   Float  `yaml:"cpus"`
			Pids   int    `yaml:"pids"`
		} `yaml:"limits"`
	} `yaml:"resources"`
}

// Network is a top-level network.
type Network struct {
	Name       string            `yaml:"name"`
	External   bool              `yaml:"external"`
	Internal   bool              `yaml:"internal"` // no route to the outside world
	Driver     string            `yaml:"driver"`
	DriverOpts map[string]string `yaml:"driver_opts"`
	Labels     Env               `yaml:"labels"`
	EnableIPv6 bool              `yaml:"enable_ipv6"`
	Attachable bool              `yaml:"attachable"` // accepted; every podman network is attachable
	IPAM       struct {
		Driver string `yaml:"driver"`
		Config []struct {
			Subnet  string `yaml:"subnet"`
			Gateway string `yaml:"gateway"`
			IPRange string `yaml:"ip_range"`
		} `yaml:"config"`
	} `yaml:"ipam"`
}

// ServiceNetworks is the networks of a service: a list of names or a map of name -> options.
type ServiceNetworks map[string]*NetOptions

type NetOptions struct {
	Aliases     []string `yaml:"aliases"`
	IPv4Address string   `yaml:"ipv4_address"`
	IPv6Address string   `yaml:"ipv6_address"`
}

func (sn *ServiceNetworks) UnmarshalYAML(n *yaml.Node) error {
	*sn = ServiceNetworks{}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			(*sn)[item.Value] = &NetOptions{}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			o := &NetOptions{}
			if v := n.Content[i+1]; v.Tag != "!!null" {
				if err := strictKeys(v, "aliases", "ipv4_address", "ipv6_address"); err != nil {
					return err
				}
				if err := v.Decode(o); err != nil {
					return err
				}
			}
			(*sn)[n.Content[i].Value] = o
		}
	default:
		return fmt.Errorf("line %d: networks must be a list or a map", n.Line)
	}
	return nil
}

// strictKeys rejects unknown keys in mappings decoded by custom unmarshalers.
func strictKeys(n *yaml.Node, allowed ...string) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if k := n.Content[i].Value; !slices.Contains(allowed, k) {
			return fmt.Errorf("line %d: %q is not supported here (supported: %s)", n.Content[i].Line, k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

type Volume struct {
	Name     string `yaml:"name"`
	External bool   `yaml:"external"`
}

type Ulimit struct{ Soft, Hard int }

func (u *Ulimit) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v, err := strconv.Atoi(n.Value)
		u.Soft, u.Hard = v, v
		return err
	}
	var m struct{ Soft, Hard int }
	err := n.Decode(&m)
	u.Soft, u.Hard = m.Soft, m.Hard
	return err
}

// Float accepts "0.5" and 0.5.
type Float float64

func (f *Float) UnmarshalYAML(n *yaml.Node) error {
	v, err := strconv.ParseFloat(n.Value, 64)
	*f = Float(v)
	return err
}

// Duration accepts compose durations ("1m30s", "10s").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }

// Strings accepts a string or a list.
type Strings []string

func (s *Strings) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*s = []string{n.Value}
		return nil
	}
	return n.Decode((*[]string)(s))
}

// Words is a command: a list, or a string split like a shell would.
type Words []string

func (w *Words) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		words, err := SplitWords(n.Value)
		*w = words
		return err
	}
	return n.Decode((*[]string)(w))
}

// Env is a map or a list of KEY=VALUE. A key without value is stored with Unset=true.
type Env []EnvVar

type EnvVar struct {
	Key, Value string
	Unset      bool
}

func (e *Env) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if v.Tag == "!!null" {
				*e = append(*e, EnvVar{Key: k.Value, Unset: true})
			} else {
				*e = append(*e, EnvVar{Key: k.Value, Value: v.Value})
			}
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			k, v, ok := strings.Cut(item.Value, "=")
			if !ok {
				// extra_hosts also allows "host:ip"
				*e = append(*e, EnvVar{Key: k, Unset: true})
				continue
			}
			*e = append(*e, EnvVar{Key: k, Value: v})
		}
	default:
		return fmt.Errorf("line %d: expected a map or a list", n.Line)
	}
	return nil
}

// Dep is one depends_on entry.
type Dep struct {
	Name      string
	Condition string // service_started | service_healthy | service_completed_successfully
	Required  bool
}

type DependsOn []Dep

func (d *DependsOn) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		for _, item := range n.Content {
			*d = append(*d, Dep{item.Value, "service_started", true})
		}
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: depends_on must be a list or a map", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1]
		if err := strictKeys(v, "condition", "required"); err != nil {
			return err
		}
		var o struct {
			Condition string `yaml:"condition"`
			Required  *bool  `yaml:"required"`
		}
		if err := v.Decode(&o); err != nil {
			return err
		}
		dep := Dep{n.Content[i].Value, o.Condition, o.Required == nil || *o.Required}
		switch dep.Condition {
		case "":
			dep.Condition = "service_started"
		case "service_started", "service_healthy", "service_completed_successfully":
		default:
			return fmt.Errorf("line %d: unknown depends_on condition %q", v.Line, dep.Condition)
		}
		*d = append(*d, dep)
	}
	return nil
}

// Names of the dependencies.
func (d DependsOn) Names() []string {
	out := make([]string, len(d))
	for i, x := range d {
		out[i] = x.Name
	}
	return out
}

// Port is a published host port.
type Port struct {
	HostIP    string
	Published string
	Target    string
	Protocol  string
}

func (p *Port) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		var m struct {
			Target    int    `yaml:"target"`
			Published string `yaml:"published"`
			HostIP    string `yaml:"host_ip"`
			Protocol  string `yaml:"protocol"`
			Mode      string `yaml:"mode"`
		}
		if err := n.Decode(&m); err != nil {
			return err
		}
		*p = Port{m.HostIP, m.Published, strconv.Itoa(m.Target), m.Protocol}
		return nil
	}
	s := n.Value
	s, p.Protocol, _ = strings.Cut(s, "/")
	parts := strings.Split(s, ":")
	if strings.HasPrefix(s, "[") { // [::1]:80:80
		i := strings.Index(s, "]")
		p.HostIP, parts = s[1:i], strings.Split(strings.TrimPrefix(s[i+1:], ":"), ":")
	}
	switch len(parts) {
	case 1:
		p.Target = parts[0]
	case 2:
		p.Published, p.Target = parts[0], parts[1]
	case 3:
		p.HostIP, p.Published, p.Target = parts[0], parts[1], parts[2]
	default:
		return fmt.Errorf("line %d: invalid port %q", n.Line, n.Value)
	}
	return nil
}

func (p Port) String() string {
	s := p.Target
	if p.Published != "" || p.HostIP != "" {
		s = p.Published + ":" + s
	}
	if p.HostIP != "" {
		ip := p.HostIP
		if strings.Contains(ip, ":") {
			ip = "[" + ip + "]"
		}
		s = ip + ":" + s
	}
	if p.Protocol != "" {
		s += "/" + p.Protocol
	}
	return s
}

// Mount is a volume entry (short or long syntax).
type Mount struct {
	Type    string // bind | volume | tmpfs
	Source  string
	Target  string
	Options string // ro, z, Z, U...
}

func (m *Mount) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		var l struct {
			Type     string `yaml:"type"`
			Source   string `yaml:"source"`
			Target   string `yaml:"target"`
			ReadOnly bool   `yaml:"read_only"`
			Bind     struct {
				SELinux string `yaml:"selinux"`
			} `yaml:"bind"`
		}
		if err := n.Decode(&l); err != nil {
			return err
		}
		*m = Mount{Type: l.Type, Source: l.Source, Target: l.Target}
		var opts []string
		if l.ReadOnly {
			opts = append(opts, "ro")
		}
		if l.Bind.SELinux != "" {
			opts = append(opts, l.Bind.SELinux)
		}
		m.Options = strings.Join(opts, ",")
		return nil
	}
	parts := strings.Split(n.Value, ":")
	switch len(parts) {
	case 1:
		m.Type, m.Target = "volume", parts[0] // anonymous volume
	case 2, 3:
		m.Source, m.Target = parts[0], parts[1]
		if len(parts) == 3 {
			m.Options = parts[2]
		}
		m.Type = "volume"
		if strings.HasPrefix(m.Source, ".") || strings.HasPrefix(m.Source, "/") || strings.HasPrefix(m.Source, "~") {
			m.Type = "bind"
		}
	default:
		return fmt.Errorf("line %d: invalid volume %q", n.Line, n.Value)
	}
	return nil
}

// ---- loading

var (
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	serviceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	envKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// IsComposeFile reports whether a file name is a compose file.
func IsComposeFile(name string) bool {
	switch name {
	case "compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml":
		return true
	}
	return strings.HasSuffix(name, ".compose.yml") || strings.HasSuffix(name, ".compose.yaml")
}

// ValidProjectPath checks every path segment can become a DNS label. A preview path is "<project>@<name>".
func ValidProjectPath(p string) error {
	base, name, preview := strings.Cut(p, "@")
	for s := range strings.SplitSeq(base, "/") {
		if !segmentRe.MatchString(s) {
			return fmt.Errorf("project %q: directory %q must match %s", p, s, segmentRe)
		}
	}
	if preview && !PreviewNameRe.MatchString(name) {
		return fmt.Errorf("preview name %q must match %s", name, PreviewNameRe)
	}
	return nil
}

// PreviewNameRe: a preview name is a dns label (it becomes <service>.<name>.<project>.<domain>).
var PreviewNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// IsPreview reports whether a project path is a preview ("shop@pr-42").
func IsPreview(path string) bool { return strings.Contains(path, "@") }

// DefaultPreviews is the tag pattern that creates previews when x-vops.previews is not set.
const DefaultPreviews = "preview-*"

// PreviewName reports whether a registry tag matches a previews pattern, and the preview it names (the
// part matched by *). "_" and "." become "-" and letters are lowercased: "preview-PR_42" is preview "pr-42".
// The name may still be invalid (check it with PreviewNameRe); a matching tag is a preview tag either way.
func PreviewName(pattern, tag string) (string, bool) {
	if pattern == "" {
		pattern = DefaultPreviews
	}
	prefix, suffix, ok := strings.Cut(pattern, "*")
	if !ok || len(tag) <= len(prefix)+len(suffix) || !strings.HasPrefix(tag, prefix) || !strings.HasSuffix(tag, suffix) {
		return "", false
	}
	return strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(tag[len(prefix) : len(tag)-len(suffix)])), true
}

// ReadEnvFile reads KEY=VALUE lines (env_file, preview.env).
func ReadEnvFile(path string) (map[string]string, error) { return readEnvFile(path) }

// Load parses the compose files of one project. env is used for interpolation and pass-through variables.
func Load(dir, path string, files []string, env map[string]string) (*Project, error) {
	if err := ValidProjectPath(path); err != nil {
		return nil, err
	}
	p := &Project{Path: path, Dir: dir, Services: map[string]*Service{}, Volumes: map[string]*Volume{}, Networks: map[string]*Network{}}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", path, f, err)
		}
		if len(doc.Content) == 0 {
			continue
		}
		resolveAliases(doc.Content[0], 0)
		var warns []string
		if err := interpolateNode(doc.Content[0], env, &warns); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", path, f, err)
		}
		for _, w := range warns {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s/%s: %s", path, f, w))
		}
		if err := p.topVops(doc.Content[0]); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", path, f, err)
		}
		stripExtensions(doc.Content[0], 0)
		clean, _ := yaml.Marshal(doc.Content[0])
		var file struct {
			Version  string              `yaml:"version"`
			Name     string              `yaml:"name"`
			Services map[string]*Service `yaml:"services"`
			Volumes  map[string]*Volume  `yaml:"volumes"`
			Networks map[string]*Network `yaml:"networks"`
		}
		dec := yaml.NewDecoder(bytes.NewReader(clean))
		dec.KnownFields(true)
		if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s/%s: %s (vops supports a subset of compose, see reference/compose.md)", path, f, strings.TrimPrefix(err.Error(), "yaml: "))
		}
		for name, s := range file.Services {
			if _, dup := p.Services[name]; dup {
				return nil, fmt.Errorf("%s/%s: service %q is defined twice", path, f, name)
			}
			if s == nil {
				s = &Service{}
			}
			s.Name = name
			p.Services[name] = s
		}
		for name, v := range file.Volumes {
			if v == nil {
				v = &Volume{}
			}
			p.Volumes[name] = v
		}
		for name, n := range file.Networks {
			if n == nil {
				n = &Network{}
			}
			p.Networks[name] = n
		}
	}
	if err := p.applyProfiles(env["COMPOSE_PROFILES"]); err != nil {
		return nil, err
	}
	if IsPreview(path) {
		if err := p.previewGuardrails(); err != nil {
			return nil, err
		}
	}
	if err := p.checkNetworks(); err != nil {
		return nil, err
	}
	for _, s := range p.Services {
		for _, d := range s.DependsOn {
			if dep := p.Services[d.Name]; dep != nil && d.Condition == "service_completed_successfully" {
				dep.job = true
			}
		}
	}
	for _, s := range p.Services {
		if err := p.check(s, env); err != nil {
			return nil, fmt.Errorf("%s: service %s: %w", path, s.Name, err)
		}
	}
	if IsPreview(path) {
		// previews never join the shared network: a service that asked for it gets the preview's default network
		for _, s := range p.Services {
			if _, shared := s.Networks[SharedKey]; shared {
				delete(s.Networks, SharedKey)
				s.Networks["default"] = &NetOptions{}
			}
		}
	}
	if _, err := p.Order(); err != nil {
		return nil, err
	}
	return p, nil
}

// topVops reads the top-level x-vops block of a compose file (project settings).
func (p *Project) topVops(root *yaml.Node) error {
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "x-vops" {
			continue
		}
		v := root.Content[i+1]
		if err := strictKeys(v, "previews"); err != nil {
			return fmt.Errorf("x-vops: %w", err)
		}
		var top struct {
			Previews string `yaml:"previews"`
		}
		if err := v.Decode(&top); err != nil {
			return fmt.Errorf("x-vops: %w", err)
		}
		if top.Previews == "" {
			continue
		}
		if top.Previews != "off" && strings.Count(top.Previews, "*") != 1 {
			return fmt.Errorf("x-vops.previews must be a tag pattern with one * (the preview name), or off; got %q", top.Previews)
		}
		if p.Previews != "" && p.Previews != top.Previews {
			return fmt.Errorf("x-vops.previews is set twice (%q and %q)", p.Previews, top.Previews)
		}
		p.Previews = top.Previews
	}
	return nil
}

// previewGuardrails keeps a preview away from production: x-vops.preview.skip services don't run,
// no published host ports, no extra domains, no external volumes.
func (p *Project) previewGuardrails() error {
	for name, s := range p.Services {
		if s.Vops.Preview.Skip {
			delete(p.Services, name)
			p.Skipped = append(p.Skipped, name)
		}
	}
	slices.Sort(p.Skipped)
	for _, s := range p.Services {
		s.DependsOn = slices.DeleteFunc(s.DependsOn, func(d Dep) bool { return slices.Contains(p.Skipped, d.Name) })
		if len(s.Ports) > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: service %s: published ports are left out of previews", p.Path, s.Name))
			s.Ports = nil
		}
		s.Vops.Domains = nil
		for _, m := range s.Volumes {
			if m.Type == "bind" && filepath.IsAbs(m.Source) {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s: service %s: %s is an absolute path, shared with production", p.Path, s.Name, m.Source))
			}
		}
	}
	for key, v := range p.Volumes {
		if v.External {
			return fmt.Errorf("%s: volume %s is external: a preview would write to it, so this project can't have previews", p.Path, key)
		}
	}
	return nil
}

// resolveAliases inlines *alias nodes, so x-* anchors can be stripped afterwards.
func resolveAliases(n *yaml.Node, depth int) {
	if depth > 100 {
		return
	}
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		*n = *deepCopy(n.Alias)
	}
	n.Anchor = ""
	for _, c := range n.Content {
		resolveAliases(c, depth+1)
	}
}

func deepCopy(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, x := range n.Content {
		c.Content[i] = deepCopy(x)
	}
	return &c
}

// stripExtensions drops x-* keys (except x-vops inside a service) so strict decoding accepts them.
func stripExtensions(n *yaml.Node, depth int) {
	if n.Kind != yaml.MappingNode {
		return
	}
	var keep []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if strings.HasPrefix(k, "x-") && !(k == "x-vops" && depth == 2) {
			continue
		}
		if depth < 2 {
			stripExtensions(n.Content[i+1], depth+1)
		}
		keep = append(keep, n.Content[i], n.Content[i+1])
	}
	n.Content = keep
}

func (p *Project) check(s *Service, env map[string]string) error {
	if !serviceRe.MatchString(s.Name) {
		return fmt.Errorf("name must match %s", serviceRe)
	}
	if s.Image == "" && s.Build == nil {
		return errors.New("needs image or build")
	}
	if s.Build != nil && s.Build.Context == "" {
		s.Build.Context = "."
	}
	switch s.NetworkMode {
	case "", "host", "none":
	default:
		return fmt.Errorf("network_mode %q not supported (vops manages networks; use host, none or nothing)", s.NetworkMode)
	}
	switch s.Restart {
	case "":
		s.Restart = "unless-stopped"
		if s.job {
			s.Restart = "no"
		}
	case "no", "always", "on-failure", "unless-stopped":
	default:
		if !strings.HasPrefix(s.Restart, "on-failure:") {
			return fmt.Errorf("invalid restart %q", s.Restart)
		}
	}
	if s.job {
		if s.Restart == "always" || s.Restart == "unless-stopped" {
			return fmt.Errorf("it runs to completion (another service waits for service_completed_successfully), so restart: %s would loop it forever", s.Restart)
		}
		if s.Vops.Port > 0 || s.Vops.Replicas > 1 {
			return errors.New("it runs to completion (another service waits for service_completed_successfully): no x-vops.port or replicas")
		}
		if s.Vops.Timeout == 0 {
			s.Vops.Timeout = Duration(10 * time.Minute)
		}
	}
	for _, d := range s.DependsOn {
		if _, ok := p.Services[d.Name]; !ok {
			return fmt.Errorf("depends_on unknown service %q", d.Name)
		}
	}
	if len(s.Networks) > 0 && s.NetworkMode != "" {
		return errors.New("networks and network_mode can't be used together")
	}
	if s.NetworkMode == "" && len(s.Networks) == 0 {
		s.Networks = ServiceNetworks{"default": {}, SharedKey: {}}
	}
	for key := range s.Networks {
		if key != "default" && key != SharedKey && p.Networks[key] == nil {
			return fmt.Errorf("network %q is not declared in the top-level networks", key)
		}
	}
	if (s.Vops.Port > 0 || len(s.Ports) > 0) && s.NetworkMode == "" {
		reachable := false
		for key := range s.Networks {
			if n := p.Networks[key]; n == nil || !n.Internal {
				reachable = true
			}
		}
		if !reachable {
			return errors.New("routed services and published ports need at least one network that is not internal")
		}
	}
	for i, m := range s.Volumes {
		switch m.Type {
		case "bind":
			src := m.Source
			if strings.HasPrefix(src, "~") {
				return fmt.Errorf("volume %q: use a path relative to the project or an absolute path", src)
			}
			if !filepath.IsAbs(src) {
				src = filepath.Join(p.Dir, src)
			}
			s.Volumes[i].Source = filepath.Clean(src)
		case "volume":
			if m.Source != "" {
				if _, ok := p.Volumes[m.Source]; !ok {
					return fmt.Errorf("volume %q is not declared in the top-level volumes", m.Source)
				}
			}
		case "tmpfs":
		default:
			return fmt.Errorf("volume type %q not supported", m.Type)
		}
		if !strings.HasPrefix(m.Target, "/") {
			return fmt.Errorf("volume target %q must be absolute", m.Target)
		}
	}
	for _, e := range s.Environment {
		if !envKeyRe.MatchString(e.Key) {
			return fmt.Errorf("invalid environment key %q", e.Key)
		}
	}
	if s.Deploy != nil && s.Deploy.Replicas > 0 && s.Vops.Replicas == 0 {
		s.Vops.Replicas = s.Deploy.Replicas
	}
	v := &s.Vops
	if v.Replicas == 0 {
		v.Replicas = 1
	}
	if v.Timeout == 0 {
		v.Timeout = Duration(60 * time.Second)
	}
	if v.Port < 0 || v.Port > 65535 {
		return fmt.Errorf("x-vops.port %d out of range", v.Port)
	}
	if v.Port > 0 && s.NetworkMode != "" {
		return errors.New("x-vops.port can't be used with network_mode")
	}
	if v.Health != "" && !strings.HasPrefix(v.Health, "/") {
		return fmt.Errorf("x-vops.health must be a path, got %q", v.Health)
	}
	for _, d := range v.Domains {
		if !domainRe.MatchString(d) {
			return fmt.Errorf("invalid domain %q", d)
		}
	}
	switch v.Strategy {
	case "":
		v.Strategy = "recreate"
		if v.Port > 0 && len(s.Ports) == 0 {
			v.Strategy = "rolling"
		}
	case "recreate":
	case "rolling":
		if len(s.Ports) > 0 {
			return errors.New("rolling strategy can't publish host ports (two replicas would fight for them); route it with x-vops.port or use recreate")
		}
	default:
		return fmt.Errorf("x-vops.strategy must be rolling or recreate, got %q", v.Strategy)
	}
	if v.Replicas > 1 && (v.Port == 0 || len(s.Ports) > 0) {
		return errors.New("replicas > 1 only works for routed services without host ports")
	}
	return nil
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$|^localhost$`)

// SharedKey is the network key that means "the shared vops network" (reachable from every project).
const SharedKey = "vops"

func (p *Project) checkNetworks() error {
	for key, n := range p.Networks {
		if key == SharedKey {
			return fmt.Errorf("%s: network %q is reserved (it is the shared network every project can join; attach services to it without declaring it)", p.Path, key)
		}
		if !netNameRe.MatchString(key) || n.Name != "" && !netNameRe.MatchString(n.Name) {
			return fmt.Errorf("%s: invalid network name %q", p.Path, key)
		}
		switch n.Driver {
		case "", "bridge", "macvlan", "ipvlan":
		default:
			return fmt.Errorf("%s: network %s: driver %q not supported (bridge, macvlan, ipvlan)", p.Path, key, n.Driver)
		}
		if n.External && (n.Internal || n.Driver != "" || len(n.DriverOpts) > 0 || len(n.IPAM.Config) > 0) {
			return fmt.Errorf("%s: network %s is external: vops doesn't manage it, so it can't configure it", p.Path, key)
		}
	}
	return nil
}

var netNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// applyProfiles drops services whose profiles are not active (COMPOSE_PROFILES, comma separated).
func (p *Project) applyProfiles(profiles string) error {
	active := strings.FieldsFunc(profiles, func(r rune) bool { return r == ',' || r == ' ' })
	dropped := map[string][]string{}
	for name, s := range p.Services {
		if len(s.Profiles) > 0 && !slices.ContainsFunc(s.Profiles, func(x string) bool { return slices.Contains(active, x) || slices.Contains(active, "*") }) {
			delete(p.Services, name)
			dropped[name] = s.Profiles
			p.Inactive = append(p.Inactive, name)
		}
	}
	slices.Sort(p.Inactive)
	for _, s := range p.Services {
		var keep DependsOn
		for _, d := range s.DependsOn {
			if slices.Contains(p.Inactive, d.Name) {
				if d.Required {
					return fmt.Errorf("%s: service %s depends on %s, which is only in profile(s) %s: add one to COMPOSE_PROFILES (vops env set %s COMPOSE_PROFILES=...) or mark the dependency required: false",
						p.Path, s.Name, d.Name, strings.Join(dropped[d.Name], ","), p.Path)
				}
				continue
			}
			keep = append(keep, d)
		}
		s.DependsOn = keep
	}
	return nil
}

// Order returns service names so that dependencies come first.
func (p *Project) Order() ([]string, error) {
	var out []string
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(n string, chain []string) error
	visit = func(n string, chain []string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("%s: dependency cycle: %s", p.Path, strings.Join(append(chain, n), " -> "))
		case 2:
			return nil
		}
		state[n] = 1
		for _, d := range p.Services[n].DependsOn.Names() {
			if err := visit(d, append(chain, n)); err != nil {
				return err
			}
		}
		state[n] = 2
		out = append(out, n)
		return nil
	}
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SplitWords splits a command string like a POSIX shell (quotes and backslashes; no expansion).
func SplitWords(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord, quote := false, rune(0)
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(runes) && strings.ContainsRune(`"\$`+"`", runes[i+1]) {
				i++
				cur.WriteRune(runes[i])
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in %q", s)
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}
