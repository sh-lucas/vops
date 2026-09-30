package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/config"
)

// limited serves a table with one route of project "a" (limits l) to an app that reads the whole body.
func limited(t *testing.T, l *Limits) (*Table, http.Handler) {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		io.WriteString(w, strings.Repeat("x", int(n%10)))
	}))
	t.Cleanup(app.Close)
	tb := NewTable()
	tb.SetRoutes([]Route{{Project: "a", Service: "web", Domains: []string{"a.test"}, Backends: []string{strings.TrimPrefix(app.URL, "http://")}, Limits: l}})
	return tb, Handler(tb)
}

func serve(h http.Handler, ip, method string, body io.Reader, size int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://a.test/", body)
	r.RemoteAddr = ip + ":1234"
	r.Header.Set("X-Forwarded-For", "9.9.9.9") // never trusted
	if size >= 0 {
		r.ContentLength = size
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRateLimitBurstAndPerIP(t *testing.T) {
	tb, h := limited(t, &Limits{Rate: 1, Burst: 3})
	clock := time.Unix(1000, 0)
	tb.limiter.now = func() time.Time { return clock }
	for i := range 3 {
		if w := serve(h, "10.0.0.1", "GET", nil, -1); w.Code != 200 {
			t.Fatalf("request %d within burst: %d", i, w.Code)
		}
	}
	w := serve(h, "10.0.0.1", "GET", nil, -1)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("over burst: %d retry-after %q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := serve(h, "10.0.0.2", "GET", nil, -1); w.Code != 200 {
		t.Fatalf("another ip is limited too: %d", w.Code)
	}
	clock = clock.Add(time.Second)
	if w := serve(h, "10.0.0.1", "GET", nil, -1); w.Code != 200 {
		t.Fatalf("after refill: %d", w.Code)
	}
	if c := tb.Limited()["a"]; c.Limited != 1 || c.TooLarge != 0 {
		t.Fatalf("counters: %+v", c)
	}
	clock = clock.Add(time.Hour)
	serve(h, "10.0.0.3", "GET", nil, -1)
	if n := len(tb.limiter.buckets); n != 1 {
		t.Fatalf("idle buckets not purged: %d left", n)
	}
}

func TestRetryAfterSlowRate(t *testing.T) {
	tb, h := limited(t, &Limits{Rate: 1.0 / 60, Burst: 1}) // 1/m
	tb.limiter.now = func() time.Time { return time.Unix(1000, 0) }
	serve(h, "10.0.0.1", "GET", nil, -1)
	if w := serve(h, "10.0.0.1", "GET", nil, -1); w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("%d retry-after %q", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestMaxBody(t *testing.T) {
	tb, h := limited(t, &Limits{MaxBody: 100})
	if w := serve(h, "10.0.0.1", "POST", strings.NewReader(strings.Repeat("a", 100)), 100); w.Code != 200 {
		t.Fatalf("at the limit: %d %s", w.Code, w.Body)
	}
	if w := serve(h, "10.0.0.1", "POST", strings.NewReader(strings.Repeat("a", 101)), 101); w.Code != 413 {
		t.Fatalf("content-length over: %d", w.Code)
	}
	// chunked: no Content-Length, the body is cut while it streams
	if w := serve(h, "10.0.0.1", "POST", io.LimitReader(zeros{}, 1<<20), -1); w.Code != 413 {
		t.Fatalf("chunked over: %d %s", w.Code, w.Body)
	}
	if w := serve(h, "10.0.0.1", "POST", strings.NewReader("small"), -1); w.Code != 200 {
		t.Fatalf("chunked under: %d", w.Code)
	}
	if c := tb.Limited()["a"]; c.TooLarge != 2 {
		t.Fatalf("counters: %+v", c)
	}
}

func TestUnlimitedAndPreviewsInherit(t *testing.T) {
	_, h := limited(t, nil)
	for range 50 {
		if w := serve(h, "10.0.0.1", "POST", io.LimitReader(zeros{}, 1<<20), -1); w.Code != 200 {
			t.Fatalf("no limits: %d", w.Code)
		}
	}
	tb := NewTable()
	tb.Replace(map[Key][2][]string{{"a@pr-1", "web"}: {{"pr.test"}, {"127.0.0.1:1"}}}, map[string]Limits{"a": {Rate: 5, Burst: 5}})
	if r := tb.Routes(); r[0].Limits == nil || r[0].Limits.Rate != 5 {
		t.Fatalf("preview route without its project's limits: %+v", r)
	}
}

// Limits travel with the table: pushed, saved to disk, back after a restart, and part of the hash.
func TestLimitsSurviveRestart(t *testing.T) {
	vh := t.TempDir()
	addr := freeAddr(t)
	cfg := config.Config{Domain: "x.test", HTTP: addr, HTTPS: "off", UI: "off", TLS: "off"}.Defaults()
	if err := config.WriteLockFile(config.LockPath(vh), cfg); err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "app") }))
	defer app.Close()
	routes := []Route{{Project: "a", Service: "web", Domains: []string{"a.x.test"}, Backends: []string{strings.TrimPrefix(app.URL, "http://")}, Limits: &Limits{Rate: 0.001, Burst: 1}}}
	if Hash(routes) == Hash([]Route{{Project: "a", Service: "web", Domains: routes[0].Domains, Backends: routes[0].Backends}}) {
		t.Fatal("limits are not part of the table hash")
	}
	stop, _ := start(t, vh)
	if _, err := NewClient(vh).Push(context.Background(), routes); err != nil {
		t.Fatal(err)
	}
	stop()
	start(t, vh)
	if code, _ := get(t, addr, "a.x.test"); code != 200 {
		t.Fatalf("first: %d", code)
	}
	if code, _ := get(t, addr, "a.x.test"); code != 429 {
		t.Fatalf("limits lost on restart: %d", code)
	}
	st, err := NewClient(vh).Status(context.Background())
	if err != nil || st.Limited["a"].Limited != 1 || st.Hash != Hash(routes) {
		t.Fatalf("status: %+v %v", st, err)
	}
}
