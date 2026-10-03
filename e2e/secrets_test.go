package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// devKey gives the developer of w an ed25519 key in ~/.ssh and authorizes it on w's host, like a real
// `ssh-copy-id`; a second world reuses the same private key (the same person, a new host).
func (w *world) devKey(priv ed25519.PrivateKey) {
	w.t.Helper()
	devHome := strings.TrimPrefix(w.env[len(w.env)-1], "HOME=")
	block, _ := ssh.MarshalPrivateKey(priv, "dev@laptop")
	os.MkdirAll(filepath.Join(devHome, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(devHome, ".ssh", "id_ed25519"), pem.EncodeToMemory(block), 0o600)
	pk, _ := ssh.NewPublicKey(priv.Public())
	os.MkdirAll(filepath.Join(w.hostHome, ".ssh"), 0o700)
	line := `no-pty,from="127.0.0.1" ` + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) + " dev@laptop\n"
	os.WriteFile(filepath.Join(w.hostHome, ".ssh", "authorized_keys"), []byte(line), 0o600)
}

// hostAPI calls the host daemon's socket directly, like the dashboard would.
func (w *world) hostAPI(method, path, body string) {
	w.t.Helper()
	sock := filepath.Join(w.hostHome, ".vops", "vops.sock")
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequest(method, "http://vops"+path, strings.NewReader(body))
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		w.t.Fatalf("%s %s: %v %v", method, path, err, resp)
	}
	resp.Body.Close()
}

// The secrets backup: committed by the cli (only that file), kept out of deploys, and restored on a new host.
func TestSecretsBackup(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := setup(t)
	w.devKey(priv)
	dev := filepath.Join(t.TempDir(), "infra")
	os.MkdirAll(dev, 0o755)
	w.vops(dev, "init", "--domain", "vops.test")
	w.setConfig(dev, map[string]string{"ui": "off"}) // no listener: a daemon without proxy or ports is enough here
	os.WriteFile(filepath.Join(dev, ".gitignore"), []byte(".*\n!.gitignore\nvops-lock.yml\n"), 0o644)
	w.git(dev, "add", "-A")
	w.git(dev, "commit", "-q", "-m", "init")
	w.vops(dev, "install", "dev@fakehost", "--binary", w.bin)
	w.startProc("daemon")

	// env set commits the backup, and only it: what the user staged stays staged
	os.WriteFile(filepath.Join(dev, "staged.txt"), []byte("wip"), 0o644)
	w.git(dev, "add", "staged.txt")
	out := w.vops(dev, "env", "set", "shop", "MSG=hello", "PEM=line1")
	if !strings.Contains(out, "secrets backup updated: .secrets.age (2 secrets, 1 keys) — committed") || !strings.Contains(out, "!/.secrets.age") {
		t.Fatalf("env set:\n%s", out)
	}
	if files := w.git(dev, "show", "--name-only", "--format=", "HEAD"); strings.TrimSpace(files) != ".secrets.age" {
		t.Fatalf("the backup commit has: %s", files)
	}
	if staged := w.git(dev, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "staged.txt" {
		t.Fatalf("staged changes were touched: %q", staged)
	}
	w.git(dev, "reset", "-q", "staged.txt")
	os.Remove(filepath.Join(dev, "staged.txt"))
	if b, _ := os.ReadFile(filepath.Join(dev, ".secrets.age")); !strings.HasPrefix(string(b), "-----BEGIN AGE ENCRYPTED FILE-----") || strings.Contains(string(b), "hello") {
		t.Fatalf("backup:\n%s", b)
	}
	if out := w.vops(dev, "env", "recipients"); !strings.Contains(out, "dev@laptop") || !strings.Contains(out, "vops user") || !strings.Contains(out, "you can open it with") {
		t.Fatalf("recipients:\n%s", out)
	}

	// sync pushes it and has nothing to deploy; a second sync changes nothing
	if out := w.vops(dev, "sync", "--yes"); !strings.Contains(out, "nothing to do") || strings.Contains(out, "updated") {
		t.Fatalf("sync:\n%s", out)
	}
	head := w.git(dev, "rev-parse", "HEAD")
	w.vops(dev, "sync", "--yes")
	if w.git(dev, "rev-parse", "HEAD") != head {
		t.Fatal("a sync without changes committed something")
	}

	// a dashboard change: the host says git is behind, the next sync saves it
	w.hostAPI("POST", "/api/env", `{"project":"shop","key":"STRIPE","value":"sk_live","preview":true}`)
	if out := w.vops(dev, "status"); !strings.Contains(out, ".secrets.age in git is behind") {
		t.Fatalf("status:\n%s", out)
	}
	if out := w.vops(dev, "sync", "--yes"); !strings.Contains(out, "(3 secrets, 1 keys) — committed") {
		t.Fatalf("sync after a dashboard change:\n%s", out)
	}
	if out := w.vops(dev, "status"); strings.Contains(out, ".secrets.age") {
		t.Fatalf("status after sync:\n%s", out)
	}

	// the host is gone: a new one (same person, same key), one secret set there already
	w2 := setup(t)
	w2.devKey(priv)
	w2.vops(dev, "install", "dev@fakehost", "--binary", w2.bin)
	w2.startProc("daemon")
	w2.hostAPI("POST", "/api/env", `{"project":"shop","key":"MSG","value":"new-host"}`)
	if out, _ := w2.try(dev, "", "env", "set", "shop", "OTHER=1"); !strings.Contains(out, "isn't a backup this host wrote: left as is") {
		t.Fatalf("env set on a new host must not replace the old backup:\n%s", out)
	}
	before, _ := os.ReadFile(filepath.Join(dev, ".secrets.age"))
	out = w2.vops(dev, "sync", "--yes")
	if !strings.Contains(out, "restored 2 secret(s): shop PEM, shop previews STRIPE") || !strings.Contains(out, "kept as is: shop MSG") || !strings.Contains(out, "(4 secrets, 1 keys) — committed") {
		t.Fatalf("restore on sync:\n%s", out)
	}
	if after, _ := os.ReadFile(filepath.Join(dev, ".secrets.age")); string(after) == string(before) {
		t.Fatal("the backup was not updated after the restore")
	}
	if out := w2.vops(dev, "env", "ls", "shop"); !strings.Contains(out, "PEM") || !strings.Contains(out, "OTHER") {
		t.Fatalf("env on the new host:\n%s", out)
	}
	// explicit restore: everything is there already
	if out := w2.vops(dev, "env", "restore"); !strings.Contains(out, "already set on the host, kept as is") || strings.Contains(out, "restored") {
		t.Fatalf("env restore:\n%s", out)
	}
}
