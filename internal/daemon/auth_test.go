package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/registry"
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

// regAuth checks registry credentials as a client at 192.0.2.1 would send them.
func regAuth(d *Daemon, user, pass string) *registry.Perm {
	p, _ := d.registryAuth(httptest.NewRequest("GET", "/v2/", nil), user, pass)
	return p
}

func TestRegistryAuth(t *testing.T) {
	d := testDaemon(t)
	d.DB.PutUser(store.User{Name: "admin", Role: store.Admin}, "admin password 1")
	d.DB.PutUser(store.User{Name: "all", Role: store.Deployer, Global: true}, "token-all-0123")
	d.DB.PutUser(store.User{Name: "ci", Role: store.Deployer, Repos: []string{"shop/web:latest", "shop/api"}}, "token-ci-01234")
	can := func(user, pass, repo string) (pull, push bool) {
		p := regAuth(d, user, pass)
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
	if p := regAuth(d, "vops-internal", d.pullToken); p == nil || !p.Pull("x/y") || p.Push("x/y") {
		t.Error("the internal user pulls everything and pushes nothing")
	}

	// a deleted user or an old token stops working at once, even though the result was cached
	h := d.UIHandler()
	_, cookie := login(t, h, "admin", "admin password 1")
	if w := call(h, "DELETE", "/api/users?name=ci", "", cookie); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if regAuth(d, "ci", "token-ci-01234") != nil {
		t.Fatal("a deleted user still pushes (auth cache)")
	}
	if w := call(h, "POST", "/api/users", `{"name":"all","new_token":true}`, cookie); w.Code != 200 || !strings.Contains(w.Body.String(), `"token":"`) {
		t.Fatalf("new token: %d %s", w.Code, w.Body)
	}
	if regAuth(d, "all", "token-all-0123") != nil {
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

// Wrong credentials from one client, dashboard and registry together, get it refused for a while; other clients,
// the internal user and earlier sessions aren't affected, and a right password doesn't reset the count.
func TestAuthLimiter(t *testing.T) {
	d := testDaemon(t)
	clock := time.Now()
	d.limiter.now = func() time.Time { return clock }
	h := d.UIHandler()
	d.DB.PutUser(store.User{Name: "admin", Role: store.Admin}, "admin password 1")
	d.DB.PutUser(store.User{Name: "ci", Role: store.Deployer, Global: true}, "token-ci-01234")
	_, before := login(t, h, "admin", "admin password 1")

	from := func(ip, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://vops.test"+path, strings.NewReader(body))
		r.RemoteAddr = ip
		r.Header.Set("X-Vops", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	reg := func(ip, user, pass string) int {
		r := httptest.NewRequest("GET", "http://vops.test/v2/", nil)
		r.RemoteAddr = ip
		r.SetBasicAuth(user, pass)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	attacker := "203.0.113.7:4000"
	if w := from(attacker, "POST", "/api/login", `{"user":"admin","password":"admin password 1"}`); w.Code != 200 {
		t.Fatalf("right password: %d", w.Code)
	}
	for i := range authMaxFails / 2 {
		if w := from(attacker, "POST", "/api/login", `{"user":"admin","password":"guess"}`); w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
		if code := reg(attacker, "ci", "guess"); code != 401 {
			t.Fatalf("registry guess %d: %d", i, code)
		}
	}
	if w := from(attacker, "POST", "/api/login", `{"user":"admin","password":"admin password 1"}`); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("refused client, right password: %d", w.Code)
	}
	if code := reg(attacker, "ci", "token-ci-01234"); code != 429 {
		t.Fatalf("refused client on the registry: %d", code)
	}
	if code := reg("[2001:db8::1]:1", "ci", "token-ci-01234"); code != 200 {
		t.Fatalf("another client: %d", code)
	}
	// `vops ui` tunnels all come from loopback, already authenticated by ssh: never refused
	for range authMaxFails + 1 {
		from("127.0.0.1:5000", "POST", "/api/login", `{"user":"admin","password":"guess"}`)
	}
	if w := from("[::1]:5001", "POST", "/api/login", `{"user":"admin","password":"admin password 1"}`); w.Code != 200 {
		t.Fatalf("a tunnel after another tunnel's failures: %d", w.Code)
	}
	if code := reg(attacker, "vops-internal", d.pullToken); code != 200 {
		t.Fatalf("the internal user: %d", code)
	}
	if w := call(h, "GET", "/api/users", "", before); w.Code != 200 {
		t.Fatalf("an earlier session: %d", w.Code)
	}
	if evs, _ := d.DB.Events("", 50); !slices.ContainsFunc(evs, func(e store.Event) bool { return strings.Contains(e.Message, "refused for 15m") }) {
		t.Fatal("no event says the client was refused")
	}
	clock = clock.Add(authWindow + time.Second)
	if w := from(attacker, "POST", "/api/login", `{"user":"admin","password":"admin password 1"}`); w.Code != 200 {
		t.Fatalf("after the window: %d", w.Code)
	}

	// an ipv6 /64 is one client; proxied addresses come without a port
	for addr, want := range map[string]string{
		"203.0.113.7:4000":         "203.0.113.7",
		"203.0.113.7":              "203.0.113.7",
		"[2001:db8:1:2:3::9]:443":  "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1":     "2001:db8:1:2::/64",
		"[::ffff:198.51.100.1]:80": "198.51.100.1",
		"@":                        "@",
	} {
		if got := clientKey(addr); got != want {
			t.Errorf("clientKey(%q) = %q, want %q", addr, got, want)
		}
	}
}

// Parallel guesses can't get past the limit: an attempt is reserved before the password is checked.
func TestAuthLimiterParallel(t *testing.T) {
	l := newAuthLimiter()
	r := httptest.NewRequest("GET", "/", nil)
	var mu sync.Mutex
	checked := 0
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			l.check(r, func() bool {
				mu.Lock()
				checked++
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				return false
			})
		})
	}
	wg.Wait()
	if checked != authMaxFails {
		t.Fatalf("%d passwords checked, want %d", checked, authMaxFails)
	}
}

// With ui: off, podman still pulls our images from a loopback port that serves only the registry.
func TestPullsWithUIOff(t *testing.T) {
	dir := t.TempDir()
	vh := filepath.Join(dir, ".vops")
	os.MkdirAll(vh, 0o700)
	if err := os.WriteFile(LockPath(vh), []byte("ui: \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := New(vh, filepath.Join(dir, "vops"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DB.Close() })
	if d.pulls == nil || d.Engine.PullAddr != d.pulls.Addr().String() || !strings.HasPrefix(d.Engine.PullAddr, "127.0.0.1:") {
		t.Fatalf("pull address with ui: off: %q", d.Engine.PullAddr)
	}
	go http.Serve(d.pulls, d.pullHandler())
	t.Cleanup(func() { d.pulls.Close() })
	get := func(path, user, pass string) int {
		req, _ := http.NewRequest("GET", "http://"+d.Engine.PullAddr+path, nil)
		req.SetBasicAuth(user, pass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/v2/", "vops-internal", d.pullToken); code != 200 {
		t.Fatalf("registry on the pull port: %d", code)
	}
	if code := get("/api/status", "vops-internal", d.pullToken); code != 404 {
		t.Fatalf("the pull port serves more than the registry: %d", code)
	}
	b, _ := os.ReadFile(d.Engine.PullAuthFile)
	if !strings.Contains(string(b), d.Engine.PullAddr) {
		t.Fatalf("pull auth file: %s", b)
	}
}
