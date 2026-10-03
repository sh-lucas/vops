// Package secrets is the encrypted backup of every `vops env` secret: age, armored, to the ssh keys
// that can reach the host (authorized_keys) plus extra recipients from vops.yml. The host encrypts
// (it only needs public keys); the laptop decrypts with the private keys in ~/.ssh.
package secrets

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
	"golang.org/x/crypto/ssh"
)

// File is the backup at the repo root.
const File = ".secrets.age"

// Sources of recipients.
const (
	FromUser   = "vops user" // ~/.ssh/authorized_keys of the user vops runs as
	FromRoot   = "root"      // /root/.ssh/authorized_keys, snapshot by `vops install`
	FromConfig = "vops.yml"  // secrets.recipients
)

// Recipient is a public key the backup is encrypted to. Reason is set on keys age can't use (skipped).
type Recipient struct {
	Type        string `json:"type"` // ssh-ed25519, ssh-rsa, age, or the unsupported type
	Comment     string `json:"comment,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Source      string `json:"source"`
	Key         string `json:"key"` // "ssh-ed25519 AAAA…" or "age1…", without options or comment
	Reason      string `json:"reason,omitempty"`
	r           age.Recipient
}

// Name is how a person recognizes the key: its comment, else its fingerprint.
func (r Recipient) Name() string {
	if r.Comment != "" {
		return r.Comment
	}
	if r.Fingerprint != "" {
		return r.Fingerprint
	}
	return r.Key
}

func fromSSH(pk ssh.PublicKey, comment, source string) Recipient {
	rc := Recipient{Type: pk.Type(), Comment: comment, Fingerprint: ssh.FingerprintSHA256(pk), Source: source,
		Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))}
	var err error
	switch pk.Type() {
	case ssh.KeyAlgoED25519:
		rc.r, err = agessh.NewEd25519Recipient(pk)
	case ssh.KeyAlgoRSA:
		rc.r, err = agessh.NewRSARecipient(pk)
	default:
		err = errors.New("age only works with ed25519 and rsa keys")
	}
	if err != nil {
		rc.Reason = err.Error()
	}
	return rc
}

// ParseAuthorizedKeys reads an authorized_keys file (options like command= or from= included).
// Keys age can't use (ecdsa, security keys, …) come back in skipped with the reason.
func ParseAuthorizedKeys(b []byte, source string) (ok, skipped []Recipient) {
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pk, comment, opts, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			skipped = append(skipped, Recipient{Type: "?", Source: source, Key: short(line), Reason: "not a key ssh understands"})
			continue
		}
		rc := fromSSH(pk, comment, source)
		if slices.Contains(opts, "cert-authority") {
			rc.Reason = "a certificate authority, not a person's key"
		}
		if rc.Reason == "" {
			ok = append(ok, rc)
		} else {
			skipped = append(skipped, rc)
		}
	}
	return ok, skipped
}

// ParseRecipient reads an entry of secrets.recipients: an ssh public key line or an age1… key.
func ParseRecipient(s, source string) Recipient {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "age1") {
		key, comment, _ := strings.Cut(s, " ")
		rc := Recipient{Type: "age", Comment: strings.TrimSpace(comment), Source: source, Key: key}
		r, err := age.ParseX25519Recipient(key)
		if err != nil {
			rc.Reason = "not an age1 X25519 key (post-quantum and plugin keys aren't supported)"
		}
		rc.r = r
		return rc
	}
	ok, skipped := ParseAuthorizedKeys([]byte(s), source)
	if len(ok) == 1 {
		return ok[0]
	}
	if len(skipped) == 1 {
		return skipped[0]
	}
	return Recipient{Type: "?", Source: source, Key: short(s), Reason: "expected one ssh public key or age1 key"}
}

func short(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// Dedupe keeps the first occurrence of each key (so the first source wins).
func Dedupe(rs []Recipient) []Recipient {
	var out []Recipient
	for _, r := range rs {
		if !slices.ContainsFunc(out, func(o Recipient) bool { return o.Key == r.Key }) {
			out = append(out, r)
		}
	}
	return out
}

// ---- the plaintext: a commented .env per project and scope

// Entry is one secret. Preview: the project's preview secrets (`vops env set --preview`).
type Entry struct {
	Project string `json:"project"`
	Preview bool   `json:"preview,omitempty"`
	Key     string `json:"key"`
	Value   string `json:"value,omitempty"`
}

func (e Entry) Scope() string {
	if e.Preview {
		return e.Project + " previews"
	}
	return e.Project
}

func (e Entry) ID() string { return e.Scope() + " " + e.Key }

// Marshal writes entries sorted, values Go-quoted (so newlines and quotes survive): the same input
// always gives the same bytes.
func Marshal(entries []Entry) []byte {
	es := slices.Clone(entries)
	slices.SortFunc(es, func(a, b Entry) int { return strings.Compare(a.ID(), b.ID()) })
	var b bytes.Buffer
	b.WriteString("# vops secrets backup: every `vops env` secret of the host. Restore it with `vops env restore`.\n")
	scope := ""
	for i, e := range es {
		if i == 0 || e.Scope() != scope {
			scope = e.Scope()
			fmt.Fprintf(&b, "\n[%s]\n", scope)
		}
		fmt.Fprintf(&b, "%s=%s\n", e.Key, strconv.Quote(e.Value))
	}
	return b.Bytes()
}

// Unmarshal reads what Marshal wrote.
func Unmarshal(b []byte) ([]Entry, error) {
	var out []Entry
	var cur *Entry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			project, rest, _ := strings.Cut(line[1:len(line)-1], " ")
			if project == "" || rest != "" && rest != "previews" {
				return nil, fmt.Errorf("line %d: bad section %s", n, line)
			}
			cur = &Entry{Project: project, Preview: rest == "previews"}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || cur == nil {
				return nil, fmt.Errorf("line %d: expected KEY=\"value\" inside a [project] section", n)
			}
			val, err := strconv.Unquote(v)
			if err != nil {
				return nil, fmt.Errorf("line %d: %s: bad value", n, k)
			}
			out = append(out, Entry{cur.Project, cur.Preview, k, val})
		}
	}
	return out, sc.Err()
}

// ---- age

// Encrypt encrypts plain to the usable recipients, armored. It refuses when there are none.
func Encrypt(plain []byte, rs []Recipient) ([]byte, error) {
	var ars []age.Recipient
	for _, r := range rs {
		if r.Reason == "" && r.r != nil {
			ars = append(ars, r.r)
		}
	}
	if len(ars) == 0 {
		return nil, errors.New("no key can decrypt it")
	}
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, ars...)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ErrNoKey: none of the identities is a recipient.
var ErrNoKey = errors.New("none of these keys can decrypt it")

// Decrypt opens an armored (or binary) backup.
func Decrypt(blob []byte, ids []age.Identity) ([]byte, error) {
	if len(ids) == 0 {
		return nil, ErrNoKey
	}
	var src io.Reader = bytes.NewReader(blob)
	if bytes.HasPrefix(bytes.TrimSpace(blob), []byte(armor.Header)) {
		src = armor.NewReader(bytes.NewReader(bytes.TrimSpace(blob)))
	}
	r, err := age.Decrypt(src, ids...)
	var nm *age.NoIdentityMatchError
	if errors.As(err, &nm) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// ---- private keys on the laptop

// Key is a private key file that can decrypt (when it is a recipient).
type Key struct {
	Path     string
	Public   string // "ssh-ed25519 AAAA…"
	Identity age.Identity
}

// CandidateKeys is where to look: the given keys first, then ~/.ssh/id_ed25519, id_rsa and other id_*.
func CandidateKeys(home string, first ...string) []string {
	var out []string
	add := func(p string) {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, p := range first {
		add(p)
	}
	add(filepath.Join(home, ".ssh", "id_ed25519"))
	add(filepath.Join(home, ".ssh", "id_rsa"))
	matches, _ := filepath.Glob(filepath.Join(home, ".ssh", "id_*"))
	for _, m := range matches {
		if !strings.HasSuffix(m, ".pub") {
			add(m)
		}
	}
	return out
}

// LoadKeys reads the private keys that exist. Passphrase-protected ones ask passphrase(path) only when
// a backup is actually encrypted to them. notes say why a file was left out.
func LoadKeys(paths []string, passphrase func(path string) ([]byte, error)) (keys []Key, notes []string) {
	for _, p := range paths {
		pem, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var pub ssh.PublicKey
		var id age.Identity
		signer, err := ssh.ParsePrivateKey(pem)
		var pm *ssh.PassphraseMissingError
		switch {
		case err == nil:
			pub = signer.PublicKey()
			id, err = agessh.ParseIdentity(pem)
		case errors.As(err, &pm):
			pub = pm.PublicKey
			if pub == nil { // old PEM keys don't carry their public key: read the .pub next to it
				if b, rerr := os.ReadFile(p + ".pub"); rerr == nil {
					pub, _, _, _, _ = ssh.ParseAuthorizedKey(b)
				}
			}
			if pub == nil {
				err = errors.New("encrypted and no .pub next to it")
				break
			}
			path := p
			id, err = agessh.NewEncryptedSSHIdentity(pub, pem, func() ([]byte, error) { return passphrase(path) })
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		keys = append(keys, Key{Path: p, Public: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), Identity: id})
	}
	return keys, notes
}

// Identities of keys, for Decrypt.
func Identities(keys []Key) []age.Identity {
	var out []age.Identity
	for _, k := range keys {
		out = append(out, k.Identity)
	}
	return out
}
