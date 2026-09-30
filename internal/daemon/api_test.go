package daemon

import (
	"net/http/httptest"
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
