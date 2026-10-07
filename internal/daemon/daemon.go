// Package daemon wires everything together: sockets, listeners, TLS, auth and the api.
package daemon

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/notify"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/sdnotify"
	"github.com/sh-lucas/vops/internal/store"
	"github.com/sh-lucas/vops/internal/sysmon"
)

type Daemon struct {
	Home string // ~/.vops
	Repo string // ~/vops

	DB      *store.DB
	Reg     *registry.Registry
	Engine  *deploy.Engine
	Routes  *proxy.Table // the source of truth; every change is pushed to the proxy process
	Version string       // this build, for /api/plan

	cfg        atomic.Pointer[config.Config] // the lock (~/.vops/config.yml): what is in effect
	restart    chan struct{}                 // the ui listener changed: Run returns ErrRestart
	pullToken  string
	authCache  sync.Map     // sha256(user:pass) -> cachedAuth
	limiter    *authLimiter // wrong credentials per client, dashboard and registry together (limit.go)
	proxy      *proxy.Client
	pushMu     sync.Mutex
	proxyDirty atomic.Bool // the last push failed: retried every second
	webRoutes  []string    // patterns of the web api, as API(false) registered them (tests walk them)
	sys        *sysmon.Sampler
	backup     backup // the encrypted secrets backup (secrets.go)
	notify     *notify.Notifier
	monitor    *notify.Monitor
}

type cachedAuth struct {
	perm    *registry.Perm
	expires time.Time
}

// Paths on the host, relative to $HOME.
func Paths(home string) (vopsHome, repo string) {
	return filepath.Join(home, ".vops"), filepath.Join(home, "vops")
}

func SocketPath(vopsHome string) string { return filepath.Join(vopsHome, "vops.sock") }

// ErrRestart: the daemon must be restarted (re-exec'd) to apply a new ui listener.
var ErrRestart = errors.New("restart")

// LockPath is the host copy of the last applied vops.yml.
func LockPath(vopsHome string) string { return config.LockPath(vopsHome) }

// New opens the state. It does not listen yet.
func New(vopsHome, repo string) (*Daemon, error) {
	if err := os.MkdirAll(vopsHome, 0o700); err != nil {
		return nil, err
	}
	host, err := config.Read(LockPath(vopsHome))
	if err != nil {
		return nil, fmt.Errorf("%w (fix it, or delete it to start from vops.yml / defaults)", err)
	}
	if _, statErr := os.Stat(LockPath(vopsHome)); statErr != nil {
		// no lock yet (install normally writes one): start from the repo's vops.yml if it is valid
		if c, err := config.ReadRepo(repo); err == nil {
			host = c
		}
		if err := config.WriteLockFile(LockPath(vopsHome), host); err != nil {
			return nil, err
		}
	}
	db, err := store.Open(filepath.Join(vopsHome, "vops.db"), filepath.Join(vopsHome, "secret.key"))
	if err != nil {
		return nil, err
	}
	reg, err := registry.New(filepath.Join(repo, "registry", "data"))
	if err != nil {
		return nil, err
	}
	d := &Daemon{Home: vopsHome, Repo: repo, DB: db, Reg: reg, Routes: proxy.NewTable(), pullToken: store.Token(), restart: make(chan struct{}, 1), limiter: newAuthLimiter(), proxy: proxy.NewClient(vopsHome), sys: sysmon.New(vopsHome, repo)}
	d.cfg.Store(&host)
	d.Routes.OnChange = d.pushRoutes
	reg.Auth = d.registryAuth
	reg.OnPush = d.onPush
	d.Engine = &deploy.Engine{Repo: repo, DB: db, Routes: d.Routes, Registry: reg, PullAddr: loopback(host.UI), PullAuthFile: filepath.Join(vopsHome, "pull-auth.json"),
		SnapshotDir: filepath.Join(vopsHome, "snapshots"), SnapshotKeep: host.SnapshotKeep, SnapshotsOff: host.Snapshots == "off",
		PreviewDir: filepath.Join(vopsHome, "previews"),
		Config:     func() config.Config { return *d.cfg.Load() }, ApplyConfig: d.applyConfig}
	d.Engine.PreviewMax, d.Engine.PreviewTTL = host.Previews()
	auth := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, d.Engine.PullAddr, base64.StdEncoding.EncodeToString([]byte("vops-internal:"+d.pullToken)))
	if err := os.WriteFile(d.Engine.PullAuthFile, []byte(auth), 0o600); err != nil {
		return nil, err
	}
	if err := d.notifier(); err != nil {
		return nil, err
	}
	d.monitor = &notify.Monitor{N: d.notify, Engine: d.Engine, Sys: d.sys}
	return d, nil
}

