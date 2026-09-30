package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/store"
)

func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	loginDelay = time.Millisecond
	dir := t.TempDir()
	d, err := New(filepath.Join(dir, ".vops"), filepath.Join(dir, "vops"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DB.Close() })
	return d
}

// call sends a request to the dashboard handler (session auth) with the X-Vops header and a cookie.
func call(h http.Handler, method, path, body, cookie string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://vops.test"+path, strings.NewReader(body))
	r.Header.Set("X-Vops", "1")
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func login(t *testing.T, h http.Handler, user, password string) (int, string) {
	t.Helper()
	w := call(h, "POST", "/api/login", `{"user":"`+user+`","password":"`+password+`"}`, "")
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return w.Code, c.Value
		}
	}
	return w.Code, ""
}

func TestRegistryAuth(t *testing.T) {
	d := testDaemon(t)
	d.DB.PutUser(store.User{Name: "admin", Role: store.Admin}, "admin password 1")
	d.DB.PutUser(store.User{Name: "all", Role: store.Deployer, Global: true}, "token-all-0123")
	d.DB.PutUser(store.User{Name: "ci", Role: store.Deployer, Repos: []string{"shop/web:latest", "shop/api"}}, "token-ci-01234")
	can := func(user, pass, repo string) (pull, push bool) {
		p := d.registryAuth(user, pass)
		if p == nil {
			return false, false
		}
		return p.Pull(repo), p.Push(repo)
	}
	for _, c := range []struct {
		user, pass, repo string
		ok               bool
	}{
		{"admin", "admin password 1", "any/thing", true},
		{"admin", "wrong", "any/thing", false},
		{"all", "token-all-0123", "other/repo", true},
		{"ci", "token-ci-01234", "shop/web", true}, // the :latest suffix was dropped
		{"ci", "token-ci-01234", "shop/api", true},
		{"ci", "token-ci-01234", "shop/db", false},
		{"ci", "token-ci-01234", "shop/web/extra", false},
		{"ci", "token-all-0123", "shop/web", false},
		{"nobody", "token-ci-01234", "shop/web", false},
		{"vops-internal", "token-ci-01234", "shop/web", false},
	} {
		if pull, push := can(c.user, c.pass, c.repo); pull != c.ok || push != c.ok {
			t.Errorf("%s on %s: pull %v push %v, want %v", c.user, c.repo, pull, push, c.ok)
		}
	}
	if p := d.registryAuth("vops-internal", d.pullToken); p == nil || !p.Pull("x/y") || p.Push("x/y") {
		t.Error("the internal user pulls everything and pushes nothing")
	}

	// a deleted user or an old token stops working at once, even though the result was cached
	h := d.UIHandler()
	_, cookie := login(t, h, "admin", "admin password 1")
	if w := call(h, "DELETE", "/api/users?name=ci", "", cookie); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if d.registryAuth("ci", "token-ci-01234") != nil {
		t.Fatal("a deleted user still pushes (auth cache)")
	}
	if w := call(h, "POST", "/api/users", `{"name":"all","new_token":true}`, cookie); w.Code != 200 || !strings.Contains(w.Body.String(), `"token":"`) {
		t.Fatalf("new token: %d %s", w.Code, w.Body)
	}
	if d.registryAuth("all", "token-all-0123") != nil {
		t.Fatal("an old token still works (auth cache)")
	}
	if w := call(h, "POST", "/api/users", `{"name":"all","global":false,"repos":["a/b"]}`, cookie); w.Code != 200 {
		t.Fatalf("narrow: %d %s", w.Code, w.Body)
	}
}

func TestDashboardLogin(t *testing.T) {
	d := testDaemon(t)
	h := d.UIHandler()
	d.DB.PutUser(store.User{Name: "admin", Role: store.Admin}, "admin password 1")
	d.DB.PutUser(store.User{Name: "ci", Role: store.Deployer, Global: true}, "token-ci-01234")

	if code, _ := login(t, h, "admin", "nope"); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	deployer := call(h, "POST", "/api/login", `{"user":"ci","password":"token-ci-01234"}`, "")
	wrong := call(h, "POST", "/api/login", `{"user":"ci","password":"token-ci-99999"}`, "")
	if deployer.Code != 401 || deployer.Body.String() != wrong.Body.String() {
		t.Fatalf("a deployer's right token must fail like a wrong one: %d %s / %s", deployer.Code, deployer.Body, wrong.Body)
	}
	if evs, _ := d.DB.Events("", 1); !strings.Contains(evs[0].Message, `failed dashboard login as "ci"`) {
		t.Fatalf("event: %+v", evs)
	}
	code, cookie := login(t, h, "admin", "admin password 1")
	if code != 200 || cookie == "" {
		t.Fatalf("admin login: %d", code)
	}
	if w := call(h, "GET", "/api/users", "", cookie); w.Code != 200 {
		t.Fatalf("with a session: %d", w.Code)
	}
	if w := call(h, "GET", "/api/users", "", "forged"); w.Code != 401 {
		t.Fatalf("unknown session: %d", w.Code)
	}
	if w := call(h, "POST", "/api/logout", "", cookie); w.Code != 200 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := call(h, "GET", "/api/users", "", cookie); w.Code != 401 {
		t.Fatalf("after logout: %d", w.Code)
	}

	// expired
	id, _ := d.DB.NewSession("admin", -time.Second)
	if w := call(h, "GET", "/api/users", "", id); w.Code != 401 {
		t.Fatalf("expired session: %d", w.Code)
	}
	// demoted: the session is gone (the db drops it)
	d.DB.PutUser(store.User{Name: "ops", Role: store.Admin}, "ops password 12")
	_, ops := login(t, h, "ops", "ops password 12")
	if w := call(h, "POST", "/api/users", `{"name":"ops","role":"deployer","global":true}`, ops); w.Code != 200 {
		t.Fatalf("demote: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/users", "", ops); w.Code != 401 {
		t.Fatalf("demoted admin keeps its session: %d", w.Code)
	}
	// the last admin stays
	_, cookie = login(t, h, "admin", "admin password 1")
	if w := call(h, "DELETE", "/api/users?name=admin", "", cookie); w.Code != 400 || !strings.Contains(w.Body.String(), "last admin") {
		t.Fatalf("delete the last admin: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/users", `{"name":"x","role":"admin"}`, cookie); w.Code != 400 || !strings.Contains(w.Body.String(), "password") {
		t.Fatalf("admin without password: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/users", `{"name":"y"}`, cookie); w.Code != 400 || !strings.Contains(w.Body.String(), "global or at least one repo") {
		t.Fatalf("deployer without access: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/status", "", cookie); w.Code == 401 {
		t.Fatalf("status: %d", w.Code)
	}
}
