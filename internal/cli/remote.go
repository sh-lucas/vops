package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Remote is a host reachable over ssh.
type Remote struct {
	Host string // user@host
	Port int
	Key  string
}

// RemoteBin is where install puts the binary; absolute-ish so non-interactive ssh PATH doesn't matter.
const RemoteBin = "~/.vops/bin/vops"

func sshBin() string {
	if b := os.Getenv("VOPS_SSH"); b != "" {
		return b
	}
	return "ssh"
}

func (r *Remote) sshOpts() []string {
	opts := []string{"-o", "StrictHostKeyChecking=accept-new", "-o", "ServerAliveInterval=30"}
	if r.Port != 0 {
		opts = append(opts, "-p", strconv.Itoa(r.Port))
	}
	if r.Key != "" {
		opts = append(opts, "-i", expandHome(r.Key), "-o", "IdentitiesOnly=yes")
	}
	return opts
}

// GitSSH is the GIT_SSH_COMMAND that reaches the same host.
func (r *Remote) GitSSH() string {
	parts := []string{sshBin()}
	for _, o := range r.sshOpts() {
		parts = append(parts, shellQuote(o))
	}
	return strings.Join(parts, " ")
}

// GitURL is the repo on the host.
func (r *Remote) GitURL() string {
	host := r.Host
	if r.Port != 0 {
		host += ":" + strconv.Itoa(r.Port)
	}
	return "ssh://" + host + "/~/vops"
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_./=:@,+", c))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Shell runs a raw shell command on the host.
func (r *Remote) Shell(script string, stdin io.Reader, stdout io.Writer) error {
	args := append(r.sshOpts(), r.Host, script)
	cmd := exec.Command(sshBin(), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return &exitError{ee.ExitCode()}
		}
		return fmt.Errorf("ssh %s: %w", r.Host, err)
	}
	return nil
}

// Run runs `vops --local <args>` on the host. VOPS_CLIENT tells the host which cli sent it: a host
// with another major.minor refuses (exit 3); hosts before 1.3 ignore it.
func (r *Remote) Run(args []string, stdin io.Reader, stdout io.Writer, tty bool) error {
	q := []string{"VOPS_CLIENT=" + shellQuote(Version), RemoteBin, "--local"}
	for _, a := range args {
		q = append(q, shellQuote(a))
	}
	opts := r.sshOpts()
	if tty {
		opts = append(opts, "-t")
	}
	cmd := exec.Command(sshBin(), append(opts, r.Host, strings.Join(q, " "))...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ee.ExitCode() == 255 {
				return fmt.Errorf("ssh %s failed", r.Host)
			}
			return &exitError{ee.ExitCode()}
		}
		return err
	}
	return nil
}

// Output runs a vops command on the host and returns its stdout.
func (r *Remote) Output(args ...string) (string, error) {
	var b strings.Builder
	err := r.Run(args, nil, &b, false)
	return b.String(), err
}
