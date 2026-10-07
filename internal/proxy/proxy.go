// Package proxy is the routing table and reverse proxy in front of every routed container.
package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Key struct{ Project, Service string }

type entry struct {
	domains  []string
	backends []string
	next     *atomic.Uint64
}

// Table maps hosts to backends. Services own their domains; Set replaces a service atomically.
type Table struct {
	mu       sync.RWMutex
	services map[Key]entry
	hosts    map[string]Key
	limits   map[string]*Limits // per project; a preview without its own uses its project's
	limiter  *limiter

	// OnChange runs after every Set/Replace, synchronously (the daemon pushes the table to the proxy process with it).
	// Its error (the proxy did not confirm) is what Set and Replace return.
	OnChange func() error
}

func NewTable() *Table {
	return &Table{services: map[Key]entry{}, hosts: map[string]Key{}, limits: map[string]*Limits{}, limiter: newLimiter()}
}

// Set replaces the domains and backends ("127.0.0.1:port") of a service. No backends removes it.
func (t *Table) Set(k Key, domains, backends []string) error {
	t.mu.Lock()
	if len(backends) == 0 || len(domains) == 0 {
		delete(t.services, k)
	} else {
		t.services[k] = entry{slices.Clone(domains), slices.Clone(backends), new(atomic.Uint64)}
	}
	t.reindex()
	t.mu.Unlock()
	return t.changed()
}

// Replace swaps the whole table and the projects' limits (used when rebuilding from podman).
func (t *Table) Replace(all map[Key][2][]string, limits map[string]Limits) error {
	t.mu.Lock()
	t.limits = map[string]*Limits{}
	for p, l := range limits {
		if l.set() {
			t.limits[p] = &l
		}
	}
	t.services = map[Key]entry{}
	for k, v := range all {
		if len(v[0]) > 0 && len(v[1]) > 0 {
			t.services[k] = entry{v[0], v[1], new(atomic.Uint64)}
		}
	}
	t.reindex()
	t.mu.Unlock()
	return t.changed()
}

// SetRoutes swaps the whole table for a list of routes (what the proxy process receives).
func (t *Table) SetRoutes(routes []Route) {
	all, limits := map[Key][2][]string{}, map[string]Limits{}
	for _, r := range routes {
		all[Key{r.Project, r.Service}] = [2][]string{r.Domains, r.Backends}
		if r.Limits != nil {
			limits[r.Project] = *r.Limits
		}
	}
	t.Replace(all, limits) // the proxy process has no OnChange
}

// limitsOf is a project's limits; previews (project@name) inherit their project's.
func (t *Table) limitsOf(project string) *Limits {
	if l := t.limits[project]; l != nil {
		return l
	}
	base, _, _ := strings.Cut(project, "@")
	return t.limits[base]
}

// Limited is what the limits refused so far, per project.
func (t *Table) Limited() map[string]Counters { return t.limiter.snapshot() }

func (t *Table) changed() error {
	if t.OnChange != nil {
		return t.OnChange()
	}
	return nil
}

func (t *Table) reindex() {
	t.hosts = map[string]Key{}
	keys := make([]Key, 0, len(t.services))
	for k := range t.services {
		keys = append(keys, k)
	}
	// deterministic winner when two services claim a domain
	slices.SortFunc(keys, func(a, b Key) int { return strings.Compare(a.Project+"/"+a.Service, b.Project+"/"+b.Service) })
	for _, k := range keys {
		for _, d := range t.services[k].domains {
			if _, taken := t.hosts[d]; !taken {
				t.hosts[d] = k
			}
		}
	}
}

// Has reports whether host is routed (used by the TLS host policy).
func (t *Table) Has(host string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.hosts[host]
	return ok
}

// Backends of a host, round-robin ordered starting at the next one.
func (t *Table) Backends(host string) []string {
	b, _, _ := t.lookup(host)
	return b
}

// lookup is a host's backends (round-robin ordered), its project and that project's limits.
func (t *Table) lookup(host string) ([]string, string, *Limits) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	k, ok := t.hosts[host]
	if !ok {
		return nil, "", nil
	}
	e := t.services[k]
	n := len(e.backends)
	start := int(e.next.Add(1) % uint64(n))
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, e.backends[(start+i)%n])
	}
	return out, k.Project, t.limitsOf(k.Project)
}

