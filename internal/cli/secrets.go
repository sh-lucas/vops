package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sh-lucas/vops/internal/daemon"
	"github.com/sh-lucas/vops/internal/secrets"
)

// The laptop side of the secrets backup: the host encrypts, the laptop commits .secrets.age (the host
// repo never commits) and decrypts it for a restore, with the private keys only it has.

func warnf(format string, a ...any) { fmt.Fprintf(os.Stderr, "! "+format+"\n", a...) }

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func backupInfo(g globals, have string) (daemon.BackupInfo, error) {
	var buf bytes.Buffer
	var info daemon.BackupInfo
	if err := forward(g, "env", []string{"backup", "--have", have}, nil, &buf); err != nil {
		return info, err
	}
	return info, json.Unmarshal(buf.Bytes(), &info)
}

type secretsOpts struct {
	sync    bool            // from `vops sync`: offer a restore, check that you can decrypt
	yes     bool            // restore without asking
	entries []secrets.Entry // the repo's copy, already decrypted
}

// syncSecrets brings the host's backup into the repo and commits only that file. A copy in git that this
// host didn't write (a new or reinstalled host) is never replaced while it holds secrets the host lacks:
// sync offers to restore them first.
func syncSecrets(g globals, r *Remote, root string, o secretsOpts) error {
	path := filepath.Join(root, secrets.File)
	repo, rerr := os.ReadFile(path)
	have := ""
	if rerr == nil {
		have = shaHex(repo)
	}
	info, err := backupInfo(g, have)
	if err != nil {
		return err
	}
	if info.Warning != "" && info.Secrets > 0 {
		warnf("%s", info.Warning)
	}
	if rerr == nil && have != info.SHA && !info.Known {
		entries := o.entries
		if entries == nil {
			if !o.sync {
				warnf("%s isn't a backup this host wrote: left as is. `vops sync` (or `vops env restore`) restores its secrets here", secrets.File)
				return nil
			}
			if entries, err = openBackup(repo, r.Key); err != nil {
				return fmt.Errorf("%s isn't a backup this host wrote, so it stays as is until it is restored: %w", secrets.File, err)
			}
		}
		if missing := missingOn(info, entries); len(missing) > 0 {
			if !o.sync {
				warnf("%s holds %d secret(s) this host doesn't have: left as is", secrets.File, len(missing))
				return nil
			}
			if !o.yes {
				if !isTerminal(os.Stdin) {
					return fmt.Errorf("%s holds %d secret(s) this host doesn't have: run `vops env restore` (or sync with -y)", secrets.File, len(missing))
				}
				if !confirmYes(fmt.Sprintf("restore %d secret(s) from %s?", len(missing), secrets.File)) {
					warnf("not restored: %s stays as is (it holds secrets this host doesn't have)", secrets.File)
					return nil
				}
			}
			if err := restoreEntries(g, entries); err != nil {
				return err
			}
			if info, err = backupInfo(g, have); err != nil {
				return err
			}
		}
	}
	if o.sync {
		defer checkMine(info, r.Key)
	}
	if info.SHA == "" || info.SHA == have {
		return nil
	}
	if err := commitBackup(root, []byte(info.Blob)); err != nil {
		return fmt.Errorf("could not commit %s: %w", secrets.File, err)
	}
	fmt.Fprintf(os.Stderr, "secrets backup updated: %s (%d secrets, %d keys) — committed\n", secrets.File, info.Secrets, len(info.Recipients))
	for _, k := range info.Skipped {
		warnf("%s can't be opened by %s %s (%s): %s", secrets.File, k.Type, k.Name(), k.Source, k.Reason)
	}
	return nil
}

func missingOn(info daemon.BackupInfo, entries []secrets.Entry) []secrets.Entry {
	var out []secrets.Entry
	for _, e := range entries {
		if !slices.ContainsFunc(info.Env, func(h secrets.Entry) bool { return h.ID() == e.ID() }) {
			out = append(out, e)
		}
	}
	return out
}

// restoreEntries sends decrypted secrets over ssh stdin; the host sets the missing ones only.
func restoreEntries(g globals, entries []secrets.Entry) error {
	return forward(g, "env", []string{"restore", "--stdin"}, bytes.NewReader(secrets.Marshal(entries)), os.Stdout)
}

