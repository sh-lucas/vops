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
func (l *authLimiter) check(r *http.Request, fn func() bool) (ok, blocked bool, err error) {
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

	l.sem <- struct{}{}
	ok = fn()
	<-l.sem

	if ok {
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

// clientKey is who a request counts against: its ip, or the /64 of an ipv6 address (one host usually has all of it).
// RemoteAddr is "ip:port", or a bare ip when the proxy forwarded it.
func clientKey(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		ap, err := netip.ParseAddrPort(addr)
		if err != nil {
			return addr
		}
		ip = ap.Addr()
	}
	ip = ip.Unmap()
	if ip.Is4() {
		return ip.String()
	}
	p, _ := ip.Prefix(64)
	return p.String()
}
