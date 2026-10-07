// Package snapshot is the btrfs layer: subvolumes for data dirs, read-only snapshots of them, and
// restoring a snapshot in place. It knows nothing about projects or the db; deploy builds on it.
//
// Rootless: files in volumes belong to the user's subuids, and btrfs only lets the owner snapshot a
// subvolume or delete it. So every operation runs inside `podman unshare` (the same user namespace
// podman uses) unless we are root. No root, no special mount options.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
)

const btrfsMagic = 0x9123683E

// Supported reports whether path (or its closest existing parent) is on btrfs and the btrfs tool exists.
func Supported(path string) bool {
	if _, err := exec.LookPath("btrfs"); err != nil {
		return false
	}
	for p := path; ; p = filepath.Dir(p) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err == nil {
			return st.Type == btrfsMagic
		}
		if p == "/" || p == "." {
			return false
		}
	}
}

// IsSubvolume: on btrfs the root directory of every subvolume has inode 256.
func IsSubvolume(path string) bool {
	var st syscall.Stat_t
	return Supported(path) && syscall.Stat(path, &st) == nil && st.Ino == 256
}

// Command is a command run as the owner of container files: directly as root, in `podman unshare` otherwise.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if os.Getuid() != 0 {
		args = append([]string{"unshare", name}, args...)
		name = podman.Bin()
	}
	return exec.CommandContext(ctx, name, args...)
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := Command(ctx, name, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "cross-device") || strings.Contains(msg, "Invalid cross-device link") {
			msg += " (snapshots must live on the same btrfs filesystem as the data)"
		}
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return nil
}

// EnsureSubvolume makes path an empty subvolume if it doesn't exist or is an empty directory.
// A non-empty plain directory or anything else (a bind-mounted file) is left alone (created reports false):
// vops never moves existing data.
func EnsureSubvolume(ctx context.Context, path string) (created bool, err error) {
	if IsSubvolume(path) || !Supported(path) {
		return false, nil
	}
	mode := os.FileMode(0o755)
	if st, err := os.Stat(path); err == nil {
		if !st.IsDir() {
			return false, nil
		}
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) > 0 {
			return false, err
		}
		mode = st.Mode().Perm()
		if err := os.Remove(path); err != nil {
			return false, err
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	// created as ourselves, so it has the same owner the plain directory had
	if out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "create", path).CombinedOutput(); err != nil {
		os.Mkdir(path, mode)
		return false, fmt.Errorf("btrfs subvolume create %s: %s", path, strings.TrimSpace(string(out)))
	}
	return true, os.Chmod(path, mode)
}

// Create makes path a new empty subvolume (its parent is created if missing).
func Create(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "create", path).CombinedOutput(); err != nil {
		return fmt.Errorf("btrfs subvolume create %s: %s", path, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReadOnly makes a subvolume read-only, like the snapshots Take makes.
func ReadOnly(ctx context.Context, path string) error {
	return run(ctx, "btrfs", "property", "set", "-ts", path, "ro", "true")
}

// Take creates a read-only snapshot of the subvolume src at dst.
func Take(ctx context.Context, src, dst string) error {
	if !IsSubvolume(src) {
		return fmt.Errorf("%s is not a btrfs subvolume", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return run(ctx, "btrfs", "subvolume", "snapshot", "-r", src, dst)
}

// Restore makes target a writable copy of the snapshot snap. The current target is moved aside first
// and put back if anything fails, so a failed restore leaves the data as it was.
func Restore(ctx context.Context, snap, target string) error {
	if !IsSubvolume(snap) {
		return fmt.Errorf("snapshot %s is missing", snap)
	}
	aside := ""
	if _, err := os.Lstat(target); err == nil {
		aside = target + ".vops-old-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		if err := run(ctx, "mv", target, aside); err != nil {
			return err
		}
	} else if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := run(ctx, "btrfs", "subvolume", "snapshot", snap, target); err != nil {
		if aside != "" {
			run(ctx, "mv", aside, target)
		}
		return err
	}
	if aside != "" {
		return Delete(ctx, aside)
	}
	return nil
}

// Delete removes a subvolume (read-only or not), or a directory tree containing some. Unprivileged btrfs
// can't "subvolume delete", but it can make one writable, empty it and rmdir it (linux 4.18+).
func Delete(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if Supported(path) {
		filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && IsSubvolume(p) {
				run(ctx, "btrfs", "property", "set", "-ts", p, "ro", "false")
			}
			return nil
		})
	}
	return run(ctx, "rm", "-rf", path)
}