type Route struct {
	Project  string   `json:"project"`
	Service  string   `json:"service"`
	Domains  []string `json:"domains"`
	Backends []string `json:"backends"`
	Limits   *Limits  `json:"limits,omitempty"`
}

func (t *Table) Routes() []Route {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := []Route{}
	for k, e := range t.services {
		out = append(out, Route{k.Project, k.Service, e.domains, e.backends, t.limitsOf(k.Project)})
	}
	slices.SortFunc(out, func(a, b Route) int { return strings.Compare(a.Project+"/"+a.Service, b.Project+"/"+b.Service) })
	return out
}

// Hash identifies a table's content, so the daemon and the proxy can tell whether they agree.
func Hash(routes []Route) string {
	b, _ := json.Marshal(routes)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Host normalizes a Host header: lowercase, no port.
func Host(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

type ctxKey struct{}

type timeoutKey struct{}

// DefaultTimeout is how long the proxy waits for a container's response headers (x-vops.timeout overrides it).
const DefaultTimeout = 60 * time.Second

var errUpstreamTimeout = errors.New("upstream response header timeout")

// headerTimeout bounds the wait for the response headers only, counted once the request is written: a slow
// upload or a streamed body (SSE, downloads) is never cut. Per request, since the timeout is per project and
// the Transport is shared.
type headerTimeout struct{ rt http.RoundTripper }

func (h headerTimeout) RoundTrip(r *http.Request) (*http.Response, error) {
	d, _ := r.Context().Value(timeoutKey{}).(time.Duration)
	if d <= 0 {
		return h.rt.RoundTrip(r)
	}
	ctx, cancel := context.WithCancel(r.Context())
	var (
		mu    sync.Mutex
		timer *time.Timer
		done  bool
		fired atomic.Bool
	)
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			timer = time.AfterFunc(d, func() { fired.Store(true); cancel() })
		}
	}}
	resp, err := h.rt.RoundTrip(r.WithContext(httptrace.WithClientTrace(ctx, trace)))
	mu.Lock()
	done = true
	if timer != nil {
		timer.Stop()
	}
	mu.Unlock()
	if fired.Load() {
		if err == nil {
			resp.Body.Close()
		}
		cancel()
		return nil, errUpstreamTimeout
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if rwc, ok := resp.Body.(io.ReadWriteCloser); ok && resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body = cancelConn{rwc, cancel} // websockets: ReverseProxy needs a writable body
		return resp, nil
	}
	resp.Body = cancelBody{resp.Body, cancel}
	return resp, nil
}

type cancelConn struct {
	io.ReadWriteCloser
	cancel context.CancelFunc
}

func (c cancelConn) Close() error {
	err := c.ReadWriteCloser.Close()
	c.cancel()
	return err
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// Handler proxies to the table, round-robin over replicas.
func Handler(t *Table) http.Handler {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	rp := &httputil.ReverseProxy{
		// SetXForwarded overwrites X-Forwarded-*: ReverseProxy drops the client's before Rewrite
		Rewrite: func(r *httputil.ProxyRequest) {
			backend := r.In.Context().Value(ctxKey{}).(string)
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = backend
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		Transport: headerTimeout{transport},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch mbe := (*http.MaxBytesError)(nil); {
			case errors.As(err, &mbe):
				http.Error(w, "vops: request body too large", http.StatusRequestEntityTooLarge)
			case errors.Is(err, errUpstreamTimeout):
				http.Error(w, "vops: upstream timed out", http.StatusGatewayTimeout)
			case bodyTimedOut(r):
				w.Header().Set("Connection", "close")
				http.Error(w, "vops: request body timed out", http.StatusRequestTimeout)
			default:
				http.Error(w, "vops: upstream unavailable", http.StatusBadGateway)
			}
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backends, project, limits := t.lookup(Host(r.Host))
		if len(backends) == 0 {
			http.Error(w, "vops: no such site", http.StatusNotFound)
			return
		}
		if !t.limiter.limit(w, r, project, limits) {
			return
		}
		timeout := DefaultTimeout
		if limits != nil && limits.Timeout > 0 {
			timeout = time.Duration(limits.Timeout) * time.Second
		}
		ctx := context.WithValue(context.WithValue(r.Context(), ctxKey{}, backends[0]), timeoutKey{}, timeout)
		rp.ServeHTTP(w, r.WithContext(ctx))
	})
}
