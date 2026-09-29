// Package config reads the config files of vops.
//
//	<repo>/vops.yml        committed: the whole config (domain, email, listeners, tls, snapshots, images, previews)
//	~/.vops/config.yml     on the host: the lock, a copy of the last applied vops.yml
//	<repo>/vops-lock.yml   gitignored, local only: how to reach the host
//
// vops.yml is desired state like the compose files: a change shows up in the plan and takes
// effect on apply. The daemon always runs from the lock, so a broken vops.yml can't break it.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Domain string `yaml:"domain"` // projects live under <project>.<domain>, the ui under vops.<domain>
	Email  string `yaml:"email"`  // for Let's Encrypt

	HTTP         string `yaml:"http"`          // default ":80"
	HTTPS        string `yaml:"https"`         // default ":443"; "off" disables it
	UI           string `yaml:"ui"`            // default "127.0.0.1:9984"; "off" disables it
	TLS          string `yaml:"tls"`           // auto (default) | off
	Snapshots    string `yaml:"snapshots"`     // on (default) | off
	SnapshotKeep int    `yaml:"snapshot_keep"` // automatic snapshots kept per project, default 5
	ImageKeep    int    `yaml:"image_keep"`    // registry gc keeps the images of the newest N deploys per project, default 10
	PreviewMax   int    `yaml:"preview_max"`   // previews on this host at once, default 5 (each one is another database)
	PreviewTTL   string `yaml:"preview_ttl"`   // previews not updated for this long are removed, default 3d
	// ACMEDirectory overrides the Let's Encrypt directory (staging, pebble in tests).
	ACMEDirectory string `yaml:"acme_directory,omitempty"`
}

type Lock struct {
	Host   string `yaml:"host"` // user@host
	Port   int    `yaml:"port,omitempty"`
	SSHKey string `yaml:"ssh_key,omitempty"`
}

const (
	RepoFile = "vops.yml"
	LockFile = "vops-lock.yml"
)