// commitBackup writes the file and commits only it (other staged changes stay staged). A .gitignore that
// matches it (`**/.*`) doesn't stop it: it is added with -f, and we say how to stop matching it.
func commitBackup(root string, blob []byte) error {
	if err := os.WriteFile(filepath.Join(root, secrets.File), blob, 0o644); err != nil {
		return err
	}
	add := []string{"add", "--", secrets.File}
	if exec.Command("git", "-C", root, "check-ignore", "-q", "--", secrets.File).Run() == nil {
		add = []string{"add", "-f", "--", secrets.File}
		fmt.Fprintf(os.Stderr, "note: your .gitignore matches %s, added anyway: add `!/%s` to .gitignore\n", secrets.File, secrets.File)
	}
	if err := run(root, nil, "git", add...); err != nil {
		return err
	}
	return run(root, nil, "git", "commit", "-q", "--only", "-m", "vops: update "+secrets.File+" (encrypted backup of env secrets)", "--", secrets.File)
}

// openBackup decrypts with the private keys here: --ssh-key / vops-lock.yml first, then ~/.ssh/id_*.
func openBackup(blob []byte, key string) ([]secrets.Entry, error) {
	keys, notes := localKeys(key)
	plain, err := secrets.Decrypt(blob, secrets.Identities(keys))
	if errors.Is(err, secrets.ErrNoKey) {
		var tried []string
		for _, k := range keys {
			tried = append(tried, k.Path)
		}
		msg := "no ssh private key found here"
		if len(tried) > 0 {
			msg = "none of your ssh keys can open it (tried " + strings.Join(tried, ", ") + ")"
		}
		if os.Getenv("SSH_AUTH_SOCK") != "" {
			msg += "; a key that is only in ssh-agent can't decrypt (age needs the key file)"
		}
		msg += ": pass --ssh-key path/to/private_key"
		for _, n := range notes {
			msg += "\n  " + n
		}
		return nil, errors.New(msg)
	}
	if err != nil {
		return nil, err
	}
	return secrets.Unmarshal(plain)
}

func localKeys(key string) ([]secrets.Key, []string) {
	home, _ := os.UserHomeDir()
	first := ""
	if key != "" {
		first = expandHome(key)
	}
	return secrets.LoadKeys(secrets.CandidateKeys(home, first), func(path string) ([]byte, error) {
		if !isTerminal(os.Stdin) {
			return nil, fmt.Errorf("%s needs its passphrase and this is not a terminal", path)
		}
		p, err := readSecret("passphrase for " + path + ": ")
		return []byte(p), err
	})
}

// myKey is the key here that can open the backup, or "".
func myKey(info daemon.BackupInfo, key string) string {
	keys, _ := localKeys(key)
	for _, k := range keys {
		if slices.ContainsFunc(info.Recipients, func(r secrets.Recipient) bool { return r.Key == k.Public }) {
			return k.Path
		}
	}
	return ""
}

// checkMine warns when none of the keys here can open the backup: it would protect everyone but you.
func checkMine(info daemon.BackupInfo, key string) {
	if info.SHA != "" && len(info.Recipients) > 0 && myKey(info, key) == "" {
		warnf("none of your ssh keys can open %s: you can't decrypt the backup yourself. add your public key to the host's ~/.ssh/authorized_keys or to secrets.recipients in vops.yml", secrets.File)
	}
}

// cmdEnvRestore: `vops env restore [file]` decrypts here and sets the secrets the host doesn't have.
func cmdEnvRestore(g globals, args []string) error {
	root := repoRoot()
	file := filepath.Join(root, secrets.File)
	if len(args) > 0 {
		file = args[0]
	}
	blob, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("%w (usage: vops env restore [file], default ./%s)", err, secrets.File)
	}
	r, err := g.remote()
	if err != nil {
		return err
	}
	key := g.key
	if r != nil {
		key = r.Key
	}
	entries, err := openBackup(blob, key)
	if err != nil {
		return err
	}
	if err := restoreEntries(g, entries); err != nil {
		return err
	}
	if r != nil && root != "" {
		o := secretsOpts{}
		if repo, err := os.ReadFile(filepath.Join(root, secrets.File)); err == nil && bytes.Equal(repo, blob) {
			o.entries = entries
		}
		if err := syncSecrets(g, r, root, o); err != nil {
			warnf("secrets backup: %v", err)
		}
	}
	return nil
}

func confirmYes(question string) bool {
	fmt.Fprintf(os.Stderr, "%s [Y/n] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}
