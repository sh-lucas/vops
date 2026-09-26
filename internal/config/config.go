// Package config reads the three config files of vops.
//
//	<repo>/vops.yml       committed: domain, acme email
//	<repo>/vops-lock.yml  gitignored, local only: how to reach the host
//	~/.vops/config.yml    on the host only: listen addresses, tls
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

type Repo struct {
	Domain string `yaml:"domain"` // projects live under <project>.<domain>, the ui under vops.<domain>
	Email  string `yaml:"email"`  // for Let's Encrypt
}

type Lock struct {
	Host   string `yaml:"host"` // user@host
	Port   int    `yaml:"port,omitempty"`
	SSHKey string `yaml:"ssh_key,omitempty"`
}

type Host struct {
	HTTP  string `yaml:"http"`  // default ":80"
	HTTPS string `yaml:"https"` // default ":443"
	UI    string `yaml:"ui"`    // default "127.0.0.1:9984"
	TLS   string `yaml:"tls"`   // auto (default when a domain is set) | off
	// Snapshots: "off" disables btrfs snapshots (and the pre-deploy ones). SnapshotKeep: automatic snapshots kept per project.
	Snapshots    string `yaml:"snapshots,omitempty"`
	SnapshotKeep int    `yaml:"snapshot_keep,omitempty"`
	// ACMEDirectory overrides the Let's Encrypt directory (staging, pebble in tests).
	ACMEDirectory string `yaml:"acme_directory,omitempty"`
}

const (
	RepoFile = "vops.yml"
	LockFile = "vops-lock.yml"
)

func read(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func write(path string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func ReadRepo(dir string) (Repo, error) {
	var r Repo
	err := read(filepath.Join(dir, RepoFile), &r)
	return r, err
}

func WriteRepo(dir string, r Repo) error { return write(filepath.Join(dir, RepoFile), r) }

func ReadLock(dir string) (Lock, error) {
	var l Lock
	err := read(filepath.Join(dir, LockFile), &l)
	return l, err
}

func WriteLock(dir string, l Lock) error { return write(filepath.Join(dir, LockFile), l) }

func ReadHost(path string) (Host, error) {
	h := Host{HTTP: ":80", HTTPS: ":443", UI: "127.0.0.1:9984"}
	err := read(path, &h)
	return h, err
}
