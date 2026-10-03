package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretsRecipients(t *testing.T) {
	c, err := Parse([]byte("secrets:\n  recipients: [\"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl alice\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	empty, _ := Parse([]byte("secrets: {recipients: []}\n"))
	if !empty.Equal(Config{}.Defaults()) {
		t.Fatal("an empty list must equal no list (no plan change)")
	}
	if d := strings.Join(Config{}.Defaults().Diff(c), ","); d != "secrets.recipients: none -> [alice]" {
		t.Fatalf("diff: %q", d)
	}
	if _, err := Parse([]byte("secrets: {recipients: [\"bob\"]}\n")); err == nil {
		t.Fatal("a recipient that isn't a key must be refused")
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	WriteLockFile(path, Config{}.Defaults())
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "secrets") {
		t.Fatalf("an empty secrets block must not reach the lock:\n%s", b)
	}
}
