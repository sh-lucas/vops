package daemon

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/secrets"
	"golang.org/x/crypto/ssh"
)

func TestSecretsBackup(t *testing.T) {
	d := testDaemon(t)
	blobPath := filepath.Join(d.Home, backupFile)
	read := func() string { b, _ := os.ReadFile(blobPath); return string(b) }
	info := func() BackupInfo {
		t.Helper()
		i, err := d.backupInfo("", true)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}

	// no secrets, no backup; secrets but nobody to encrypt to: refused, loudly
	if i := info(); i.SHA != "" || i.Warning != "" || i.Repo != "none" {
		t.Fatalf("empty host: %+v", i)
	}
	d.DB.SetEnv("shop", "DB", "hunter2")
	if i := info(); i.SHA != "" || !strings.Contains(i.Warning, "no ssh key can decrypt") || len(d.backupWarnings()) != 1 {
		t.Fatalf("zero recipients: %+v %v", i, d.backupWarnings())
	}

	// the vops user's keys (an ecdsa one skipped) and vops.yml's
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pk, _ := ssh.NewPublicKey(pub)
	os.MkdirAll(filepath.Join(filepath.Dir(d.Home), ".ssh"), 0o700)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	epk, _ := ssh.NewPublicKey(&ec.PublicKey)
	ecLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(epk))) + " old-laptop\n"
	os.WriteFile(filepath.Join(filepath.Dir(d.Home), ".ssh", "authorized_keys"), append(ssh.MarshalAuthorizedKey(pk), ecLine...), 0o600)
	id, _ := age.GenerateX25519Identity()
	d.cfg.Store(&config.Config{Secrets: config.Secrets{Recipients: []string{id.Recipient().String() + " teammate"}}})
	d.DB.SetEnv("shop@*", "STRIPE", "sk_test")
	i := info()
	if i.SHA == "" || i.Warning != "" || len(i.Recipients) != 2 || len(i.Skipped) != 1 || i.Skipped[0].Comment != "old-laptop" || i.Secrets != 2 || i.Repo != "behind" {
		t.Fatalf("backup: %+v", i)
	}
	plain, err := secrets.Decrypt([]byte(i.Blob), []age.Identity{id})
	if err != nil || !strings.Contains(string(plain), "[shop previews]\nSTRIPE=\"sk_test\"") || !strings.Contains(string(plain), "DB=\"hunter2\"") {
		t.Fatalf("decrypt: %v\n%s", err, plain)
	}

	// nothing changed: same blob (no git churn); a change of content or recipients: a new one
	first := read()
	d.refreshBackup()
	if read() != first {
		t.Fatal("re-encrypted without a change")
	}
	d.DB.SetEnv("shop", "DB", "hunter3")
	d.refreshBackup()
	second := read()
	if second == first {
		t.Fatal("a changed value must re-encrypt")
	}
	d.cfg.Store(&config.Config{})
	d.refreshBackup()
	if read() == second {
		t.Fatal("a removed recipient must re-encrypt")
	}
	if fp := d.DB.Meta(metaBackup); fp == "" || strings.Contains(fp, "hunter") {
		t.Fatalf("fingerprint: %q", fp)
	}

	// every authorized key gone: the previous backup stays
	os.Remove(filepath.Join(filepath.Dir(d.Home), ".ssh", "authorized_keys"))
	kept := read()
	d.DB.SetEnv("shop", "NEW", "x")
	if i := info(); read() != kept || !strings.Contains(i.Warning, "keeping the previous backup") {
		t.Fatalf("zero recipients must keep the previous backup: %+v", i)
	}

	// the copy in git: current, behind (one of ours, older), foreign (another host's)
	os.MkdirAll(d.Repo, 0o755)
	repoFile := filepath.Join(d.Repo, secrets.File)
	os.WriteFile(repoFile, []byte(kept), 0o644)
	if i := info(); i.Repo != "current" {
		t.Fatalf("repo: %s", i.Repo)
	}
	os.WriteFile(repoFile, []byte(first), 0o644)
	if i, _ := d.backupInfo(sha([]byte(first)), false); i.Repo != "behind" || !i.Known {
		t.Fatalf("repo: %s known %v", i.Repo, i.Known)
	}
	os.WriteFile(repoFile, []byte("another host"), 0o644)
	if i, _ := d.backupInfo(sha([]byte("another host")), false); i.Repo != "foreign" || i.Known {
		t.Fatalf("repo: %s known %v", i.Repo, i.Known)
	}
}

func TestRestoreOnlyMissing(t *testing.T) {
	d := testDaemon(t)
	d.DB.SetEnv("shop", "DB", "on-the-host")
	restored, kept, err := d.restoreEnv([]secrets.Entry{
		{Project: "shop", Key: "DB", Value: "from-backup"},
		{Project: "shop", Key: "API", Value: "a"},
		{Project: "shop", Preview: true, Key: "DB", Value: "p"},
	})
	if err != nil || strings.Join(restored, ",") != "shop API,shop previews DB" || strings.Join(kept, ",") != "shop DB" {
		t.Fatalf("restored %v kept %v err %v", restored, kept, err)
	}
	env, _ := d.DB.Env("shop")
	pv, _ := d.DB.Env("shop@*")
	if env["DB"] != "on-the-host" || env["API"] != "a" || pv["DB"] != "p" {
		t.Fatalf("env %v previews %v", env, pv)
	}
	if _, _, err := d.restoreEnv([]secrets.Entry{{Project: "x@y", Key: "K"}}); err == nil {
		t.Fatal("a bad project must be refused")
	}
}
