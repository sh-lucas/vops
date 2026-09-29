package cli

import (
	"encoding/json/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sh-lucas/vops/internal/proxy"
)

// fakeProxy answers /status on the control socket like a running proxy of that version.
func fakeProxy(t *testing.T, vh string, version int) {
	l, err := net.Listen("unix", proxy.ControlSocket(vh))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.MarshalWrite(w, proxy.Status{Version: version})
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
}

func TestSetupRestartsTheProxyOnlyWhenNeeded(t *testing.T) {
	home := t.TempDir()
	vh := filepath.Join(home, ".vops")
	os.MkdirAll(vh, 0o700)
	var calls []string
	u := units{vh: vh, home: home, dir: filepath.Join(home, "units"), wanted: "default.target", journal: "journalctl",
		systemctl:  func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil },
		syncRoutes: func() error { return nil },
		say:        func(string, ...any) {}, warn: func(string, ...any) {}}
	install := func() []string {
		t.Helper()
		calls = nil
		if err := u.install(); err != nil {
			t.Fatal(err)
		}
		return calls
	}

	// first install (or from v1.2: no proxy unit, the old daemon holds the ports): daemon first, then the proxy
	c := install()
	d, p := slices.Index(c, "restart vops.service"), slices.Index(c, "restart vops-proxy.service")
	if d < 0 || p < d {
		t.Fatalf("first install must restart the daemon, then start the proxy: %v", c)
	}
	unit, _ := os.ReadFile(filepath.Join(u.dir, "vops-proxy.service"))
	if !strings.Contains(string(unit), "proxy\n") || !strings.Contains(string(unit), "WatchdogSec=") || !strings.Contains(string(unit), "Type=notify") {
		t.Fatalf("proxy unit:\n%s", unit)
	}

	// upgrade, same proxy.Version running, unit unchanged: the daemon restarts, the proxy doesn't
	fakeProxy(t, vh, proxy.Version)
	if c := install(); !slices.Contains(c, "restart vops.service") || slices.Contains(c, "restart vops-proxy.service") {
		t.Fatalf("same proxy version must not restart the proxy: %v", c)
	}

	// a running proxy of another version is restarted
	os.Remove(proxy.ControlSocket(vh))
	fakeProxy(t, vh, proxy.Version-1)
	if c := install(); !slices.Contains(c, "restart vops-proxy.service") {
		t.Fatalf("another proxy version must be restarted: %v", c)
	}
}