// applyConfig makes a new vops.yml take effect: write the lock, update what can change live,
// tell the proxy (it re-execs itself for new http/https/tls), and restart the daemon for a new ui listener.
// Called by apply (engine lock held).
func (d *Daemon) applyConfig(c config.Config, w io.Writer) error {
	old := *d.cfg.Load()
	if err := config.WriteLockFile(LockPath(d.Home), c); err != nil {
		return err
	}
	d.cfg.Store(&c)
	d.Engine.SnapshotKeep, d.Engine.SnapshotsOff = c.SnapshotKeep, c.Snapshots == "off"
	d.Engine.PreviewMax, d.Engine.PreviewTTL = c.Previews()
	d.DB.Event("", "config", "vops.yml applied: %s", strings.Join(old.Diff(c), ", "))
	if err := d.refreshBackup(); err != nil {
		fmt.Fprintf(w, "secrets backup: ! %v\n", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if restart, err := d.proxy.Reload(ctx); err != nil {
		fmt.Fprintf(w, "vops.yml: ! the proxy did not take it (%v); it reads it when it starts\n", err)
	} else if restart {
		fmt.Fprintln(w, "vops.yml: listeners changed, the proxy restarts (sites blink for a moment; containers keep running)")
	}
	if old.UIListener(c) {
		fmt.Fprintln(w, "vops.yml: ui listener changed, the daemon restarts in a second (containers keep running)")
		go func() {
			time.Sleep(time.Second) // let the apply response finish
			d.restart <- struct{}{}
		}()
	}
	return nil
}

// pushRoutes sends the whole table to the proxy process and returns once the proxy serves it, so a rolling
// release drains the old replicas only after traffic moved. Its error is the table's (Routes.Set): a rolling
// release that can't get a confirmation puts the old routes back and fails; anything else goes on (the proxy
// keeps serving its last table) and the push is retried every second until it answers.
func (d *Daemon) pushRoutes() error {
	d.pushMu.Lock()
	defer d.pushMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.proxy.Push(ctx, d.Routes.Routes())
	switch was := d.proxyDirty.Swap(err != nil); {
	case err != nil && !was:
		log.Printf("proxy: %v (it serves its last table; retrying)", err)
		d.DB.Event("", "error", "proxy unreachable, routes not sent: %v (sites keep the routes it had; retrying every second)", err)
	case err == nil && was:
		log.Print("proxy: reachable again, routes sent")
		d.DB.Event("", "config", "proxy reachable again, routes sent")
	}
	return err
}

// ProxyState is what /api/status says about the proxy process.
type ProxyState struct {
	Up      bool   `json:"up"`
	Version int    `json:"version"`
	Binary  string `json:"binary,omitempty"`
	Routes  int    `json:"routes"`
	InSync  bool   `json:"in_sync"`
	Error   string `json:"error,omitempty"`

	Limited map[string]proxy.Counters `json:"limited,omitempty"` // per project: requests refused by x-vops limits
}

func (d *Daemon) proxyState(ctx context.Context) (ProxyState, []string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := d.proxy.Status(ctx)
	if err != nil {
		return ProxyState{Error: err.Error()}, []string{"the proxy is not running: sites are down until it is back (it restarts by itself; check: journalctl -u vops-proxy)"}
	}
	ours := d.Routes.Routes()
	ps := ProxyState{Up: true, Version: st.Version, Binary: st.Binary, Routes: st.Routes, InSync: st.Hash == proxy.Hash(ours), Limited: st.Limited}
	var warns []string
	if !ps.InSync {
		warns = append(warns, fmt.Sprintf("the proxy serves %d routes, the daemon has %d: they are resent every minute (if it lasts, restart vops-proxy)", st.Routes, len(ours)))
	}
	if st.Version != proxy.Version {
		warns = append(warns, fmt.Sprintf("the proxy runs protocol v%d, this vops expects v%d: run `vops install` again", st.Version, proxy.Version))
	}
	return ps, warns
}

// loopback turns ":9984" or "0.0.0.0:9984" into "127.0.0.1:9984" (what podman pulls from).
func loopback(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (d *Daemon) domain() string { return d.cfg.Load().Domain }

func (d *Daemon) uiHost() string {
	if d.domain() == "" {
		return ""
	}
	return "vops." + d.domain()
}

func (d *Daemon) registryHost() string {
	if d.domain() == "" {
		return ""
	}
	return "registry." + d.domain()
}

// ---- auth

// registryAuth: the internal user pulls everything; admins and global deployers push and pull everything,
// other deployers their repos. Push and pull are the same permission. Wrong credentials count against the
// client like failed logins (the internal user only comes from loopback and is never refused).
func (d *Daemon) registryAuth(r *http.Request, user, pass string) (*registry.Perm, error) {
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	if user == "vops-internal" {
		if subtle.ConstantTimeCompare([]byte(pass), []byte(d.pullToken)) == 1 {
			return &registry.Perm{Name: user, Pull: all, Push: none}, nil
		}
		return nil, nil
	}
	key := store.Hash(user + "\x00" + pass)
	if c, ok := d.authCache.Load(key); ok && time.Now().Before(c.(cachedAuth).expires) {
		return c.(cachedAuth).perm, nil
	}
	var u store.User
	ok, blocked, err := d.limiter.check(r, func() bool {
		var ok bool
		u, ok = d.DB.CheckUser(user, pass)
		return ok
	})
	if blocked {
		d.DB.Event("", "auth", "too many wrong registry credentials from %s (last as %q): refused for %s", r.RemoteAddr, user, authWindow)
	}
	if !ok {
		return nil, err
	}
	perm := &registry.Perm{Name: user, Pull: u.Allows, Push: u.Allows}
	d.authCache.Store(key, cachedAuth{perm, time.Now().Add(5 * time.Minute)})
	return perm, nil
}

// forgetAuth drops cached registry credentials: every user change calls it, so a deleted user or an old token
// stops working at once.
func (d *Daemon) forgetAuth() { d.authCache.Clear() }

// ---- push trigger (the watchtower replacement, and previews from tags)

func (d *Daemon) onPush(repo, tag, digest string) {
	d.DB.Event("", "push", "%s:%s pushed (%.19s)", repo, tag, digest)
	ctx := context.Background()
	plan, err := d.Engine.Plan(ctx)
	if err != nil {
		log.Printf("push trigger: %v", err)
		return
	}
	w := &logWriter{prefix: "push " + repo + ":" + tag + ": "}
	// a preview tag creates or updates a preview; Watching never returns it, so production is not touched
	targets, skipped := plan.PreviewTargets(repo, tag)
	for _, k := range skipped {
		d.DB.Event(k.Project, "preview", "%s:%s pushed: no preview for %s, it has no x-vops.preview", repo, tag, k.Service)
	}
	if _, preview := compose.PreviewName(tag); preview && len(targets) == 0 && len(skipped) == 0 {
		log.Printf("push %s:%s: no preview: no service runs %s", repo, tag, repo)
	}
	for _, t := range targets {
		if err := d.Engine.PreviewUp(ctx, w, deploy.PreviewOpts{Project: t.Project, Name: t.Name, Images: t.Images}); err != nil {
			log.Printf("push trigger %s:%s: preview %s of %s: %v", repo, tag, t.Name, t.Project, err)
			d.DB.Event(t.Project, "error", "preview %s from %s:%s: %v", t.Name, repo, tag, err)
		}
	}
	var keys []proxy.Key
	for _, sv := range plan.Watching(repo, tag) {
		keys = append(keys, sv)
	}
	if len(keys) == 0 {
		return
	}
	// a pinned service (image rollback) running this tag gets the newer version: its pin ends
	if _, err := d.Engine.Apply(ctx, w, deploy.ApplyOpts{Services: keys, Trigger: "push", Unpin: repo + ":" + tag + " pushed"}); err != nil {
		log.Printf("push trigger %s:%s: %v", repo, tag, err)
	}
}

type logWriter struct{ prefix string }

func (l *logWriter) Write(p []byte) (int, error) {
	for line := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
		log.Print(l.prefix + line)
	}
	return len(p), nil
}

// ---- serving

// WebHandler serves web.sock: what the proxy forwards for vops.<domain> (the ui) and registry.<domain>.
func (d *Daemon) WebHandler() http.Handler {
	ui := d.UIHandler()
	regOnly := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/") {
			d.Reg.ServeHTTP(w, r)
			return
		}
		http.Error(w, "this is a container registry: podman login "+r.Host, http.StatusNotFound)
	})
	return fromProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch h := proxy.Host(r.Host); {
		case h != "" && h == d.uiHost():
			ui.ServeHTTP(w, r)
		case h != "" && h == d.registryHost():
			regOnly.ServeHTTP(w, r)
		default:
			http.Error(w, "vops: no such site", http.StatusNotFound)
		}
	}))
}

