package deploy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sh-lucas/vops/internal/compose"
)

// Upgrading vops must not redeploy anything: the hash of an unchanged service stays the same across versions.
// If this fails on purpose (a new field in the hash), say so in the release notes: every service restarts.
func TestServiceHashIsStable(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(`services:
  web:
    image: nginx:alpine
    environment: {A: "1"}
    volumes: ["data:/data"]
    x-vops: {port: 80}
  db:
    image: docker.io/library/postgres:16
volumes:
  data:
`), 0o644)
	proj, err := compose.Load(dir, "shop", []string{"compose.yml"}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	specs, err := proj.Specs("example.com", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"web": "b0650465a495e82c", "db": "1dcfb577922b4f6e"}
	e := &Engine{}
	for _, sp := range specs {
		d, err := e.resolve(context.Background(), dir, sp, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		if d.hash != want[sp.Service] {
			t.Errorf("%s: hash %s, want %s", sp.Service, d.hash, want[sp.Service])
		}
	}
}
