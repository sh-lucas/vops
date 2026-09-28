// Package daemon wires everything together: sockets, listeners, TLS, auth and the api.
package daemon

import (
	"context"
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
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/store"
)

type Daemon struct {
	Home string // ~/.vops
	Repo string // ~/vops

	DB     *store.DB
	Reg    *registry.Registry
	Engine *deploy.Engine
	Routes *proxy.Table

	cfg       atomic.Pointer[config.Config] // the lock (~/.vops/config.yml): what is in effect
	restart   chan struct{}                 // listeners changed: Run returns ErrRestart
	pullToken string
	authCache sync.Map // sha256(user:pass) -> cachedAuth
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

// ErrRestart: the daemon must be restarted (re-exec'd) to apply new listeners.
var ErrRestart = errors.New("restart")

// LockPath is the host copy of the last applied vops.yml.
func LockPath(vopsHome string) string { return filepath.Join(vopsHome, "config.yml") }

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
	d := &Daemon{Home: vopsHome, Repo: repo, DB: db, Reg: reg, Routes: proxy.NewTable(), pullToken: store.Token(), restart: make(chan struct{}, 1)}
	d.cfg.Store(&host)
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
	return d, nil
}

// applyConfig makes a new vops.yml take effect: write the lock, update what can change live,
// and restart the daemon when listeners change. Called by apply (engine lock held).
func (d *Daemon) applyConfig(c config.Config, w io.Writer) error {
	old := *d.cfg.Load()
	if err := config.WriteLockFile(LockPath(d.Home), c); err != nil {
		return err
	}
	d.cfg.Store(&c)
	d.Engine.SnapshotKeep, d.Engine.SnapshotsOff = c.SnapshotKeep, c.Snapshots == "off"
	d.Engine.PreviewMax, d.Engine.PreviewTTL = c.Previews()
	d.DB.Event("", "config", "vops.yml applied: %s", strings.Join(old.Diff(c), ", "))
	if old.Listeners(c) {
		fmt.Fprintln(w, "vops.yml: listeners changed, the daemon restarts in a second (containers keep running)")
		go func() {
			time.Sleep(time.Second) // let the apply response finish
			d.restart <- struct{}{}
		}()
	}
	return nil
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

// registryAuth: the internal user pulls everything; admin pulls everything and pushes nothing; users follow their rules.
func (d *Daemon) registryAuth(user, pass string) *registry.Perm {
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	if user == "vops-internal" {
		if pass == d.pullToken {
			return &registry.Perm{Name: user, Pull: all, Push: none}
		}
		return nil
	}
	key := store.Hash(user + "\x00" + pass)
	if c, ok := d.authCache.Load(key); ok && time.Now().Before(c.(cachedAuth).expires) {
		return c.(cachedAuth).perm
	}
	var perm *registry.Perm
	if user == "admin" {
		if d.DB.CheckAdmin(pass) {
			perm = &registry.Perm{Name: user, Pull: all, Push: none}
		}
	} else if u, ok := d.DB.CheckUser(user, pass); ok {
		allowed := UserAllows(u)
		perm = &registry.Perm{Name: user, Pull: allowed, Push: allowed}
	}
	if perm != nil {
		d.authCache.Store(key, cachedAuth{perm, time.Now().Add(5 * time.Minute)})
	}
	return perm
}

// UserAllows reports which repos a registry user may push and pull: exact names, or the anchored regex.
func UserAllows(u store.User) func(string) bool {
	var re *regexp.Regexp
	if u.Pattern != "" {
		re, _ = regexp.Compile("^(?:" + u.Pattern + ")$")
	}
	return func(repo string) bool {
		return slices.Contains(u.Repos, repo) || re != nil && re.MatchString(repo)
	}
}

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
	for _, t := range plan.PreviewTargets(repo, tag) {
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

// Handler routes by Host: vops.<domain> is the ui, registry.<domain> the registry, anything else the proxy.
func (d *Daemon) Handler() http.Handler {
	ui := d.UIHandler()
	regOnly := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/") {
			d.Reg.ServeHTTP(w, r)
			return
		}
		http.Error(w, "this is a container registry: podman login "+r.Host, http.StatusNotFound)
	})
	routes := proxy.Handler(d.Routes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch h := proxy.Host(r.Host); {
		case h != "" && h == d.uiHost():
			ui.ServeHTTP(w, r)
		case h != "" && h == d.registryHost():
			regOnly.ServeHTTP(w, r)
		default:
			routes.ServeHTTP(w, r)
		}
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
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		}
		h.ServeHTTP(w, r)
	})
}

