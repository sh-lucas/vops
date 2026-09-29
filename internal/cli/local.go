package cli

import (
	"bytes"
	"context"
	"debug/buildinfo"
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
	"github.com/sh-lucas/vops/internal/proxy"
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
	path := filepath.Join(root, config.RepoFile)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		b = []byte(config.Template(strings.ToLower(*domain), *email))
	case err != nil:
		return err
	case *domain != "" || *email != "":
		fields := map[string]string{}
		if *domain != "" {
			fields["domain"] = strings.ToLower(*domain)
		}
		if *email != "" {
			fields["email"] = *email
		}
		if b, err = config.SetFields(b, fields); err != nil {
			return fmt.Errorf("%s: %w", config.RepoFile, err)
		}
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return fmt.Errorf("%s: %w", config.RepoFile, err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
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
	force := fs.Bool("force", false, "")
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
	if err := r.Shell(`uname -m; for c in podman git systemctl; do command -v $c >/dev/null 2>&1 && echo has-$c; done; `+RemoteBin+` version 2>/dev/null; true`, nil, &probe); err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(probe.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return errors.New("could not probe the host")
	}
	hostVersion := ""
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "vops "); ok {
			hostVersion = strings.TrimSpace(v)
		}
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
		if arch != runtime.GOARCH || runtime.GOOS != "linux" {
			return fmt.Errorf("the host is linux/%s but this vops is %s/%s: get one with `GOOS=linux GOARCH=%s go install github.com/sh-lucas/vops/cmd/vops@latest` and pass --binary $(go env GOPATH)/bin/linux_%s/vops", arch, runtime.GOOS, runtime.GOARCH, arch, arch)
		}
		if bin, err = os.Executable(); err != nil {
			return err
		}
	}
	newVersion := Version
	if *binary != "" {
		newVersion = binaryVersion(bin)
	}
	hv, hok := parseVersion(hostVersion)
	nv, nok := parseVersion(newVersion)
	switch {
	case hostVersion == "":
		fmt.Fprintf(os.Stderr, "installing vops %s\n", newVersion)
	case hostVersion == newVersion:
		fmt.Fprintf(os.Stderr, "reinstalling vops %s\n", newVersion)
	case hok && nok && nv.less(hv) && !*force:
		return &versionError{fmt.Sprintf("the host runs vops %s, this is %s: installing it would downgrade the host. update yours (%s), or pass --force", hostVersion, newVersion, updateCmd)}
	case hok && nok && nv.less(hv):
		fmt.Fprintf(os.Stderr, "downgrading vops %s → %s (--force)\n", hostVersion, newVersion)
	default:
		fmt.Fprintf(os.Stderr, "upgrading vops %s → %s\n", hostVersion, newVersion)
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
	// the host starts from this repo's vops.yml (its lock), so the first start already has the right ports and tls
	var setupIn io.Reader
	setupArgs := []string{"setup"}
	if root := repoRoot(); root != "" {
		if b, err := os.ReadFile(filepath.Join(root, config.RepoFile)); err == nil {
			if _, err := config.Parse(b); err != nil {
				return fmt.Errorf("%s: %w", config.RepoFile, err)
			}
			setupIn, setupArgs = bytes.NewReader(b), append(setupArgs, "--config-stdin")
		}
	}
	if err := r.Run(setupArgs, setupIn, os.Stdout, false); err != nil {
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

// binaryVersion asks a vops binary its version (it may be for another arch: then its build info says).
func binaryVersion(bin string) string {
	if out, err := exec.Command(bin, "version").Output(); err == nil {
		if v, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "vops "); ok {
			return v
		}
	}
	if bi, err := buildinfo.ReadFile(bin); err == nil && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
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
	return applyFlow(g, *yes, head, nil, "sync")
}

// ---- apply

func cmdApply(g globals, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	fs.BoolVar(yes, "y", false, "")
	commit := fs.String("commit", "", "")
	trigger := fs.String("trigger", "apply", "")
	projects, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if *yes && *commit != "" {
		return forward(g, "apply", append([]string{"--yes", "--commit", *commit, "--trigger", *trigger}, projects...), nil, os.Stdout)
	}
	return applyFlow(g, *yes, *commit, projects, *trigger)
}

// applyFlow shows the plan, asks, then applies exactly the commit that was shown. trigger goes to the deploy history.
func applyFlow(g globals, yes bool, commit string, projects []string, trigger string) error {
	var buf bytes.Buffer
	if err := forward(g, "plan", []string{"--json"}, nil, &buf); err != nil {
		return err
	}
	var plan deploy.Plan
	if err := json.Unmarshal(buf.Bytes(), &plan); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if r, _ := g.remote(); r != nil {
		if err := checkPlanVersion(plan.Version); err != nil {
			return err
		}
	}
	if commit != "" && plan.Commit != commit {
		return fmt.Errorf("the host is at %.12s, expected %.12s", plan.Commit, commit)
	}
	plan.Print(os.Stdout)
	if !plan.Changes() {
		// nothing changes, but the host should record that it matches this commit
		return forward(g, "apply", append([]string{"--yes", "--commit", plan.Commit, "--trigger", trigger}, projects...), nil, io.Discard)
	}
	if !yes {
		if !isTerminal(os.Stdin) {
			return errors.New("not a terminal: pass --yes to apply without asking")
		}
		if !confirm("apply?") {
			return errors.New("aborted")
		}
	}
	return forward(g, "apply", append([]string{"--yes", "--commit", plan.Commit, "--trigger", trigger}, projects...), nil, os.Stdout)
}

// ---- env and admin read secrets here and send them over stdin, never on a command line

func cmdEnv(g globals, args []string) error {
	if len(args) < 2 || args[0] != "set" || slices.Contains(args, "--stdin") {
		return forward(g, "env", args, os.Stdin, os.Stdout)
	}
	project, pairs := args[1], args[2:]
	var extra []string
	if i := slices.Index(pairs, "--preview"); i >= 0 {
		pairs, extra = slices.Delete(slices.Clone(pairs), i, i+1), []string{"--preview"}
	}
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
	return forward(g, "env", append([]string{"set", project, "--stdin"}, extra...), &lines, os.Stdout)
}

// ---- previews

// cmdPreview forwards to the host; `up --ref <branch>` of a local branch pushes it to the host first,
// so the worktree there can check it out.
func cmdPreview(g globals, args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	ref := fs.String("ref", "", "")
	fs.String("name", "", "")
	fs.Int64("from", 0, "")
	var images multi
	fs.Var(&images, "image", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	r, err := g.remote()
	if err != nil {
		return err
	}
	root := repoRoot()
	if len(pos) > 0 && pos[0] == "up" && *ref != "" && *ref != "main" && r != nil && root != "" {
		if _, err := output(root, "git", "rev-parse", "--verify", "-q", "refs/heads/"+*ref); err == nil {
			if err := ensureGitRemote(root, r); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "pushing %s to the host\n", *ref)
			if err := run(root, []string{"GIT_SSH_COMMAND=" + r.GitSSH()}, "git", "push", "-q", "-f", "vops", *ref+":refs/heads/"+*ref); err != nil {
				return err
			}
		}
	}
	return forward(g, "preview", args, nil, os.Stdout)
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
	d.Version = Version
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := d.Run(ctx); !errors.Is(err, daemon.ErrRestart) {
		return err
	}
	return reexec() // a new ui listener from vops.yml
}

// cmdProxy runs the proxy process: :80/:443, certificates, the routing table the daemon sends.
func cmdProxy() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	vh, _ := daemon.Paths(home)
	s, err := proxy.NewServer(vh, Version)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := s.Run(ctx); !errors.Is(err, proxy.ErrRestart) {
		return err
	}
	return reexec() // new http/https/tls from vops.yml
}

