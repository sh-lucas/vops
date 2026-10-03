package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/secrets"
)

// The secrets backup: the host keeps the latest blob in ~/.vops/secrets.age and the laptop commits it
// as .secrets.age (the host never commits). Re-encrypted only when the secrets or the recipients
// change (age output is random: anything else would be git churn).
const (
	backupFile = "secrets.age"
	knownFile  = "secrets.known"              // sha256 of every blob this host wrote: the repo's copy is ours, just older
	rootKeys   = "root_authorized_keys"       // /root/.ssh/authorized_keys as of the last `vops install`
	metaBackup = "secrets_backup_fingerprint" // HMAC of plaintext + recipients
)

// RootKeysPath is where `vops setup` snapshots root's authorized_keys (the daemon can't read /root).
func RootKeysPath(vopsHome string) string { return filepath.Join(vopsHome, rootKeys) }

// BackupInfo is what /api/secrets says about the backup. Names only, never values.
type BackupInfo struct {
	File       string              `json:"file"`
	Secrets    int                 `json:"secrets"`
	Recipients []secrets.Recipient `json:"recipients"`
	Skipped    []secrets.Recipient `json:"skipped"`
	Warning    string              `json:"warning,omitempty"`
	SHA        string              `json:"sha,omitempty"` // sha256 of the current blob, "" = none yet
	UpdatedAt  int64               `json:"updated_at,omitempty"`
	Repo       string              `json:"repo"`          // the copy in git (~/vops): none | current | behind | foreign
	Env        []secrets.Entry     `json:"env,omitempty"` // names of what the host has (for restore)
	Known      bool                `json:"known"`         // ?have=<sha> is a blob this host wrote
	Blob       string              `json:"blob,omitempty"`
}

type backup struct {
	mu   sync.Mutex
	info BackupInfo
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// recipients: the vops user's authorized_keys (read live: who can deploy), root's snapshot, vops.yml.
func (d *Daemon) recipients() (ok, skipped []secrets.Recipient) {
	add := func(o, s []secrets.Recipient) { ok, skipped = append(ok, o...), append(skipped, s...) }
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(d.Home), ".ssh", "authorized_keys")); err == nil {
		add(secrets.ParseAuthorizedKeys(b, secrets.FromUser))
	}
	if b, err := os.ReadFile(RootKeysPath(d.Home)); err == nil {
		add(secrets.ParseAuthorizedKeys(b, secrets.FromRoot))
	}
	for _, s := range d.cfg.Load().Secrets.Recipients {
		if r := secrets.ParseRecipient(s, secrets.FromConfig); r.Reason == "" {
			ok = append(ok, r)
		} else {
			skipped = append(skipped, r)
		}
	}
	return secrets.Dedupe(ok), secrets.Dedupe(skipped)
}

// refreshBackup re-encrypts when the secrets or the recipients changed. Never writes a backup nobody can
// open: the previous one stays and the warning says why.
func (d *Daemon) refreshBackup() error {
	d.backup.mu.Lock()
	defer d.backup.mu.Unlock()
	all, err := d.DB.AllEnv()
	if err != nil {
		return err
	}
	var entries []secrets.Entry
	for _, v := range all {
		project, preview := strings.CutSuffix(v.Scope, "@*")
		entries = append(entries, secrets.Entry{Project: project, Preview: preview, Key: v.Key, Value: v.Value})
	}
	ok, skipped := d.recipients()
	info := BackupInfo{File: secrets.File, Secrets: len(entries), Recipients: ok, Skipped: skipped}
	for _, e := range entries {
		info.Env = append(info.Env, secrets.Entry{Project: e.Project, Preview: e.Preview, Key: e.Key})
	}
	path := filepath.Join(d.Home, backupFile)
	defer func() {
		if st, err := os.Stat(path); err == nil {
			b, _ := os.ReadFile(path)
			info.SHA, info.UpdatedAt = sha(b), st.ModTime().Unix()
		}
		d.backup.info = info
	}()
	_, statErr := os.Stat(path)
	if len(entries) == 0 && statErr != nil {
		return nil // nothing to back up, and never was
	}
	plain := secrets.Marshal(entries)
	keys := []string{}
	for _, r := range ok {
		keys = append(keys, r.Key)
	}
	slices.Sort(keys)
	fp := d.DB.Fingerprint(append(append(plain, 0), strings.Join(keys, "\n")...))
	if len(ok) == 0 {
		info.Warning = "no ssh key can decrypt the secrets backup (" + secrets.File + "): add an ed25519 or rsa key to ~/.ssh/authorized_keys on the host, or secrets.recipients in vops.yml"
		if statErr == nil {
			info.Warning += "; keeping the previous backup"
		}
		return nil
	}
	if statErr == nil && d.DB.Meta(metaBackup) == fp {
		return nil
	}
	blob, err := secrets.Encrypt(plain, ok)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d.Home, knownFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fmt.Fprintln(f, sha(blob))
	f.Close()
	if err := os.WriteFile(path+".tmp", blob, 0o600); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	return d.DB.SetMeta(metaBackup, fp)
}

