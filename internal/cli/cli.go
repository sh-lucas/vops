// Package cli is the vops command line. Commands that act on the host run there: locally they are
// forwarded over ssh to `~/.vops/bin/vops --local <cmd>`, which talks to the daemon's unix socket.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/daemon"
)

var Version = "dev"

const usage = `vops: git push your containers to your own server.

on your machine (inside the repo):
  init [--domain d] [--email e]      prepare the repo (vops.yml, .gitignore, registry/)
  install user@host [--ssh-key k]    install or upgrade vops on a host and link this repo to it
                                     (--force: install an older vops over a newer one)
  sync [-y]                          pull, push, show the plan, apply
  ui                                 open the dashboard through an ssh tunnel

anywhere (forwarded to the host over ssh when run inside a linked repo):
  status                             projects, services, containers
  plan                               what apply would do
  apply [-y] [project...]            make the host match git
  logs <project> [service] [-f] [-n N] [--grep s] [--since t] [--until t]
  restart <project> [service]
  enable|disable <project>
  env ls|set|rm <project> [--preview] [KEY=VALUE... | KEY...]   (set reads KEY=VALUE lines from stdin if none given;
                                     --preview: the secrets of the project's previews, which never get its env;
                                     ls also shows each service's variables and where they come from)
  env recipients                     who can open .secrets.age, the encrypted backup of every env secret in git
  env restore [file]                 decrypt .secrets.age with your ssh key and set the secrets the host lacks
  preview up <project> --name n [--image svc=ref]... [--ref r] [--from snapshot-id]
  preview ls [project] | rm <project> <name>   previews of services with x-vops.preview: <service>.<name>.<project>.<domain>
  user ls | rm <name>                users: admins (dashboard + every repo) and deployers (registry only)
  user add <name> --admin            an admin (prompts its password; dashboard and podman login)
  user add <name> --global | --repo r...   a deployer: every repo, or these (prints its token once;
                                     --token-stdin: choose it)
  user token <name> | password <name>   new token for a deployer | new password for an admin
  registry ls [repo] | rm <repo:tag> | gc
  snapshot ls [project] | create <project> [-m note] | rm <id>
  snapshot export <project> <id> > f.tar.gz | import <project> < f.tar.gz   backups (btrfs), off-host
  rollback <project> [deploy-id] [--images] [--data] [--service s]... [--snapshot id] [-y]
                                     back to right before a deploy (default: the last one): its images
                                     (pinned by digest) and its data; undo: rollback <project> <rollback-id>
  unpin <project> [service...]       back to the images compose says (rolling)
  history <project> [-n N]           deploys, rollbacks and snapshots, newest first
  admin password                     same as user password admin
  events [project]                   what happened (deploys, pushes, config)
  notifications                      open alerts (being pushed to the dashboard's devices)
  audit [-n N]                       every write to the host's state
  version

on the host:
  daemon                             run the daemon (systemd does this)
  proxy                              run the proxy on :80/:443 (systemd does this)
  setup                              create dirs, git repo, systemd unit (install does this)

global flags: --host user@host, --port N, --ssh-key path (override vops-lock.yml)
`

type globals struct {
	host, key string
	port      int
	local     bool // run against the socket here, never ssh
}

// Main runs the cli and returns the exit code.
func Main(args []string) int {
	g, rest := parseGlobals(args)
	if len(rest) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := rest[0], rest[1:]
	var err error
	// a forwarded command: refuse when the cli that sent it has another major.minor (setup: install decided)
	if client := os.Getenv("VOPS_CLIENT"); client != "" && !slices.Contains([]string{"version", "--version", "help", "-h", "--help", "setup"}, cmd) {
		err = checkClient(client)
	}
	if err == nil {
		err = dispatch(g, cmd, rest)
	}
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		var ve *versionError
		if errors.As(err, &ve) {
			fmt.Fprintln(os.Stderr, "vops:", ve.msg)
			return exitVersion
		}
		fmt.Fprintln(os.Stderr, "vops:", err)
		return 1
	}
	return 0
}