// reexec becomes a fresh process with the same pid (systemd doesn't notice; the new one sends READY again).
func reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(strings.TrimSuffix(exe, " (deleted)"), os.Args, os.Environ())
}

// ---- rollback: show what it does (per service, the data, the code), ask, then do exactly that

func cmdRollback(g globals, args []string) error {
	if g.local { // the host side of a forwarded rollback: the laptop already showed the plan
		return runHere("rollback", args, nil, os.Stdout)
	}
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	fs.BoolVar(yes, "y", false, "")
	asJSON := fs.Bool("json", false, "")
	images := fs.Bool("images", false, "")
	data := fs.Bool("data", false, "")
	var services multi
	fs.Var(&services, "service", "")
	snap := fs.String("snapshot", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || len(pos) > 2 {
		return errors.New("usage: vops rollback <project> [deploy-id] [--images] [--data] [--service s]... [--snapshot id] [-y]")
	}
	rest := slices.Clone(pos)
	if *images {
		rest = append(rest, "--images")
	}
	if *data {
		rest = append(rest, "--data")
	}
	for _, s := range services {
		rest = append(rest, "--service", s)
	}
	if *snap != "" {
		rest = append(rest, "--snapshot", *snap)
	}
	var buf bytes.Buffer
	if err := forward(g, "rollback", append(slices.Clone(rest), "--plan", "--json"), nil, &buf); err != nil {
		return err
	}
	if *asJSON {
		_, err := os.Stdout.Write(buf.Bytes())
		return err
	}
	var rp deploy.RollbackPlan
	if err := json.Unmarshal(buf.Bytes(), &rp); err != nil {
		return err
	}
	rp.Print(os.Stdout)
	if len(rp.Parts) == 0 {
		return errors.New("nothing to roll back")
	}
	if !*yes {
		if !isTerminal(os.Stdin) {
			return errors.New("not a terminal: pass --yes")
		}
		if !confirm("roll back?") {
			return errors.New("aborted")
		}
	}
	if len(pos) == 1 && rp.Before != nil {
		rest = append(rest, fmt.Sprint(rp.Before.ID)) // exactly the point shown, even if a deploy lands in between
	}
	return forward(g, "rollback", append(rest, "--yes"), nil, os.Stdout)
}
