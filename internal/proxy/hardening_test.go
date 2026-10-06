package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// route serves a table with one route of project "a" (limits l) to app.
func route(t *testing.T, app http.Handler, l *Limits) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(app)
	t.Cleanup(up.Close)
	tb := NewTable()
	tb.SetRoutes([]Route{{Project: "a", Service: "web", Domains: []string{"a.test"}, Backends: []string{strings.TrimPrefix(up.URL, "http://")}, Limits: l}})
	srv := httptest.NewServer(idleBodies(Handler(tb)))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, req *http.Request) (int, string) {
	t.Helper()
	req.Host = "a.test"
	resp, err := srv.Client().Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The upstream timeout covers the wait for response headers only: a slow app is a 504, a long stream is not cut.
func TestUpstreamHeaderTimeout(t *testing.T) {
	srv := route(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(3 * time.Second)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 4 {
			fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(500 * time.Millisecond)
		}
	}), &Limits{Timeout: 1})
	req, _ := http.NewRequest("GET", srv.URL+"/slow", nil)
	start := time.Now()
	if code, _ := do(t, srv, req); code != 504 || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("slow upstream: %d after %s", code, time.Since(start))
	}
	req, _ = http.NewRequest("GET", srv.URL+"/events", nil)
	if code, body := do(t, srv, req); code != 200 || !strings.Contains(body, "data: 3") {
		t.Fatalf("a stream longer than the timeout was cut: %d %q", code, body)
	}
}

// The header timeout starts once the request is written: an upload slower than it is not a 504.
func TestUpstreamTimeoutAfterUpload(t *testing.T) {
	srv := route(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, n)
	}), &Limits{Timeout: 1})
	pr, pw := io.Pipe()
	go func() {
		for range 5 {
			pw.Write([]byte("x"))
			time.Sleep(400 * time.Millisecond)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", srv.URL+"/upload", pr)
	if code, body := do(t, srv, req); code != 200 || body != "5" {
		t.Fatalf("slow upload: %d %q", code, body)
	}
}

// X-Forwarded-For is the tcp peer, never what the client sent (the daemon trusts it for login events).
func TestForwardedForIsOverwritten(t *testing.T) {
	srv := route(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Header.Get("X-Forwarded-For"))
	}), nil)
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	if code, body := do(t, srv, req); code != 200 || body != "127.0.0.1" {
		t.Fatalf("X-Forwarded-For: %d %q", code, body)
	}
}

// A request body that stalls is dropped; one that keeps moving, however slowly overall, is not.
func TestBodyIdleTimeout(t *testing.T) {
	old := BodyIdle
	BodyIdle = 300 * time.Millisecond
	t.Cleanup(func() { BodyIdle = old })
	srv := route(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, n)
	}), nil)
	pr, pw := io.Pipe()
	go func() {
		for range 6 { // 1.2s in total, never 300ms without a byte
			pw.Write([]byte("x"))
			time.Sleep(200 * time.Millisecond)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", srv.URL, pr)
	if code, body := do(t, srv, req); code != 200 || body != "6" {
		t.Fatalf("slow but moving body: %d %q", code, body)
	}

	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "POST / HTTP/1.1\r\nHost: a.test\r\nContent-Length: 10\r\n\r\nab")
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("stalled body: no answer: %v", err)
	}
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("stalled body: %d", resp.StatusCode)
	}
}

// Connections past the per-ip cap are closed at accept; a slot frees when one closes.
func TestConnectionCaps(t *testing.T) {
	old := MaxConnsPerIP
	MaxConnsPerIP = 2
	t.Cleanup(func() { MaxConnsPerIP = old })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })}
	go srv.Serve(limitListener{l, &connLimiter{perIP: map[string]int{}}})
	t.Cleanup(func() { srv.Close() })
	get := func(c net.Conn) error {
		fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	var conns []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	for i, c := range conns[:2] {
		if err := get(c); err != nil {
			t.Fatalf("connection %d under the cap: %v", i, err)
		}
	}
	if err := get(conns[2]); err == nil {
		t.Fatal("a third connection from the same ip was served")
	}
	conns[0].Close()
	time.Sleep(100 * time.Millisecond)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := get(c); err != nil {
		t.Fatalf("after one closed: %v", err)
	}
}

// A failed write never replaces the table on disk, and is retried on the next save.
func TestSaveFailureKeepsTheOldFile(t *testing.T) {
	s := &Server{Home: t.TempDir(), table: NewTable()}
	s.table.SetRoutes([]Route{{Project: "a", Service: "web", Domains: []string{"a.test"}, Backends: []string{"127.0.0.1:1"}}})
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(TablePath(s.Home))
	writeTable = func(f *os.File, b []byte) error { f.Write(b[:len(b)/2]); return errors.New("disk full") }
	t.Cleanup(func() {
		writeTable = func(f *os.File, b []byte) error {
			if _, err := f.Write(b); err != nil {
				return err
			}
			return f.Sync()
		}
	})
	s.table.SetRoutes([]Route{{Project: "b", Service: "web", Domains: []string{"b.test"}, Backends: []string{"127.0.0.1:2"}}})
	if err := s.save(); err == nil {
		t.Fatal("save ignored a write error")
	}
	if after, _ := os.ReadFile(TablePath(s.Home)); string(after) != string(before) {
		t.Fatalf("a failed save changed the file: %s", after)
	}
	if entries, _ := os.ReadDir(s.Home); len(entries) != 1 {
		t.Fatalf("temp files left: %d entries", len(entries))
	}
}
