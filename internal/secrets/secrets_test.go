package secrets

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"golang.org/x/crypto/ssh"
)

func authLine(t *testing.T, pub any, comment string) string {
	t.Helper()
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) + " " + comment
}

// writeKey writes an openssh private key (encrypted when passphrase != "") and returns its authorized line.
func writeKey(t *testing.T, dir, name string, priv any, pub any, passphrase string) string {
	t.Helper()
	var block *pem.Block
	var err error
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, name)
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, name, []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return authLine(t, pub, name)
}

func TestParseAuthorizedKeys(t *testing.T) {
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ed := authLine(t, edPub, "alice@laptop")
	file := strings.Join([]string{
		"# a comment",
		"",
		`command="echo hi",no-pty,from="10.0.0.0/8" ` + ed,
		authLine(t, &rsaKey.PublicKey, "contabo"),
		authLine(t, &ec.PublicKey, "bob-ecdsa"),
		"sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIEX/dQ0v4127bEo8eeG1EV0ApO2lWbSnN6RWusn/NjqIAAAABHNzaDo= yubikey",
		`cert-authority ` + authLine(t, edPub, "ca"),
		"not a key",
	}, "\n")
	ok, skipped := ParseAuthorizedKeys([]byte(file), FromUser)
	if len(ok) != 2 || ok[0].Comment != "alice@laptop" || ok[0].Type != "ssh-ed25519" || ok[1].Type != "ssh-rsa" || ok[0].Source != FromUser {
		t.Fatalf("ok = %+v", ok)
	}
	if strings.Contains(ok[0].Key, "command") || strings.Contains(ok[0].Key, "alice") {
		t.Fatalf("key keeps options or comment: %q", ok[0].Key)
	}
	names := []string{}
	for _, s := range skipped {
		if s.Reason == "" {
			t.Fatalf("skipped without a reason: %+v", s)
		}
		names = append(names, s.Name())
	}
	if got := strings.Join(names, ","); !strings.HasPrefix(got, "bob-ecdsa,yubikey,ca,") || len(skipped) != 4 {
		t.Fatalf("skipped = %s", got)
	}

	// the same key from root and vops.yml counts once, the first source wins
	all := Dedupe(append(ok, ParseRecipient(ed+" again", FromConfig), ParseRecipient(ed, FromRoot)))
	if len(all) != 2 || all[0].Source != FromUser {
		t.Fatalf("dedupe: %+v", all)
	}
	id, _ := age.GenerateX25519Identity()
	if r := ParseRecipient(id.Recipient().String()+" teammate", FromConfig); r.Reason != "" || r.Type != "age" || r.Comment != "teammate" {
		t.Fatalf("age recipient: %+v", r)
	}
	if r := ParseRecipient("age1notakey", FromConfig); r.Reason == "" {
		t.Fatal("a bad age key must be skipped")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	in := []Entry{
		{Project: "shop/api", Key: "PEM", Value: "-----BEGIN-----\nline \"2\"\n"},
		{Project: "shop/api", Preview: true, Key: "STRIPE", Value: "sk_test"},
		{Project: "blog", Key: "A", Value: ""},
		{Project: "shop/api", Key: "DB", Value: "hunter2 # not a comment"},
	}
	b := Marshal(in)
	if !bytes.Equal(b, Marshal([]Entry{in[3], in[2], in[1], in[0]})) {
		t.Fatal("Marshal must not depend on order")
	}
	for _, want := range []string{"[shop/api]\n", "[shop/api previews]\nSTRIPE=\"sk_test\"", "[blog]\nA=\"\""} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in\n%s", want, b)
		}
	}
	out, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 || !bytes.Equal(Marshal(out), b) {
		t.Fatalf("round trip:\n%s\n%+v", b, out)
	}
	if _, err := Unmarshal([]byte("A=\"x\"\n")); err == nil {
		t.Fatal("a key outside a section must fail")
	}
}

func TestEncryptDecrypt(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	encPub, encPriv, _ := ed25519.GenerateKey(rand.Reader)
	strangerPub, strangerPriv, _ := ed25519.GenerateKey(rand.Reader)
	keys := strings.Join([]string{
		writeKey(t, sshDir, "id_ed25519", edPriv, edPub, ""),
		writeKey(t, sshDir, "id_rsa", rsaKey, &rsaKey.PublicKey, ""),
		writeKey(t, filepath.Join(home, "deploy"), "key", encPriv, encPub, "s3cret"),
	}, "\n")
	writeKey(t, filepath.Join(home, "other"), "id_ed25519", strangerPriv, strangerPub, "")
	rs, _ := ParseAuthorizedKeys([]byte(keys), FromUser)
	if _, err := Encrypt([]byte("x"), nil); err == nil {
		t.Fatal("no recipients must be refused")
	}
	plain := Marshal([]Entry{{Project: "shop", Key: "K", Value: "v"}})
	blob, err := Encrypt(plain, rs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(blob), "-----BEGIN AGE ENCRYPTED FILE-----") || bytes.Contains(blob, []byte("shop")) {
		t.Fatalf("not armored or not encrypted:\n%s", blob)
	}
	again, _ := Encrypt(plain, rs)
	if bytes.Equal(blob, again) {
		t.Fatal("age output is random; the daemon must avoid re-encrypting, not rely on this")
	}

	asked := 0
	pass := func(string) ([]byte, error) { asked++; return []byte("s3cret"), nil }
	for _, c := range []struct {
		name  string
		paths []string
	}{
		{"ed25519", []string{filepath.Join(sshDir, "id_ed25519")}},
		{"rsa", []string{filepath.Join(sshDir, "id_rsa")}},
		{"passphrase", []string{filepath.Join(home, "deploy", "key")}},
	} {
		ks, notes := LoadKeys(c.paths, pass)
		if len(ks) != 1 {
			t.Fatalf("%s: keys %v notes %v", c.name, ks, notes)
		}
		got, err := Decrypt(blob, Identities(ks))
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	if asked != 1 {
		t.Fatalf("the passphrase must be asked only for the encrypted key, asked %d times", asked)
	}
	ks, _ := LoadKeys([]string{filepath.Join(home, "other", "id_ed25519"), filepath.Join(home, "missing")}, pass)
	if _, err := Decrypt(blob, Identities(ks)); !errors.Is(err, ErrNoKey) {
		t.Fatalf("a stranger's key: %v", err)
	}
	cands := CandidateKeys(home, "/x/key")
	if cands[0] != "/x/key" || cands[1] != filepath.Join(sshDir, "id_ed25519") || cands[2] != filepath.Join(sshDir, "id_rsa") || len(cands) != 3 {
		t.Fatalf("candidates: %v", cands)
	}
}
