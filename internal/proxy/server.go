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
const Version = 2

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
	if _, err := f.Write(b); err == nil {
		err = f.Sync()
	}
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
			if r.Context().Err() == nil {
				log.Printf("%s: daemon: %v", r.Host, err)
			}
			http.Error(w, "vops: the daemon is not running, so the dashboard and the registry are down (sites keep working). it restarts by itself; check: journalctl -u vops", http.StatusBadGateway)
		},
	}
}

func (s *Server) handler() http.Handler {
	routes, web := Handler(s.table), webProxy(WebSocket(s.Home))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.daemonHost(Host(r.Host)) {
			web.ServeHTTP(w, r)
			return
		}
		routes.ServeHTTP(w, r)
	})
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