// Defaults fills empty fields.
func (c Config) Defaults() Config {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&c.HTTP, ":80")
	def(&c.HTTPS, ":443")
	def(&c.UI, "127.0.0.1:9984")
	def(&c.TLS, "auto")
	def(&c.Snapshots, "on")
	if c.SnapshotKeep == 0 {
		c.SnapshotKeep = 5
	}
	if c.ImageKeep == 0 {
		c.ImageKeep = 10
	}
	if c.PreviewMax == 0 {
		c.PreviewMax = 5
	}
	def(&c.PreviewTTL, "3d")
	c.Domain = strings.ToLower(c.Domain)
	return c
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$`)

func (c Config) Validate() error {
	if c.Domain != "" && !domainRe.MatchString(c.Domain) {
		return fmt.Errorf("domain %q is not a domain", c.Domain)
	}
	if c.Email != "" && !strings.Contains(c.Email, "@") {
		return fmt.Errorf("email %q is not an email", c.Email)
	}
	for name, addr := range map[string]string{"http": c.HTTP, "https": c.HTTPS, "ui": c.UI} {
		if addr == "off" {
			continue
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("%s: %q is not host:port (or off)", name, addr)
		}
	}
	if c.TLS != "auto" && c.TLS != "off" {
		return fmt.Errorf("tls must be auto or off, got %q", c.TLS)
	}
	if c.Snapshots != "on" && c.Snapshots != "off" {
		return fmt.Errorf("snapshots must be on or off, got %q", c.Snapshots)
	}
	if c.SnapshotKeep < 0 {
		return fmt.Errorf("snapshot_keep must be positive")
	}
	if c.ImageKeep < 0 {
		return fmt.Errorf("image_keep must be positive")
	}
	if c.PreviewMax < 0 {
		return fmt.Errorf("preview_max must be positive")
	}
	if _, err := ParseTTL(c.PreviewTTL); err != nil {
		return fmt.Errorf("preview_ttl: %w", err)
	}
	return nil
}

// ParseTTL reads a duration like "3d", "12h" or "90m".
func ParseTTL(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%q is not a duration (3d, 12h, 90m)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a duration (3d, 12h, 90m)", s)
	}
	return d, nil
}

// Previews returns the preview limits.
func (c Config) Previews() (max int, ttl time.Duration) {
	ttl, _ = ParseTTL(c.Defaults().PreviewTTL)
	return c.Defaults().PreviewMax, ttl
}

// ProxyListeners reports whether the proxy's sockets or certificates change (the proxy restarts).
func (c Config) ProxyListeners(o Config) bool {
	return c.HTTP != o.HTTP || c.HTTPS != o.HTTPS || c.TLS != o.TLS || c.ACMEDirectory != o.ACMEDirectory ||
		c.TLS == "auto" && c.Email != o.Email
}

// UIListener reports whether the daemon's ui listener changes (the daemon restarts).
func (c Config) UIListener(o Config) bool { return c.UI != o.UI }

// LockPath is the host copy of the last applied vops.yml (~/.vops/config.yml).
func LockPath(vopsHome string) string { return filepath.Join(vopsHome, "config.yml") }

// Diff lists changed fields as "name: old -> new".
func (c Config) Diff(o Config) []string {
	var out []string
	add := func(name string, a, b any) {
		if fmt.Sprint(a) != fmt.Sprint(b) {
			out = append(out, fmt.Sprintf("%s: %v -> %v", name, orNone(a), orNone(b)))
		}
	}
	add("domain", c.Domain, o.Domain)
	add("email", c.Email, o.Email)
	add("http", c.HTTP, o.HTTP)
	add("https", c.HTTPS, o.HTTPS)
	add("ui", c.UI, o.UI)
	add("tls", c.TLS, o.TLS)
	add("snapshots", c.Snapshots, o.Snapshots)
	add("snapshot_keep", c.SnapshotKeep, o.SnapshotKeep)
	add("image_keep", c.ImageKeep, o.ImageKeep)
	add("preview_max", c.PreviewMax, o.PreviewMax)
	add("preview_ttl", c.PreviewTTL, o.PreviewTTL)
	add("acme_directory", c.ACMEDirectory, o.ACMEDirectory)
	return out
}

func orNone(v any) any {
	if v == "" {
		return `""`
	}
	return v
}

// Parse reads a config from yaml, strictly (a typo is an error, not a silently ignored key).
func Parse(b []byte) (Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return c, err
	}
	c = c.Defaults()
	return c, c.Validate()
}

// Read reads a config file; a missing file is the default config.
func Read(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}.Defaults(), nil
	}
	if err != nil {
		return Config{}, err
	}
	c, err := Parse(b)
	if err != nil {
		return c, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return c, nil
}

// ReadRepo reads <repo>/vops.yml.
func ReadRepo(dir string) (Config, error) { return Read(filepath.Join(dir, RepoFile)) }

// WriteLockFile writes the host lock (~/.vops/config.yml) atomically.
func WriteLockFile(path string, c Config) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	b = append([]byte("# written by vops: a copy of the last applied vops.yml. edit vops.yml in the repo instead;\n# a hand edit here shows up as a change in the next plan and is reverted by apply.\n"), b...)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Template is the vops.yml that init writes.
func Template(domain, email string) string {
	return fmt.Sprintf(`# vops config. committed with the repo; changes show up in "vops plan" and take effect on apply.
# the host keeps a copy of the last applied one in ~/.vops/config.yml.

domain: %q    # projects are served at <project>.<domain>, the dashboard at vops.<domain>
email: %q     # for let's encrypt

http: ":80"
https: ":443"            # "off" to disable
ui: "127.0.0.1:9984"     # dashboard + registry for "vops ui" tunnels; ":9984" exposes it (plain http!)
tls: auto                # auto (let's encrypt) | off
snapshots: on            # btrfs snapshots before every deploy | off
snapshot_keep: 5         # automatic snapshots kept per project
image_keep: 10           # registry gc keeps the images of the last N deploys per project (rollback, previews)
preview_max: 5           # previews on this host at once (each one runs its own copy of the databases)
preview_ttl: 3d          # previews not updated for this long are removed
`, domain, email)
}

// SetFields sets top-level keys in a vops.yml, keeping its comments and layout.
func SetFields(b []byte, fields map[string]string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
		doc.Kind = yaml.DocumentNode
	}
	m := doc.Content[0]
	for k, v := range fields {
		val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
		if _, err := strconv.Atoi(v); err == nil {
			val = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v}
		}
		found := false
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == k {
				val.LineComment = m.Content[i+1].LineComment
				m.Content[i+1] = val
				found = true
			}
		}
		if !found {
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, val)
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ReadLock(dir string) (Lock, error) {
	var l Lock
	b, err := os.ReadFile(filepath.Join(dir, LockFile))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if err := yaml.Unmarshal(b, &l); err != nil {
		return l, fmt.Errorf("%s: %w", LockFile, err)
	}
	return l, nil
}

func WriteLock(dir string, l Lock) error {
	b, err := yaml.Marshal(l)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, LockFile), b, 0o644)
}
