package daemon

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sh-lucas/vops/internal/store"
)

// Every route of the web api needs a session, and every mutating one the X-Vops header (CSRF) first.
func TestEveryRouteNeedsASession(t *testing.T) {
	d := testDaemon(t)
	h := d.UIHandler()
	if len(d.webRoutes) < 20 {
		t.Fatalf("only %d routes recorded", len(d.webRoutes))
	}
	wild := regexp.MustCompile(`\{[^}]*\}`)
	for _, pattern := range d.webRoutes {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Errorf("route %q has no method: it answers every method", pattern)
			continue
		}
		path = wild.ReplaceAllString(path, "x")
		if method != "GET" {
			r := httptest.NewRequest(method, "http://vops.test"+path, strings.NewReader("{}"))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Errorf("%s without X-Vops: %d", pattern, w.Code)
			}
		}
		if w := call(h, method, path, "{}", ""); w.Code != 401 {
			t.Errorf("%s without a session: %d", pattern, w.Code)
		}
		if w := call(h, method, path, "{}", "not-a-session"); w.Code != 401 {
			t.Errorf("%s with a made-up session: %d", pattern, w.Code)
		}
	}
}

func TestJSONBodies(t *testing.T) {
	d := testDaemon(t)
	h := d.UIHandler()
	d.DB.PutUser(store.User{Name: "admin", Role: store.Admin}, "admin password 1")
	_, cookie := login(t, h, "admin", "admin password 1")
	big := `{"project":"shop","key":"K","value":"` + strings.Repeat("a", 2<<20) + `"}`
	if w := call(h, "POST", "/api/env", big, cookie); w.Code != 413 || !strings.Contains(w.Body.String(), "too large") {
		t.Fatalf("oversized: %d %s", w.Code, w.Body)
	}
	for _, body := range []string{"{", `{"project": 1}`, "[]", "", `{"project":"shop"}{"x":1}`} {
		if w := call(h, "POST", "/api/env", body, cookie); w.Code != 400 {
			t.Errorf("malformed %q: %d %s", body, w.Code, w.Body)
		}
	}
	if keys, _ := d.DB.EnvKeys("shop"); len(keys) != 0 {
		t.Fatalf("a bad request wrote env: %+v", keys)
	}
	if w := call(h, "POST", "/api/login", `{"password":"`+strings.Repeat("a", 2<<20)+`"}`, ""); w.Code != 400 {
		t.Fatalf("oversized login: %d", w.Code)
	}
	if w := call(h, "POST", "/api/users", `{"name":"x'; DROP TABLE users;--","global":true}`, cookie); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid user name") {
		t.Fatalf("user name: %d %s", w.Code, w.Body)
	}
}

// What the web api takes from a client is checked before it reaches the db, podman or the disk.
func TestInputsValidated(t *testing.T) {
	d := testDaemon(t)
	h := d.UIHandler()
	d.DB.PutUser(store.User{Name: "admin", Role: store.Admin}, "admin password 1")
	_, cookie := login(t, h, "admin", "admin password 1")
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/project", `{"project":"../etc","disabled":true}`},
		{"POST", "/api/project", `{"project":"shop@pr-1","disabled":true}`},
		{"POST", "/api/project", `{"project":"","disabled":true}`},
		{"POST", "/api/restart", `{"project":"shop","service":"--all"}`},
		{"POST", "/api/restart", `{"project":"shop@../x"}`},
		{"GET", "/api/logs?project=shop&service=a%20b", ""},
		{"GET", "/api/logs?project=-shop", ""},
		{"GET", "/api/timeline?project=shop/../x", ""},
		{"GET", "/api/snapshots?project=a//b", ""},
		{"POST", "/api/snapshots", `{"project":"shop@pr"}`},
		{"POST", "/api/snapshots/import?project=..", ""},
		{"POST", "/api/unpin", `{"project":"shop","services":["web","x;y"]}`},
		{"DELETE", "/api/env?project=shop&key=A-B", ""},
		{"DELETE", "/api/env?project=*", ""},
		{"DELETE", "/api/users?name=../admin", ""},
	} {
		if w := call(h, c.method, c.path, c.body, cookie); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid") && !strings.Contains(w.Body.String(), "must match") {
			t.Errorf("%s %s %s: %d %s", c.method, c.path, c.body, w.Code, w.Body)
		}
	}
	if flags, _ := d.DB.Projects(); len(flags) != 0 {
		t.Fatalf("a refused request wrote a project: %+v", flags)
	}
}

// The dashboard shows git-tracked text files only, never through a symlink, never outside the repo.
func TestFileStaysInRepo(t *testing.T) {
	d := testDaemon(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "a.yml"), []byte("secret: 1\n"), 0o644)
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", d.Repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.MkdirAll(filepath.Join(d.Repo, "conf"), 0o755)
	os.WriteFile(filepath.Join(d.Repo, "readme.md"), []byte("hi\n"), 0o644)
	os.WriteFile(filepath.Join(d.Repo, "conf", "a.yml"), []byte("a: 1\n"), 0o644)
	os.WriteFile(filepath.Join(d.Repo, ".env"), []byte("K=v\n"), 0o644)
	os.Symlink(filepath.Join(outside, "a.yml"), filepath.Join(d.Repo, "link.yml"))
	git("init", "-q")
	git("add", "-A")
	git("commit", "-qm", "x")
	// after the commit, conf/ becomes a symlink out of the repo; git still lists conf/a.yml
	os.RemoveAll(filepath.Join(d.Repo, "conf"))
	os.Symlink(outside, filepath.Join(d.Repo, "conf"))

	ctx := t.Context()
	if b, err := d.file(ctx, "readme.md"); err != nil || string(b) != "hi\n" {
		t.Fatalf("readme: %q %v", b, err)
	}
	for _, p := range []string{"link.yml", "conf/a.yml", ".env", "../a.yml", "untracked.md", "/etc/hostname"} {
		if b, err := d.file(ctx, p); err == nil {
			t.Errorf("%s: read %q", p, b)
		}
	}
	os.WriteFile(filepath.Join(d.Repo, "readme.md"), bytes.Repeat([]byte("a"), maxFile+1), 0o644)
	if _, err := d.file(ctx, "readme.md"); err == nil {
		t.Error("a file over 512KB was read")
	}
}

func TestSecurityHeaders(t *testing.T) {
	d := testDaemon(t)
	h := d.UIHandler()
	w := call(h, "GET", "/", "", "")
	for _, k := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options", "Cross-Origin-Opener-Policy"} {
		if w.Header().Get(k) == "" {
			t.Errorf("/ has no %s", k)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "script-src 'unsafe") {
		t.Errorf("csp: %s", csp)
	}
	if w := call(h, "GET", "/v2/", "", ""); w.Header().Get("Content-Security-Policy") != "" || w.Code != 401 {
		t.Errorf("registry: %d %v", w.Code, w.Header())
	}
}

// Invalid utf-8 (container output cut mid-rune) still makes valid json, never an empty 200.
func TestWriteJSONInvalidUTF8(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, map[string]string{"error": "boom \xc3"})
	if !strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("body: %q", w.Body)
	}
	if got := trunc("ação", 2); got != "a" {
		t.Fatalf("trunc cut a rune: %q", got)
	}
}
