// Package proxy is the routing table and reverse proxy in front of every routed container.
package proxy

import (
	"context"
	"net"
	"net/http"
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
}

func NewTable() *Table { return &Table{services: map[Key]entry{}, hosts: map[string]Key{}} }

// Set replaces the domains and backends ("127.0.0.1:port") of a service. No backends removes it.
func (t *Table) Set(k Key, domains, backends []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(backends) == 0 || len(domains) == 0 {
		delete(t.services, k)
	} else {
		t.services[k] = entry{slices.Clone(domains), slices.Clone(backends), new(atomic.Uint64)}
	}
	t.reindex()
}

// Replace swaps the whole table (used when rebuilding from podman).
func (t *Table) Replace(all map[Key][2][]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.services = map[Key]entry{}
	for k, v := range all {
		if len(v[0]) > 0 && len(v[1]) > 0 {
			t.services[k] = entry{v[0], v[1], new(atomic.Uint64)}
		}
	}
	t.reindex()
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
	t.mu.RLock()
	defer t.mu.RUnlock()
	k, ok := t.hosts[host]
	if !ok {
		return nil
	}
	e := t.services[k]
	n := len(e.backends)
	start := int(e.next.Add(1) % uint64(n))
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, e.backends[(start+i)%n])
	}
	return out
}

type Route struct {
	Project  string   `json:"project"`
	Service  string   `json:"service"`
	Domains  []string `json:"domains"`
	Backends []string `json:"backends"`
}

func (t *Table) Routes() []Route {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := []Route{}
	for k, e := range t.services {
		out = append(out, Route{k.Project, k.Service, e.domains, e.backends})
	}
	slices.SortFunc(out, func(a, b Route) int { return strings.Compare(a.Project+a.Service, b.Project+b.Service) })
	return out
}

// Host normalizes a Host header: lowercase, no port.
func Host(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

type ctxKey struct{}

// Handler proxies to the table, round-robin over replicas.
func Handler(t *Table) http.Handler {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0,
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			backend := r.In.Context().Value(ctxKey{}).(string)
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = backend
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "vops: upstream unavailable", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backends := t.Backends(Host(r.Host))
		if len(backends) == 0 {
			http.Error(w, "vops: no such site", http.StatusNotFound)
			return
		}
		rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, backends[0])))
	})
}
