package cli

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/daemon"
	"github.com/sh-lucas/vops/internal/deploy"
)

func run(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, os.Stderr, os.Stderr
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func output(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// ensureLine appends line to file unless it is already there.
func ensureLine(file, line string) error {
	b, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for l := range strings.SplitSeq(string(b), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		b = append(b, '\n')
	}
	os.MkdirAll(filepath.Dir(file), 0o755)
	return os.WriteFile(file, append(b, []byte(line+"\n")...), 0o644)
}

// ---- init

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	domain := fs.String("domain", "", "")
	email := fs.String("email", "", "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	root := repoRoot()
	if root == "" {
		cwd, _ := os.Getwd()
		if err := run(cwd, nil, "git", "init", "-q", "-b", "main"); err != nil {
			return err
		}
		root = cwd
	}
	cfg, err := config.ReadRepo(root)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(filepath.Join(root, config.RepoFile))
	if *domain != "" {
		cfg.Domain = strings.ToLower(*domain)
	}
	if *email != "" {
		cfg.Email = *email
	}
	if statErr != nil || *domain != "" || *email != "" {
		if err := config.WriteRepo(root, cfg); err != nil {
			return err
		}
	}
	if err := ensureLine(filepath.Join(root, "registry", ".gitignore"), "data/"); err != nil {
		return err
	}
	if err := ensureLine(filepath.Join(root, ".gitignore"), config.LockFile); err != nil {
		return err
	}
	fmt.Printf(`vops repo ready at %s

  %s      domain: %q, email: %q
  registry/     your registry lives here on the host (data/ is ignored)

next:
  1. point *.%s and %s at the host (dns A records)
  2. vops install user@host
  3. mkdir myapp && write myapp/compose.yml, commit, vops sync
`, root, config.RepoFile, cfg.Domain, cfg.Email, orDash(cfg.Domain), orDash(cfg.Domain))
	return nil
}

// ---- install

var archNames = map[string]string{"x86_64": "amd64", "aarch64": "arm64", "armv7l": "arm", "riscv64": "riscv64"}

func cmdInstall(g globals, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	binary := fs.String("binary", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	r := &Remote{Host: g.host, Port: g.port, Key: g.key}
	if len(pos) > 0 {
		r.Host = pos[0]
	}
	if r.Host == "" {
		if rr, _ := g.remote(); rr != nil {
			r = rr
		} else {
			return errors.New("usage: vops install user@host [--ssh-key path] [--port 22]")
		}
	}

	fmt.Fprintf(os.Stderr, "checking %s\n", r.Host)
	var probe bytes.Buffer
	if err := r.Shell(`uname -m; for c in podman git systemctl; do command -v $c >/dev/null 2>&1 && echo has-$c; done; true`, nil, &probe); err != nil {
		return err
	}
	lines := strings.Fields(probe.String())
	if len(lines) == 0 {
		return errors.New("could not probe the host")
	}
	arch := archNames[lines[0]]
	var missing []string
	for _, c := range []string{"podman", "git", "systemctl"} {
		if !slices.Contains(lines, "has-"+c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the host is missing: %s (install them first, e.g. apt install podman git)", strings.Join(missing, ", "))
	}
	bin := *binary
	if bin == "" {
		if arch != runtime.GOARCH {
			return fmt.Errorf("the host is %s but this vops is %s: build one with GOARCH=%s and pass --binary", lines[0], runtime.GOARCH, arch)
		}
		if bin, err = os.Executable(); err != nil {
			return err
		}
	}
	f, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(os.Stderr, "uploading %s\n", bin)
	if err := r.Shell(`mkdir -p ~/.vops/bin && cat > ~/.vops/bin/vops.new && chmod 755 ~/.vops/bin/vops.new && mv -f ~/.vops/bin/vops.new ~/.vops/bin/vops`, f, os.Stdout); err != nil {
		return err
	}
	if err := r.Run([]string{"setup"}, nil, os.Stdout, false); err != nil {
		return err
	}
	if root := repoRoot(); root != "" {
		if err := linkRepo(root, r); err != nil {
			return err
		}
		fmt.Printf("\nlinked %s to %s (vops-lock.yml, git remote \"vops\"); next: vops sync\n", root, r.Host)
	}
	return nil
}

func linkRepo(root string, r *Remote) error {
	if err := config.WriteLock(root, config.Lock{Host: r.Host, Port: r.Port, SSHKey: r.Key}); err != nil {
		return err
	}
	if err := ensureLine(filepath.Join(root, ".gitignore"), config.LockFile); err != nil {
		return err
	}
	return ensureGitRemote(root, r)
}

func ensureGitRemote(root string, r *Remote) error {
	url := r.GitURL()
	current, err := output(root, "git", "remote", "get-url", "vops")
	if err != nil {
		return run(root, nil, "git", "remote", "add", "vops", url)
	}
	if current != url {
		return run(root, nil, "git", "remote", "set-url", "vops", url)
	}
	return nil
}

// ---- sync

func cmdSync(g globals, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	fs.BoolVar(yes, "y", false, "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	root := repoRoot()
	if root == "" {
		return errors.New("not inside a git repo")
	}
	r, err := g.remote()
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("this repo is not linked to a host: vops install user@host (or pass --host)")
	}
	if err := ensureGitRemote(root, r); err != nil {
		return err
	}
	env := []string{"GIT_SSH_COMMAND=" + r.GitSSH()}
	if _, err := output(root, "git", "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		return errors.New("nothing committed yet")
	}
	if dirty, _ := output(root, "git", "status", "--porcelain"); dirty != "" {
		fmt.Fprintln(os.Stderr, "note: uncommitted changes are not synced")
	}
	fmt.Fprintln(os.Stderr, "pulling")
	if err := run(root, env, "git", "fetch", "-q", "vops"); err != nil {
		return err
	}
	if _, err := output(root, "git", "rev-parse", "--verify", "-q", "refs/remotes/vops/main"); err == nil {
		if _, err := output(root, "git", "merge-base", "--is-ancestor", "vops/main", "HEAD"); err != nil {
			if err := run(root, env, "git", "merge", "--no-edit", "vops/main"); err != nil {
				return fmt.Errorf("could not merge the host's changes: resolve the conflict, commit, and sync again (%w)", err)
			}
		}
	}
	fmt.Fprintln(os.Stderr, "pushing")
	if err := run(root, env, "git", "push", "-q", "vops", "HEAD:main"); err != nil {
		return err
	}
	head, _ := output(root, "git", "rev-parse", "HEAD")
	return applyFlow(g, *yes, head, nil)
}

// ---- apply

func cmdApply(g globals, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	fs.BoolVar(yes, "y", false, "")
	commit := fs.String("commit", "", "")
	projects, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if *yes && *commit != "" {
		return forward(g, "apply", append([]string{"--yes", "--commit", *commit}, projects...), nil, os.Stdout)
	}
	return applyFlow(g, *yes, *commit, projects)
}

// applyFlow shows the plan, asks, then applies exactly the commit that was shown.
func applyFlow(g globals, yes bool, commit string, projects []string) error {
	var buf bytes.Buffer
	if err := forward(g, "plan", []string{"--json"}, nil, &buf); err != nil {
		return err
	}
	var plan deploy.Plan
	if err := json.Unmarshal(buf.Bytes(), &plan); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if commit != "" && plan.Commit != commit {
		return fmt.Errorf("the host is at %.12s, expected %.12s", plan.Commit, commit)
	}
	plan.Print(os.Stdout)
	if !plan.Changes() {
		// nothing changes, but the host should record that it matches this commit
		return forward(g, "apply", append([]string{"--yes", "--commit", plan.Commit}, projects...), nil, io.Discard)
	}
	if !yes {
		if !isTerminal(os.Stdin) {
			return errors.New("not a terminal: pass --yes to apply without asking")
		}
		if !confirm("apply?") {
			return errors.New("aborted")
		}
	}
	return forward(g, "apply", append([]string{"--yes", "--commit", plan.Commit}, projects...), nil, os.Stdout)
}

// ---- env and admin read secrets here and send them over stdin, never on a command line

func cmdEnv(g globals, args []string) error {
	if len(args) < 2 || args[0] != "set" || slices.Contains(args, "--stdin") {
		return forward(g, "env", args, os.Stdin, os.Stdout)
	}
	project, pairs := args[1], args[2:]
	var lines bytes.Buffer
	for _, p := range pairs {
		if k, _, ok := strings.Cut(p, "="); ok && k != "" {
			lines.WriteString(p + "\n")
			continue
		}
		v, err := readSecret(p + "=")
		if err != nil {
			return err
		}
		lines.WriteString(p + "=" + v + "\n")
	}
	if len(pairs) == 0 {
		if isTerminal(os.Stdin) {
			return errors.New("usage: vops env set <project> KEY=VALUE... | KEY (prompts) | < file.env")
		}
		if _, err := lines.ReadFrom(os.Stdin); err != nil {
			return err
		}
	}
	return forward(g, "env", []string{"set", project, "--stdin"}, &lines, os.Stdout)
}

func cmdAdmin(g globals, args []string) error {
	if len(args) == 0 || args[0] != "password" {
		return errors.New("usage: vops admin password")
	}
	if slices.Contains(args, "--stdin") {
		return forward(g, "admin", args, os.Stdin, os.Stdout)
	}
	pw, err := readSecret("new dashboard password: ")
	if err != nil {
		return err
	}
	if isTerminal(os.Stdin) {
		again, err := readSecret("again: ")
		if err != nil {
			return err
		}
		if again != pw {
			return errors.New("passwords differ")
		}
	}
	return forward(g, "admin", []string{"password", "--stdin"}, strings.NewReader(pw+"\n"), os.Stdout)
}

// ---- ui

func cmdUI(g globals, args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9984", "")
	remoteAddr := fs.String("remote", "127.0.0.1:9984", "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	r, err := g.remote()
	if err != nil {
		return err
	}
	url := "http://" + *listen
	if r == nil {
		fmt.Println(url)
		return nil
	}
	cmd := exec.Command(sshBin(), append(r.sshOpts(), "-N", "-o", "ExitOnForwardFailure=yes", "-L", *listen+":"+*remoteAddr, r.Host)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	time.Sleep(time.Second)
	fmt.Printf("dashboard at %s (ctrl-c to close the tunnel)\n", url)
	exec.Command("xdg-open", url).Start()
	return cmd.Wait()
}

// ---- host side

func cmdDaemon() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	vh, repo := daemon.Paths(home)
	d, err := daemon.New(vh, repo)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return d.Run(ctx)
}
