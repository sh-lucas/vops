package proxy

import (
	"context"
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

	"github.com/sh-lucas/vops/internal/config"
)

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// start runs a proxy server in vh until the test ends; stop cancels it, done gets what Run returned.
func start(t *testing.T, vh string) (stop func(), done chan error) {
	t.Helper()
	s, err := NewServer(vh, "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done, exited := make(chan error, 1), make(chan struct{})
	go func() { done <- s.Run(ctx); close(exited) }()
	stop = func() { cancel(); <-exited }
	t.Cleanup(stop)
	c := NewClient(vh)
	for i := 0; c.Ping(context.Background()) != nil; i++ {
		if i > 100 {
			t.Fatal("proxy did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return stop, done
}

func get(t *testing.T, addr, host string) (int, string) {
	req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServerTableDiskAndDaemonForwarding(t *testing.T) {
	vh := t.TempDir()
	addr := freeAddr(t)
	cfg := config.Config{Domain: "x.test", HTTP: addr, HTTPS: "off", UI: "off", TLS: "off"}.Defaults()
	if err := config.WriteLockFile(config.LockPath(vh), cfg); err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "app") }))
	defer app.Close()

	stop, _ := start(t, vh)
	st, err := NewClient(vh).Push(context.Background(), []Route{{Project: "a", Service: "web", Domains: []string{"a.x.test"}, Backends: []string{strings.TrimPrefix(app.URL, "http://")}}})
	if err != nil || st.Routes != 1 || st.Version != Version {
		t.Fatalf("push: %+v %v", st, err)
	}
	if code, body := get(t, addr, "a.x.test"); code != 200 || body != "app" {
		t.Fatalf("site: %d %q", code, body)
	}
	// the daemon is down: its hosts say so, sites keep working
	if code, body := get(t, addr, "vops.x.test"); code != 502 || !strings.Contains(body, "daemon is not running") {
		t.Fatalf("ui without daemon: %d %q", code, body)
	}

	// the daemon on web.sock: big uploads stream through, the scheme and client ip are forwarded
	wl, err := net.Listen("unix", WebSocket(vh))
	if err != nil {
		t.Fatal(err)
	}
	daemon := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "%s %d %s", r.Host, n, r.Header.Get("X-Forwarded-For"))
	})}
	go daemon.Serve(wl)
	defer daemon.Close()
	const size = 64 << 20
	req, _ := http.NewRequest("PUT", "http://"+addr+"/v2/x/blobs/uploads/1", io.LimitReader(zeros{}, size))
	req.Host = "registry.x.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := fmt.Sprintf("registry.x.test %d 127.0.0.1", size); string(b) != want {
		t.Fatalf("upload through the proxy: %q, want %q", b, want)
	}

	// a restart starts from the table on disk, no daemon needed
	stop()
	if _, err := os.Stat(TablePath(vh)); err != nil {
		t.Fatal(err)
	}
	_, done := start(t, vh)
	if code, body := get(t, addr, "a.x.test"); code != 200 || body != "app" {
		t.Fatalf("after restart: %d %q", code, body)
	}

	// the domain applies live; a new listener re-execs (Run returns ErrRestart)
	cfg.Domain = "y.test"
	config.WriteLockFile(config.LockPath(vh), cfg)
	if restart, err := NewClient(vh).Reload(context.Background()); err != nil || restart {
		t.Fatalf("domain reload: %v %v", restart, err)
	}
	if code, body := get(t, addr, "vops.y.test"); code != 200 || !strings.HasPrefix(body, "vops.y.test ") {
		t.Fatalf("new domain not live: %d %q", code, body)
	}
	cfg.HTTP = freeAddr(t)
	config.WriteLockFile(config.LockPath(vh), cfg)
	if restart, err := NewClient(vh).Reload(context.Background()); err != nil || !restart {
		t.Fatalf("listener reload: %v %v", restart, err)
	}
	if err := <-done; !errors.Is(err, ErrRestart) {
		t.Fatalf("run: %v", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