// backupNow refreshes after an env write; a failure shows up in /api/status, the write stands.
func (d *Daemon) backupNow() {
	if err := d.refreshBackup(); err != nil {
		log.Printf("secrets backup: %v", err)
	}
}

// backupInfo refreshes and describes the backup; have is the sha of a copy the caller holds.
func (d *Daemon) backupInfo(have string, blob bool) (BackupInfo, error) {
	err := d.refreshBackup()
	d.backup.mu.Lock()
	info := d.backup.info
	d.backup.mu.Unlock()
	if info.Recipients == nil {
		info.Recipients = []secrets.Recipient{}
	}
	if info.Skipped == nil {
		info.Skipped = []secrets.Recipient{}
	}
	known := func(s string) bool {
		if s == "" {
			return false
		}
		if s == info.SHA {
			return true
		}
		b, _ := os.ReadFile(filepath.Join(d.Home, knownFile))
		return slices.Contains(strings.Fields(string(b)), s)
	}
	info.Known = known(have)
	info.Repo = "none"
	if b, rerr := os.ReadFile(filepath.Join(d.Repo, secrets.File)); rerr == nil {
		switch s := sha(b); {
		case s == info.SHA:
			info.Repo = "current"
		case known(s):
			info.Repo = "behind"
		default:
			info.Repo = "foreign"
		}
	} else if info.SHA != "" {
		info.Repo = "behind"
	}
	if blob && info.SHA != "" {
		b, rerr := os.ReadFile(filepath.Join(d.Home, backupFile))
		if rerr != nil {
			return info, rerr
		}
		info.Blob = string(b)
	}
	return info, err
}

// backupWarnings go to /api/status (dashboard overview, `vops status`).
func (d *Daemon) backupWarnings() []string {
	info, err := d.backupInfo("", false)
	var out []string
	if err != nil {
		out = append(out, "secrets backup: "+err.Error())
	}
	if info.Warning != "" && info.Secrets > 0 {
		out = append(out, info.Warning)
	}
	switch info.Repo {
	case "behind":
		out = append(out, secrets.File+" in git is behind the host's secrets: run `vops sync` to save it to git")
	case "foreign":
		out = append(out, secrets.File+" in git isn't a backup this host wrote: `vops sync` offers to restore its secrets here (or `vops env restore`)")
	}
	return out
}

// restoreEnv sets the entries whose key is missing on the host; existing ones are never overwritten.
func (d *Daemon) restoreEnv(entries []secrets.Entry) (restored, kept []string, err error) {
	for _, e := range entries {
		scope := e.Project
		if e.Preview {
			scope = deploy.PreviewEnvScope(e.Project)
		}
		if _, err := envScope(e.Project, false); err != nil {
			return restored, kept, err
		}
		if !envKeyRe.MatchString(e.Key) {
			return restored, kept, fmt.Errorf("invalid key %q", e.Key)
		}
		set, err := d.DB.SetEnvIfMissing(scope, e.Key, e.Value)
		if err != nil {
			return restored, kept, err
		}
		if set {
			restored = append(restored, e.ID())
			d.DB.Event(e.Project, "config", "%s %s restored from %s", envLabel(e.Preview), e.Key, secrets.File)
		} else {
			kept = append(kept, e.ID())
		}
	}
	return restored, kept, d.refreshBackup()
}
