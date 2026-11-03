package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckStaticRefusesCgoBuilds(t *testing.T) {
	dir := t.TempDir()
	build := func(name, cgo string) string {
		t.Helper()
		out := filepath.Join(dir, name)
		src := filepath.Join(dir, name+".go")
		// net with cgo links the libc resolver: a dynamic binary wherever a C toolchain exists
		os.WriteFile(src, []byte("package main\nimport _ \"net\"\nfunc main() {}\n"), 0o644)
		cmd := exec.Command("go", "build", "-o", out, src)
		cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgo)
		if b, err := cmd.CombinedOutput(); err != nil {
			if cgo == "1" {
				t.Skipf("no cgo toolchain: %s", b)
			}
			t.Fatalf("%s", b)
		}
		return out
	}
	if err := checkStatic(build("static", "0")); err != nil {
		t.Fatalf("static build refused: %v", err)
	}
	if err := checkStatic(build("dynamic", "1")); err == nil || !strings.Contains(err.Error(), "CGO_ENABLED=0") {
		t.Fatalf("cgo build accepted: %v", err)
	}
	if err := checkStatic(filepath.Join(dir, "static.go")); err == nil {
		t.Fatal("a non-elf file was accepted")
	}
}
