package proxy

import (
	"context"
	"io"
	"net"
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
	tb.Replace([]Route{{Project: "a@pr-1", Service: "web", Domains: []string{"pr.test"}, Backends: []string{"127.0.0.1:1"}}}, map[string]Limits{"a": {Rate: 5, Burst: 5}})
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

// One ipv6 /64 is one client for the rate limit and the per-ip connection cap; an ipv4-mapped address is its ipv4.
func TestIPv6PrefixIsOneClient(t *testing.T) {
	tb, h := limited(t, &Limits{Rate: 1, Burst: 1})
	tb.limiter.now = func() time.Time { return time.Unix(1000, 0) }
	for _, c := range []struct {
		ip   string
		code int
	}{{"[2001:db8::1]", 200}, {"[2001:db8::ffff]", 429}, {"[2001:db8:0:1::1]", 200}, {"10.0.0.1", 200}, {"[::ffff:10.0.0.1]", 429}} {
		if w := serve(h, c.ip, "GET", nil, -1); w.Code != c.code {
			t.Fatalf("%s: %d, want %d", c.ip, w.Code, c.code)
		}
	}

	old := MaxConnsPerIP
	MaxConnsPerIP = 1
	t.Cleanup(func() { MaxConnsPerIP = old })
	conns := make(chan net.Conn, 3)
	for _, ip := range []string{"2001:db8::1", "2001:db8::2", "2001:db8:0:1::1"} {
		c, _ := net.Pipe()
		conns <- addrConn{c, &net.TCPAddr{IP: net.ParseIP(ip), Port: 1234}}
	}
	ll := limitListener{chanListener{conns: conns}, &connLimiter{perIP: map[string]int{}}}
	for _, want := range []string{"2001:db8::1", "2001:db8:0:1::1"} { // 2001:db8::2 shares the first one's /64: closed at accept
		c, err := ll.Accept()
		if err != nil || c.RemoteAddr().(*net.TCPAddr).IP.String() != want {
			t.Fatalf("accepted %v %v, want %s", c.RemoteAddr(), err, want)
		}
	}
}

type addrConn struct {
	net.Conn
	addr net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.addr }

type chanListener struct {
	net.Listener
	conns chan net.Conn
}

func (l chanListener) Accept() (net.Conn, error) { return <-l.conns, nil }
