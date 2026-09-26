package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/daemon"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/store"
)

const unitTemplate = `[Unit]
Description=vops daemon
StartLimitIntervalSec=0
%s
[Service]
ExecStart=%s daemon
Environment=HOME=%s
Restart=always
RestartSec=2
# containers are not children of the daemon: restarting vops must never stop them
KillMode=process
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

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
		warn("filesystem is %s: snapshots and rollback need btrfs (everything else works)", fsType)
	} else if _, err := exec.LookPath("btrfs"); err != nil {
		warn("btrfs-progs is missing: install it for snapshots and rollback")
	} else {
		say("btrfs: snapshots and rollback enabled")
	}

	// dashboard password
	db, err := store.Open(filepath.Join(vh, "vops.db"), filepath.Join(vh, "secret.key"))
	if err != nil {
		return err
	}
	if !db.HasAdmin() {
		pw := store.Token()[:24]
		if err := db.SetAdminPassword(pw); err != nil {
			return err
		}
		fmt.Printf("\n  dashboard login: admin / %s\n  (shown once; change it with `vops admin password`)\n\n", pw)
	}
	db.Close()

	if os.Getenv("VOPS_NO_SYSTEMD") != "" {
		say("VOPS_NO_SYSTEMD set: not installing the service")
		return nil
	}
	return installUnit(vh, say, warn)
}

func installUnit(vh string, say, warn func(string, ...any)) error {
	bin := filepath.Join(vh, "bin", "vops")
	root := os.Getuid() == 0
	var unitPath string
	var ctl []string
	if root {
		unitPath = "/etc/systemd/system/vops.service"
		ctl = []string{"systemctl"}
		if err := os.WriteFile(unitPath, fmt.Appendf(nil, unitTemplate, "After=network-online.target\nWants=network-online.target\n", bin, filepath.Dir(vh), "multi-user.target"), 0o644); err != nil {
			return err
		}
	} else {
		home, _ := os.UserHomeDir()
		unitPath = filepath.Join(home, ".config", "systemd", "user", "vops.service")
		ctl = []string{"systemctl", "--user"}
		os.MkdirAll(filepath.Dir(unitPath), 0o755)
		if err := os.WriteFile(unitPath, fmt.Appendf(nil, unitTemplate, "", bin, home, "default.target"), 0o644); err != nil {
			return err
		}
		// keep the user manager (and the daemon) running without an open session
		if u, err := user.Current(); err == nil {
			if err := exec.Command("loginctl", "enable-linger", u.Username).Run(); err != nil {
				warn("loginctl enable-linger %s failed: the daemon stops when you log out. run it with sudo", u.Username)
			}
		}
		if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err == nil {
			if n, _ := strconv.Atoi(strings.TrimSpace(string(b))); n > 80 {
				warn("a non-root user can't listen on 80/443 here. run once:\n      echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/90-vops.conf && sudo sysctl --system")
			}
		}
	}
	say("wrote %s", unitPath)
	systemctl := func(args ...string) error {
		out, err := exec.Command(ctl[0], append(ctl[1:], args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %s", strings.Join(ctl, " "), strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "vops.service"); err != nil {
		return err
	}
	if err := systemctl("restart", "vops.service"); err != nil {
		return err
	}
	time.Sleep(2 * time.Second) // a daemon that dies at startup is still "active" for a moment
	if err := systemctl("is-active", "--quiet", "vops.service"); err != nil {
		return errors.New("vops.service did not start: journalctl " + strings.Join(ctl[1:], " ") + " -u vops")
	}
	say("vops.service is running")
	return nil
}
