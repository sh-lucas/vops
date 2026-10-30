package cli

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ssh must get "--" before the host, so a host like -oProxyCommand=x is never an option.
func TestSSHHostIsNeverAnOption(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "args")
	script := filepath.Join(dir, "ssh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + rec + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOPS_SSH", script)

	const host = "-oProxyCommand=touch pwned"
	check := func(name string, call func() error) {
		t.Helper()
		os.Remove(rec)
		if err := call(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := os.ReadFile(rec)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
		if i := slices.Index(args, host); i < 1 || args[i-1] != "--" {
			t.Fatalf("%s: want \"--\" right before the host, got %q", name, args)
		}
	}
	check("Shell", func() error { return (&Remote{Host: host}).Shell("true", nil, io.Discard) })
	check("Run", func() error { return (&Remote{Host: host}).Run([]string{"status"}, nil, io.Discard, false) })
}
