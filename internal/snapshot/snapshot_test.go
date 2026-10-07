package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/testenv"
)

// Real btrfs, as a normal user, with files owned by a subuid (like a postgres volume in rootless podman).
func TestSnapshotRestoreDelete(t *testing.T) {
	testenv.Podman(t)
	cache, _ := os.UserCacheDir()
	dir, err := os.MkdirTemp(filepath.Join(cache, "vops-test"), "snap")
	if err != nil {
		t.Fatal(err)
	}
	if !Supported(dir) {
		t.Skip("not on btrfs")
	}
	ctx := context.Background()
	t.Cleanup(func() { Delete(ctx, dir) })

	data := filepath.Join(dir, "vol", "_data")
	os.MkdirAll(data, 0o750)
	if created, err := EnsureSubvolume(ctx, data); err != nil || !created || !IsSubvolume(data) {
		t.Fatalf("ensure: %v %v", created, err)
	}
	if st, _ := os.Stat(data); st.Mode().Perm() != 0o750 {
		t.Fatalf("mode not kept: %v", st.Mode())
	}
	// write as a subuid, like a container user would
	write := func(content string) {
		t.Helper()
		if _, err := podman.Run(ctx, "unshare", "sh", "-c", "echo "+content+" > "+data+"/f && chown -R 999:999 "+data); err != nil {
			t.Fatal(err)
		}
	}
	// files belong to a subuid, so read them from inside the namespace too
	cat := func(p string) string {
		out, _ := podman.Run(ctx, "unshare", "cat", p)
		return out
	}
	read := func() string { return cat(filepath.Join(data, "f")) }
	write("A")
	snapA := filepath.Join(dir, "snaps", "1", "vol")
	if err := Take(ctx, data, snapA); err != nil {
		t.Fatal(err)
	}
	write("B")
	if err := Restore(ctx, snapA, data); err != nil {
		t.Fatal(err)
	}
	if read() != "A" || !IsSubvolume(data) {
		t.Fatalf("after restore: %q", read())
	}
	// restored copy is writable, the snapshot stays read-only and intact
	write("C")
	if b := cat(filepath.Join(snapA, "f")); b != "A" {
		t.Fatalf("snapshot changed: %q", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(data))
	if len(entries) != 1 {
		t.Fatalf("restore left things behind: %v", entries)
	}
	if err := Delete(ctx, snapA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapA); !os.IsNotExist(err) {
		t.Fatal("snapshot not deleted")
	}
	// a non-empty plain dir is never touched
	plain := filepath.Join(dir, "plain")
	os.MkdirAll(plain, 0o755)
	os.WriteFile(filepath.Join(plain, "x"), []byte("x"), 0o644)
	if created, err := EnsureSubvolume(ctx, plain); err != nil || created || IsSubvolume(plain) {
		t.Fatalf("plain dir converted: %v %v", created, err)
	}
	// nor is a bind-mounted file
	file := filepath.Join(dir, "app.db")
	os.WriteFile(file, []byte("data"), 0o644)
	if created, err := EnsureSubvolume(ctx, file); err != nil || created {
		t.Fatalf("file converted: %v %v", created, err)
	}
	if b, _ := os.ReadFile(file); string(b) != "data" {
		t.Fatalf("file changed: %q", b)
	}
	if err := Take(ctx, plain, filepath.Join(dir, "nope")); err == nil {
		t.Fatal("snapshot of a plain dir should fail")
	}
}
