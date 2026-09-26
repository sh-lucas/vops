// Package podman is a thin wrapper over the podman cli.
package podman

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Bin is the podman executable; VOPS_PODMAN overrides it (tests use a wrapper with an isolated storage).
func Bin() string {
	if b := os.Getenv("VOPS_PODMAN"); b != "" {
		return b
	}
	return "podman"
}

// Run runs podman and returns trimmed stdout. Errors carry stderr.
func Run(ctx context.Context, args ...string) (string, error) {
	return RunIn(ctx, nil, args...)
}

// RunIn is Run with stdin.
func RunIn(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, Bin(), args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &out, &errb, stdin
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("podman %s: %s", first(args), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func first(args []string) string {
	if len(args) > 1 {
		return args[0] + " " + args[1]
	}
	return strings.Join(args, " ")
}

// Container is the subset of `podman ps --format json` that vops uses.
type Container struct {
	ID       string            `json:"Id"`
	Names    []string          `json:"Names"`
	Image    string            `json:"Image"`
	ImageID  string            `json:"ImageID"`
	State    string            `json:"State"`
	Status   string            `json:"Status"`
	Created  int64             `json:"Created"`
	Labels   map[string]string `json:"Labels"`
	Exited   bool              `json:"Exited"`
	ExitCode int               `json:"ExitCode"`
}

func (c Container) Name() string {
	if len(c.Names) > 0 {
		return c.Names[0]
	}
	return c.ID[:12]
}

// PS lists all containers (running or not) matching the label filters ("k=v" or "k").
func PS(ctx context.Context, labels ...string) ([]Container, error) {
	args := []string{"ps", "-a", "--format", "json"}
	for _, l := range labels {
		args = append(args, "--filter", "label="+l)
	}
	out, err := Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var cs []Container
	if out == "" {
		return cs, nil
	}
	if err := json.Unmarshal([]byte(out), &cs, json.RejectUnknownMembers(false)); err != nil {
		return nil, fmt.Errorf("podman ps: %w", err)
	}
	return cs, nil
}

// ImageID returns the id of a local image, or "" if it is not present.
func ImageID(ctx context.Context, ref string) string {
	out, err := Run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		return ""
	}
	return out
}

// Mount is a mount of an inspected container.
type Mount struct {
	Type        string `json:"Type"` // volume | bind
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// Mounts returns the mounts of containers by id.
func Mounts(ctx context.Context, ids ...string) (map[string][]Mount, error) {
	out := map[string][]Mount{}
	if len(ids) == 0 {
		return out, nil
	}
	raw, err := Run(ctx, append([]string{"inspect", "--type", "container", "--format", "json"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var cs []struct {
		ID     string  `json:"Id"`
		Mounts []Mount `json:"Mounts"`
	}
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		return nil, fmt.Errorf("podman inspect: %w", err)
	}
	for _, c := range cs {
		out[c.ID] = c.Mounts
	}
	return out, nil
}

// VolumePath returns the mountpoint of a named volume, or "" if it doesn't exist.
func VolumePath(ctx context.Context, name string) string {
	out, err := Run(ctx, "volume", "inspect", "--format", "{{.Mountpoint}}", name)
	if err != nil {
		return ""
	}
	return out
}
