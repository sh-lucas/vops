package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/sdnotify"
)

// Version is the proxy's behaviour plus the daemon<->proxy protocol. Bump it when either changes:
// `vops setup` restarts the running proxy (every site blinks) only when it reports another Version.
const Version = 4

// ControlSocket is where the proxy takes its routing table from the daemon.
func ControlSocket(vopsHome string) string { return filepath.Join(vopsHome, "proxy.sock") }

// WebSocket is where the daemon serves vops.<domain> and registry.<domain> to the proxy.
func WebSocket(vopsHome string) string { return filepath.Join(vopsHome, "web.sock") }

// TablePath is the last table the proxy received; it starts from it, so it needs no daemon to serve.
func TablePath(vopsHome string) string { return filepath.Join(vopsHome, "routes.json") }

// ErrRestart: the listeners changed, the proxy must be re-exec'd.
var ErrRestart = errors.New("restart")

type Status struct {
	Version int    `json:"version"`
	Binary  string `json:"binary"` // the vops build it runs
	PID     int    `json:"pid"`
	Routes  int    `json:"routes"`
	Hash    string `json:"hash"`
	Started int64  `json:"started"`
	Synced  int64  `json:"synced"` // last table from the daemon (unix seconds); 0: only what was on disk

	Limited map[string]Counters `json:"limited,omitempty"` // requests refused by x-vops limits (429, 413), per project
}

type tableFile struct {
	Routes []Route `json:"routes"`
}

// Server is the `vops proxy` process: :80/:443, certificates, the routing table.
type Server struct {
	Home   string
	Binary string

	table   *Table
	cfg     atomic.Pointer[config.Config]
	restart chan struct{}
	started time.Time
	synced  atomic.Int64
	saveMu  sync.Mutex
	saved   string // hash of what is on disk
}

func NewServer(vopsHome, binary string) (*Server, error) {
	cfg, err := config.Read(config.LockPath(vopsHome))
	if err != nil {
		return nil, err
	}
	s := &Server{Home: vopsHome, Binary: binary, table: NewTable(), restart: make(chan struct{}, 1), started: time.Now()}
	s.cfg.Store(&cfg)
	if b, err := os.ReadFile(TablePath(vopsHome)); err == nil {
		var f tableFile
		if err := json.Unmarshal(b, &f); err != nil {
			log.Printf("%s: %v (starting empty, the daemon sends the table)", TablePath(vopsHome), err)
		} else {
			s.table.SetRoutes(f.Routes)
			s.saved = Hash(s.table.Routes())
			log.Printf("loaded %d routes from %s", len(f.Routes), TablePath(vopsHome))
		}
	}
	return s, nil
}

func (s *Server) domain() string { return s.cfg.Load().Domain }

func (s *Server) daemonHost(h string) bool {
	d := s.domain()
	return d != "" && (h == "vops."+d || h == "registry."+d)
}

// hostPolicy only lets autocert ask for certificates of hosts we serve.
func (s *Server) hostPolicy(_ context.Context, host string) error {
	if s.daemonHost(host) || s.table.Has(host) {
		return nil
	}
	return fmt.Errorf("vops: unknown host %q", host)
}

func (s *Server) status() Status {
	routes := s.table.Routes()
	return Status{Version, s.Binary, os.Getpid(), len(routes), Hash(routes), s.started.Unix(), s.synced.Load(), s.table.Limited()}
}

// save writes the table atomically (write + rename). The proxy is the only writer: the file is always
// exactly what it serves.
func (s *Server) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	routes := s.table.Routes()
	h := Hash(routes)
	if h == s.saved {
		return nil
	}
	b, err := json.Marshal(tableFile{routes})
	if err != nil {
		return err
	}
	path := TablePath(s.Home)
	f, err := os.CreateTemp(filepath.Dir(path), ".routes-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = writeTable(f, b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	s.saved = h
	return nil
}

// writeTable writes and fsyncs the table file (tests make it fail).
var writeTable = func(f *os.File, b []byte) error {
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

// control is the api on proxy.sock (0600, the daemon and `vops setup` use it).
func (s *Server) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { json.MarshalWrite(w, s.status()) })
	mux.HandleFunc("GET /routes", func(w http.ResponseWriter, r *http.Request) { json.MarshalWrite(w, s.table.Routes()) })
	// PUT /routes swaps the whole table and answers once it serves it (a rolling release drains right after)
	mux.HandleFunc("PUT /routes", func(w http.ResponseWriter, r *http.Request) {
		var f tableFile
		if err := json.UnmarshalRead(io.LimitReader(r.Body, 16<<20), &f); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.table.SetRoutes(f.Routes)
		s.synced.Store(time.Now().Unix())
		if err := s.save(); err != nil {
			log.Printf("saving routes: %v", err)
		}
		json.MarshalWrite(w, s.status())
	})
	// POST /reload re-reads the config lock: the domain applies live, new listeners re-exec the proxy
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) {
		c, err := config.Read(config.LockPath(s.Home))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		old := s.cfg.Load()
		restart := old.ProxyListeners(c)
		if !restart {
			s.cfg.Store(&c)
		}
		json.MarshalWrite(w, map[string]bool{"restart": restart})
		if restart {
			select {
			case s.restart <- struct{}{}:
			default:
			}
		}
	})
	return mux
}

// webProxy forwards vops.<domain> and registry.<domain> to the daemon, streaming both ways
// (registry pushes are big uploads, logs are long responses): no body limits, no read timeouts.
func webProxy(sock string) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host = "http", "vops"
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", sock)
			},
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if bodyTimedOut(r) {
				w.Header().Set("Connection", "close")
				http.Error(w, "vops: request body timed out", http.StatusRequestTimeout)
				return
			}
			if r.Context().Err() == nil {
				log.Printf("%s: daemon: %v", r.Host, err)
			}
			http.Error(w, "vops: the daemon is not running, so the dashboard and the registry are down (sites keep working). it restarts by itself; check: journalctl -u vops", http.StatusBadGateway)
		},
	}
}