// fromProxy trusts what the proxy forwarded (web.sock is 0600, only the proxy talks to it): the client's
// address for login events, and https, so session cookies stay Secure.
func fromProxy(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
			r.RemoteAddr = strings.TrimSpace(ip[strings.LastIndex(ip, ",")+1:])
		}
		if r.Header.Get("X-Forwarded-Proto") == "https" {
			r.TLS = &tls.ConnectionState{}
		}
		h.ServeHTTP(w, r)
	})
}

// UIHandler serves the dashboard, its api (session auth) and the registry (for loopback pulls and `vops ui` tunnels).
func (d *Daemon) UIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v2/", d.Reg)
	mux.Handle("/api/", d.sessionAuth(d.API(false)))
	mux.Handle("/", staticUI())
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v2") {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "same-origin")
			w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		}
		h.ServeHTTP(w, r)
	})
}

// Run serves until ctx is done. The daemon has no public listener: the proxy process owns :80/:443 and
// forwards the ui and registry hosts to web.sock.
func (d *Daemon) Run(ctx context.Context) error {
	log.SetFlags(0)
	cfg := *d.cfg.Load()
	var servers []*http.Server
	errc := make(chan error, 4)
	serve := func(name string, l net.Listener, h http.Handler) {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		servers = append(servers, srv)
		log.Printf("listening on %s (%s)", l.Addr(), name)
		go func() {
			if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}
	unix := func(path string) (net.Listener, error) {
		os.Remove(path)
		l, err := net.Listen("unix", path)
		if err == nil {
			os.Chmod(path, 0o600)
		}
		return l, err
	}
	sl, err := unix(SocketPath(d.Home))
	if err != nil {
		return err
	}
	serve("socket", sl, d.API(true))
	wl, err := unix(proxy.WebSocket(d.Home))
	if err != nil {
		return err
	}
	serve("web, from the proxy", wl, d.WebHandler())
	if cfg.UI != "" && cfg.UI != "off" {
		l, err := net.Listen("tcp", cfg.UI)
		if err != nil {
			return fmt.Errorf("ui: %w", err)
		}
		serve("ui", l, d.UIHandler())
	}
	sdnotify.Notify("READY=1")
	go sdnotify.Watchdog(ctx, d.ping)
	d.backupNow()

	// after a reboot: start what should run; this also sends the routes to the proxy
	d.Engine.StartStopped(ctx, &logWriter{prefix: "boot: "})
	go d.retryProxy(ctx)
	go d.housekeeping(ctx)
	d.monitor.Run(ctx)
	select {
	case <-ctx.Done():
	case err = <-errc:
	case <-d.restart:
		err = ErrRestart
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(shutdown)
	}
	d.notify.Wait()
	d.DB.Close()
	return err
}

// ping is the watchdog's health check: the daemon's own socket answers.
func (d *Daemon) ping(ctx context.Context) error {
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", SocketPath(d.Home))
	}}}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://vops/api/ping", nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New(resp.Status)
	}
	return nil
}

