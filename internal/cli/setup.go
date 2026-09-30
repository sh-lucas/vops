package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/daemon"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/store"
)

// The daemon (deploys, registry, dashboard) and the proxy (:80/:443) are two units of the same binary,
// so an upgrade or a crash of the daemon never takes the sites down.
const daemonUnit = `[Unit]
Description=vops daemon (deploys, registry, dashboard)
StartLimitIntervalSec=0
%s
[Service]
Type=notify
ExecStart=%s daemon
Environment=HOME=%s
Restart=always
RestartSec=2
WatchdogSec=60
# containers are not children of the daemon: restarting vops must never stop them
KillMode=process
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

[Install]
WantedBy=%s
`

const proxyUnit = `[Unit]
Description=vops proxy (:80/:443, tls, routes to containers)
StartLimitIntervalSec=0
%s
[Service]
Type=notify
ExecStart=%s proxy
Environment=HOME=%s
Restart=always
RestartSec=1
WatchdogSec=30

[Install]
WantedBy=%s
`

// cmdSetup runs on the host (install calls it over ssh). It is idempotent: running it again upgrades.
func cmdSetup(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	vh, repo := daemon.Paths(home)
	for _, d := range []string{filepath.Join(vh, "bin"), filepath.Join(vh, "certs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	// --config-stdin: the repo's vops.yml becomes the lock the daemon starts from
	if slices.Contains(args, "--config-stdin") {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		cfg, err := config.Parse(b)
		if err != nil {
			return fmt.Errorf("vops.yml: %w", err)
		}
		if err := config.WriteLockFile(daemon.LockPath(vh), cfg); err != nil {
			return err
		}
	}
	say := func(format string, a ...any) { fmt.Printf("  "+format+"\n", a...) }
	fmt.Println("setting up vops on this host")

	// the repo: pushing to it updates the working tree
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		if err := run(home, nil, "git", "init", "-q", "-b", "main", repo); err != nil {
			return err
		}
		say("created git repo %s", repo)
	}
	if err := run(repo, nil, "git", "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		return err
	}
	if err := ensureLine(filepath.Join(repo, ".git", "info", "exclude"), "/registry/data/"); err != nil {
		return err
	}
	os.MkdirAll(filepath.Join(repo, "registry", "data"), 0o755)

	// checks
	ctx := context.Background()
	warn := func(format string, a ...any) { fmt.Printf("  ! "+format+"\n", a...) }
	if v, err := podman.Run(ctx, "version", "--format", "{{.Client.Version}}"); err != nil {
		return fmt.Errorf("podman does not work: %w", err)
	} else {
		say("podman %s", v)
		if major, _ := strconv.Atoi(strings.Split(v, ".")[0]); major < 4 {
			warn("podman %s is old, vops needs 4.0+", v)
		}
	}
	if b, _ := podman.Run(ctx, "info", "--format", "{{.Host.NetworkBackend}}"); b != "netavark" {
		warn("podman uses the %q network backend: containers can't resolve each other by name. switch to netavark (containers.conf: network_backend = \"netavark\", then podman system reset)", b)
	}
	if fsType, _ := output(home, "stat", "-f", "-c", "%T", repo); fsType != "btrfs" {
		warn("%s is not on btrfs: snapshots and rollback are off (everything else works)", repo)
	} else if _, err := exec.LookPath("btrfs"); err != nil {
		warn("btrfs-progs is missing: install it for snapshots and rollback")
	} else {
		say("btrfs: snapshots and rollback enabled")
	}

	// the first admin (a user row: dashboard login and podman login)
	db, err := store.Open(filepath.Join(vh, "vops.db"), filepath.Join(vh, "secret.key"))
	if err != nil {
		return err
	}
	if !db.HasAdmin() {
		pw := store.Token()[:24]
		if err := db.PutUser(store.User{Name: "admin", Role: store.Admin}, pw); err != nil {
			return err
		}
		fmt.Printf("\n  dashboard login: admin / %s\n  (shown once; change it with `vops user password admin`)\n\n", pw)
	}
	db.Close()

	if os.Getenv("VOPS_NO_SYSTEMD") != "" {
		say("VOPS_NO_SYSTEMD set: not installing the service")
		return nil
	}
	return installUnit(vh, say, warn)
}

func installUnit(vh string, say, warn func(string, ...any)) error {
	home := filepath.Dir(vh)
	u := units{vh: vh, home: home, say: say, warn: warn}
	var ctl []string
	if os.Getuid() == 0 {
		u.dir, u.after, u.wanted, ctl = "/etc/systemd/system", "After=network-online.target\nWants=network-online.target\n", "multi-user.target", []string{"systemctl"}
	} else {
		u.dir, u.wanted, ctl = filepath.Join(home, ".config", "systemd", "user"), "default.target", []string{"systemctl", "--user"}
		// keep the user manager (and the daemon) running without an open session
		if cur, err := user.Current(); err == nil {
			if err := exec.Command("loginctl", "enable-linger", cur.Username).Run(); err != nil {
				warn("loginctl enable-linger %s failed: vops stops when you log out. run it with sudo", cur.Username)
			}
		}
		if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err == nil {
			if n, _ := strconv.Atoi(strings.TrimSpace(string(b))); n > 80 {
				warn("a non-root user can't listen on 80/443 here. run once:\n      echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/90-vops.conf && sudo sysctl --system")
			}
		}
	}
	u.journal = "journalctl " + strings.Join(ctl[1:], " ")
	u.systemctl = func(args ...string) error {
		out, err := exec.Command(ctl[0], append(ctl[1:], args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %s", strings.Join(ctl, " "), strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	u.syncRoutes = func() error {
		c, err := socketClient()
		if err != nil {
			return err
		}
		resp, err := c.do("POST", "/api/proxy/sync", nil)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	return u.install()
}

// units installs and (re)starts vops.service and vops-proxy.service. The daemon always restarts (new code);
// the proxy only when it isn't running, its unit changed or it speaks another proxy.Version, because
// restarting it is the only thing that interrupts the sites.
type units struct {
	vh, home, dir, after, wanted, journal string
	systemctl                             func(args ...string) error
	syncRoutes                            func() error // the daemon sends its table to the proxy now
	say, warn                             func(string, ...any)
}

func (u units) install() error {
	bin := filepath.Join(u.vh, "bin", "vops")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	running, perr := proxy.NewClient(u.vh).Status(ctx)
	cancel()
	os.MkdirAll(u.dir, 0o755)
	write := func(name, tmpl string) (changed, isNew bool, err error) {
		path := filepath.Join(u.dir, name)
		b := fmt.Appendf(nil, tmpl, u.after, bin, u.home, u.wanted)
		old, rerr := os.ReadFile(path)
		if rerr == nil && bytes.Equal(old, b) {
			return false, false, nil
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return false, false, err
		}
		u.say("wrote %s", path)
		return true, rerr != nil, nil
	}
	if _, _, err := write("vops.service", daemonUnit); err != nil {
		return err
	}
	proxyChanged, proxyNew, err := write("vops-proxy.service", proxyUnit)
	if err != nil {
		return err
	}
	if err := u.systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := u.systemctl("enable", "vops.service", "vops-proxy.service"); err != nil {
		return err
	}
	if proxyNew {
		u.say("the proxy moves to its own service (vops-proxy): sites are down for a few seconds, this once")
	}
	// the daemon first: a daemon from before 1.3 holds :80/:443 until it stops
	if err := u.systemctl("restart", "vops.service"); err != nil {
		return fmt.Errorf("%w\n  see: %s -u vops", err, u.journal)
	}
	if err := u.systemctl("is-active", "--quiet", "vops.service"); err != nil {
		return errors.New("vops.service did not start: " + u.journal + " -u vops")
	}
	u.say("vops.service is running")
	switch {
	case perr != nil || proxyChanged || running.Version != proxy.Version:
		if err := u.systemctl("restart", "vops-proxy.service"); err != nil {
			return fmt.Errorf("%w\n  see: %s -u vops-proxy", err, u.journal)
		}
		if err := u.systemctl("is-active", "--quiet", "vops-proxy.service"); err != nil {
			return errors.New("vops-proxy.service did not start: " + u.journal + " -u vops-proxy")
		}
		u.say("vops-proxy.service (re)started (proxy v%d)", proxy.Version)
	default:
		u.say("vops-proxy.service kept running (proxy v%d unchanged): sites were not interrupted", proxy.Version)
	}
	var serr error
	for range 10 { // the daemon may still be starting its containers after a reboot-like start
		if serr = u.syncRoutes(); serr == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	u.warn("the daemon could not send its routes to the proxy yet (%v): it retries every second", serr)
	return nil
}