func (s *Server) handler() http.Handler {
	routes, web := Handler(s.table), webProxy(WebSocket(s.Home))
	return idleBodies(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.daemonHost(Host(r.Host)) {
			web.ServeHTTP(w, r)
			return
		}
		routes.ServeHTTP(w, r)
	}))
}

// BodyIdle drops a request whose body sends nothing for this long. Not a total ReadTimeout: big uploads
// that keep moving are fine, a stalled one doesn't hold its connection forever.
var BodyIdle = 30 * time.Second

func idleBodies(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			h.ServeHTTP(w, r)
			return
		}
		b := &idleBody{rc: r.Body, ctl: http.NewResponseController(w), timedOut: new(atomic.Bool)}
		r = r.WithContext(context.WithValue(r.Context(), bodyTimeoutKey{}, b.timedOut))
		r.Body = b
		defer b.finish()
		h.ServeHTTP(w, r)
	})
}

type bodyTimeoutKey struct{}

// bodyTimedOut: the request failed because its body stalled (the proxy's error is only "context canceled").
func bodyTimedOut(r *http.Request) bool {
	t, _ := r.Context().Value(bodyTimeoutKey{}).(*atomic.Bool)
	return t != nil && t.Load()
}

// idleBody sets the connection's read deadline before each read of the body and clears it at EOF, so a
// streamed response (SSE) that outlives the body isn't cut by it.
type idleBody struct {
	rc       io.ReadCloser
	ctl      *http.ResponseController
	mu       sync.Mutex
	done     bool // the handler returned: the connection is not ours anymore
	timedOut *atomic.Bool
}

func (b *idleBody) deadline(t time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.done {
		b.ctl.SetReadDeadline(t)
	}
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.deadline(time.Now().Add(BodyIdle))
	n, err := b.rc.Read(p)
	switch {
	case err == io.EOF:
		b.deadline(time.Time{})
	case errors.Is(err, os.ErrDeadlineExceeded):
		b.timedOut.Store(true) // the deadline stays: the server must not wait for the rest of the body either
	}
	return n, err
}

func (b *idleBody) Close() error { return b.rc.Close() }

func (b *idleBody) finish() {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
}

// Connection caps on the public listeners, per client ip and in total; no real client gets near them.
var (
	MaxConnsPerIP = 256
	MaxConns      = 10000
)

type connLimiter struct {
	mu    sync.Mutex
	total int
	perIP map[string]int
}

// limitListener closes accepted connections past the caps right away.
type limitListener struct {
	net.Listener
	l *connLimiter
}

func (ll limitListener) Accept() (net.Conn, error) {
	for {
		c, err := ll.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		ll.l.mu.Lock()
		ok := ll.l.total < MaxConns && ll.l.perIP[ip] < MaxConnsPerIP
		if ok {
			ll.l.total++
			ll.l.perIP[ip]++
		}
		ll.l.mu.Unlock()
		if ok {
			return &countedConn{Conn: c, release: sync.OnceFunc(func() { ll.l.release(ip) })}, nil
		}
		c.Close()
	}
}

func (l *connLimiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
}

type countedConn struct {
	net.Conn
	release func()
}

func (c *countedConn) Close() error {
	c.release()
	return c.Conn.Close()
}

func hsts(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		h.ServeHTTP(w, r)
	})
}

// Run serves until ctx is done, or returns ErrRestart when the listeners changed.
func (s *Server) Run(ctx context.Context) error {
	log.SetFlags(0)
	cfg := *s.cfg.Load()
	var servers []*http.Server
	errc := make(chan error, 4)
	serve := func(name string, l net.Listener, h http.Handler, tlsCfg *tls.Config) {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10, TLSConfig: tlsCfg}
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
	defer func() {
		// short: nothing listens until the next process starts, so sites wait on this
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, srv := range servers {
			if srv.Shutdown(shutdown) != nil {
				srv.Close()
			}
		}
	}()

	sock := ControlSocket(s.Home)
	os.Remove(sock)
	cl, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o600)
	serve("control", cl, s.control(), nil)

	main := s.handler()
	conns := &connLimiter{perIP: map[string]int{}} // shared by http and https
	listen := func(addr string) (net.Listener, error) {
		if addr == "" || addr == "off" {
			return nil, nil
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		return limitListener{l, conns}, nil
	}
	if cfg.TLS != "off" && cfg.HTTPS != "" && cfg.HTTPS != "off" {
		// always listen: the domain may only arrive with the first sync
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(filepath.Join(s.Home, "certs")),
			Email:      cfg.Email,
			HostPolicy: s.hostPolicy,
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
				if s.domain() == "" {
					main.ServeHTTP(w, r)
					return
				}
				if s.hostPolicy(r.Context(), Host(r.Host)) != nil {
					http.NotFound(w, r)
					return
				}
				http.Redirect(w, r, "https://"+Host(r.Host)+r.URL.RequestURI(), http.StatusMovedPermanently)
			})), nil)
		}
	} else if l, err := listen(cfg.HTTP); err != nil {
		return fmt.Errorf("http: %w", err)
	} else if l != nil {
		serve("http, tls off", l, main, nil)
	}

	sdnotify.Notify("READY=1")
	go sdnotify.Watchdog(ctx, NewClient(s.Home).Ping) // healthy = its own control socket answers
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	case <-s.restart:
		time.Sleep(200 * time.Millisecond) // let the reload response reach the daemon
		return ErrRestart
	}
}