// retryProxy resends the table every second while the proxy is unreachable.
func (d *Daemon) retryProxy(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if d.proxyDirty.Load() {
			d.pushRoutes()
		}
	}
}

// housekeeping: daily registry GC of uploads, route refresh in case a container restarted on its own
// (which also resends the whole table to the proxy, so drift heals), expired previews and sessions.
func (d *Daemon) housekeeping(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n++
		d.Engine.Lock(func() { d.Engine.RefreshRoutes(ctx) })
		d.Engine.ExpirePreviews(ctx, &logWriter{prefix: "previews: "})
		d.DB.PurgeSessions()
		d.backupNow() // authorized_keys may have changed
		d.monitor.Reconcile(ctx)
		if n%(24*60) == 0 {
			if res, err := d.gc(); err == nil && res.Blobs > 0 {
				d.DB.Event("", "gc", "registry gc: %d manifests, %d blobs, %d bytes freed", res.Manifests, res.Blobs, res.Freed)
			}
		}
	}
}

// gc collects the registry, keeping what pins, previews and the newest image_keep deploys of every project run
// (untagged once a tag moves on), so rollback and "Preview from here" still find them.
func (d *Daemon) gc() (registry.GCResult, error) {
	keep, err := d.Engine.KeepDigests(d.cfg.Load().Defaults().ImageKeep)
	if err != nil {
		return registry.GCResult{}, err
	}
	return d.Reg.GC(time.Hour, keep)
}