// Run serves until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	log.SetFlags(0)
	d.Engine.StartStopped(ctx, &logWriter{prefix: "boot: "})

	cfg := *d.cfg.Load()
	var servers []*http.Server
	errc := make(chan error, 4)
	serve := func(name string, l net.Listener, h http.Handler, tlsCfg *tls.Config) {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, TLSConfig: tlsCfg}
		servers = append(servers, srv)
		log.Printf("listening on %s (%s)", l.Addr(), name)
		go func() {
			var err error
			if tlsCfg != nil {
				err = srv.ServeTLS(l, "", "")
			} else {
				err = srv.Serve(l)
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	sock := SocketPath(d.Home)
	os.Remove(sock)
	sl, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o600)
	serve("socket", sl, d.API(true), nil)

	if cfg.UI != "" && cfg.UI != "off" {
		l, err := net.Listen("tcp", cfg.UI)
		if err != nil {
			return fmt.Errorf("ui: %w", err)
		}
		serve("ui", l, d.UIHandler(), nil)
	}

	main := d.Handler()
	listen := func(addr string) (net.Listener, error) {
		if addr == "" || addr == "off" {
			return nil, nil
		}
		return net.Listen("tcp", addr)
	}
	if cfg.TLS != "off" && cfg.HTTPS != "" && cfg.HTTPS != "off" {
		// always listen: the domain may only arrive with the first sync
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(filepath.Join(d.Home, "certs")),
			Email:      cfg.Email,
			HostPolicy: d.hostPolicy,
		}
		if cfg.ACMEDirectory != "" {
			m.Client = &acme.Client{DirectoryURL: cfg.ACMEDirectory}
		}
		l, err := listen(cfg.HTTPS)
		if err != nil {
			return fmt.Errorf("https: %w", err)
		}
		tlsCfg := m.TLSConfig()
		tlsCfg.MinVersion = tls.VersionTLS12
		serve("https", l, hsts(main), tlsCfg)
		if l, err := listen(cfg.HTTP); err != nil {
			return fmt.Errorf("http: %w", err)
		} else if l != nil {
			// acme challenges, then https redirects for hosts we serve; plain http until a domain is set
			serve("http", l, m.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if d.domain() == "" {
					main.ServeHTTP(w, r)
					return
				}
				if d.hostPolicy(r.Context(), proxy.Host(r.Host)) != nil {
					http.NotFound(w, r)
					return
				}
				http.Redirect(w, r, "https://"+proxy.Host(r.Host)+r.URL.RequestURI(), http.StatusMovedPermanently)
			})), nil)
		}
	} else if l, err := listen(cfg.HTTP); err != nil {
		return fmt.Errorf("http: %w", err)
	} else if l != nil {
		serve("http, tls off", l, main, nil)
	}

	go d.housekeeping(ctx)
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
	d.DB.Close()
	return err
}

func hsts(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		h.ServeHTTP(w, r)
	})
}

// hostPolicy only lets autocert ask for certificates of hosts we serve.
func (d *Daemon) hostPolicy(_ context.Context, host string) error {
	if host == d.uiHost() || host == d.registryHost() || d.Routes.Has(host) {
		return nil
	}
	return fmt.Errorf("vops: unknown host %q", host)
}

// housekeeping: daily registry GC of uploads, route refresh in case a container restarted on its own,
// expired previews.
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
