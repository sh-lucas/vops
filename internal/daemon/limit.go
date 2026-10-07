package daemon

import (
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// authLimiter slows password guessing on the web: a client (an ip, or an ipv6 /64) gets authMaxFails wrong
// credentials, dashboard logins and registry auth together, then is refused until authWindow passes without a
// new failure. An attempt is reserved before the (slow, pbkdf2) check, so parallel requests can't skip it.
type authLimiter struct {
	mu  sync.Mutex
	m   map[string]*authFails
	now func() time.Time // tests move it
	sem chan struct{}    // at most a few password checks at once: a flood can't take every cpu
}

type authFails struct {
	n     int // failures and checks in flight
	until time.Time
}

const (
	authMaxFails = 10
	authWindow   = 15 * time.Minute
)

var errTooManyFails = errors.New("too many failed logins from your address: try again in 15 minutes")

func newAuthLimiter() *authLimiter {
	return &authLimiter{m: map[string]*authFails{}, now: time.Now, sem: make(chan struct{}, 4)}
}

// check runs fn (a credential check) for the client of r, unless it is refused: errTooManyFails.
// blocked is true when this failure is the one that got the client refused (log it once).
// Loopback clients aren't limited: they are `vops ui` tunnels (ssh already authenticated them, and they all
// share 127.0.0.1) or the host itself; the proxy forwards everyone else with their real address.
func (l *authLimiter) check(r *http.Request, fn func() bool) (ok, blocked bool, err error) {
	if ip, ok := parseAddr(r.RemoteAddr); ok && ip.IsLoopback() {
		return l.run(fn), false, nil
	}
	key := clientKey(r.RemoteAddr)
	l.mu.Lock()
	now := l.now()
	f := l.m[key]
	if f != nil && now.After(f.until) {
		f = nil
	}
	if f == nil {
		if len(l.m) > 10000 {
			for k, x := range l.m {
				if now.After(x.until) {
					delete(l.m, k)
				}
			}
		}
		f = &authFails{}
		l.m[key] = f
	}
	if f.n >= authMaxFails {
		l.mu.Unlock()
		return false, false, errTooManyFails
	}
	f.n++
	f.until = now.Add(authWindow)
	l.mu.Unlock()

	if ok = l.run(fn); ok {
		l.mu.Lock()
		f.n-- // a success gives its attempt back, but doesn't forgive earlier failures
		l.mu.Unlock()
		return true, false, nil
	}
	l.mu.Lock()
	blocked = f.n == authMaxFails
	l.mu.Unlock()
	return false, blocked, nil
}

func (l *authLimiter) run(fn func() bool) bool {
	l.sem <- struct{}{}
	defer func() { <-l.sem }()
	return fn()
}

// clientKey is who a request counts against: its ip, or the /64 of an ipv6 address (one host usually has all of it).
// RemoteAddr is "ip:port", or a bare ip when the proxy forwarded it.
func clientKey(addr string) string {
	ip, ok := parseAddr(addr)
	if !ok {
		return addr
	}
	if ip.Is4() {
		return ip.String()
	}
	p, _ := ip.Prefix(64)
	return p.String()
}

// parseAddr reads "ip:port" or a bare ip.
func parseAddr(addr string) (netip.Addr, bool) {
	if ip, err := netip.ParseAddr(addr); err == nil {
		return ip.Unmap(), true
	}
	ap, err := netip.ParseAddrPort(addr)
	return ap.Addr().Unmap(), err == nil
}