func dispatch(g globals, cmd string, rest []string) (err error) {
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "version", "--version":
		fmt.Println("vops", Version)
	case "init":
		err = cmdInit(rest)
	case "install":
		err = cmdInstall(g, rest)
	case "sync":
		err = cmdSync(g, rest)
	case "ui":
		err = cmdUI(g, rest)
	case "daemon":
		err = cmdDaemon()
	case "proxy":
		err = cmdProxy()
	case "setup":
		err = cmdSetup(rest)
	case "apply":
		err = cmdApply(g, rest)
	case "env":
		err = cmdEnv(g, rest)
	case "admin":
		err = cmdAdmin(g, rest)
	case "user":
		err = cmdUser(g, rest)
	case "snapshot":
		err = cmdSnapshot(g, rest)
	case "rollback":
		err = cmdRollback(g, rest)
	case "preview":
		err = cmdPreview(g, rest)
	case "status", "plan", "logs", "restart", "enable", "disable", "registry", "events", "notifications", "audit", "history", "unpin":
		err = forward(g, cmd, rest, os.Stdin, os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	return err
}

type exitError struct{ code int }

func (e *exitError) Error() string { return "exit " + strconv.Itoa(e.code) }

func parseGlobals(args []string) (globals, []string) {
	var g globals
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "--") {
			rest = append(rest, a)
			continue
		}
		takes := func() string {
			if hasVal {
				return val
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch name {
		case "host":
			g.host = takes()
		case "ssh-key":
			g.key = takes()
		case "port":
			g.port, _ = strconv.Atoi(takes())
		case "local":
			g.local = true
		default:
			rest = append(rest, a)
		}
	}
	return g, rest
}

// parseFlags parses flags that may come after positional args.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// ---- where to run

// repoRoot is the git toplevel of the cwd, or "".
func repoRoot() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// remote returns the ssh target, or nil when commands should run against the local socket.
func (g globals) remote() (*Remote, error) {
	if g.local {
		return nil, nil
	}
	r := &Remote{Host: g.host, Port: g.port, Key: g.key}
	if root := repoRoot(); root != "" {
		lock, err := config.ReadLock(root)
		if err != nil {
			return nil, err
		}
		if r.Host == "" {
			r.Host = lock.Host
		}
		if r.Port == 0 {
			r.Port = lock.Port
		}
		if r.Key == "" {
			r.Key = lock.SSHKey
		}
	}
	if r.Host == "" {
		return nil, nil
	}
	return r, nil
}

// forward runs a host command: over ssh if linked to a remote, else against the local daemon.
func forward(g globals, cmd string, args []string, stdin io.Reader, stdout io.Writer) error {
	r, err := g.remote()
	if err != nil {
		return err
	}
	if r != nil {
		tty := cmd == "logs" && isTerminal(os.Stdin) && contains(args, "-f")
		return r.Run(append([]string{cmd}, args...), stdin, stdout, tty)
	}
	return runHere(cmd, args, stdin, stdout)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// ---- socket client (on the host)

func vopsHome() string {
	home, _ := os.UserHomeDir()
	h, _ := daemon.Paths(home)
	return h
}

type client struct{ http *http.Client }

func socketClient() (*client, error) {
	sock := daemon.SocketPath(vopsHome())
	if _, err := os.Stat(sock); err != nil {
		return nil, fmt.Errorf("no vops daemon here (%s missing) and no host linked: run inside a repo with vops-lock.yml, or pass --host", sock)
	}
	return &client{&http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}, nil
}

func (c *client) do(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://vops"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(b))
		if i := strings.Index(msg, `"error":"`); i >= 0 {
			msg = strings.TrimSuffix(strings.TrimSuffix(msg[i+9:], "}"), `"`)
		}
		return nil, errors.New(msg)
	}
	return resp, nil
}

// stream copies a streamed response and fails if its last line is "==> error: ...".
func stream(resp *http.Response, w io.Writer) error {
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	last := ""
	for sc.Scan() {
		last = sc.Text()
		if strings.HasPrefix(last, "==> ") {
			continue
		}
		fmt.Fprintln(w, last)
	}
	if msg, ok := strings.CutPrefix(last, "==> error: "); ok {
		return errors.New(msg)
	}
	return sc.Err()
}

// ---- small helpers

func confirm(question string) bool {
	if !isTerminal(os.Stdin) {
		return false
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// readSecret reads a line without echo when stdin is a terminal.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if isTerminal(os.Stdin) {
		stty := exec.Command("stty", "-echo")
		stty.Stdin = os.Stdin
		if stty.Run() == nil {
			defer func() {
				on := exec.Command("stty", "echo")
				on.Stdin = os.Stdin
				on.Run()
				fmt.Fprintln(os.Stderr)
			}()
		}
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}
