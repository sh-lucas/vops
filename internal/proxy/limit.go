package proxy

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Limits are a project's request limits, from compose's top-level x-vops. Zero values mean unlimited.
type Limits struct {
	Rate    float64 `json:"rate,omitempty"`     // requests per second per client ip
	Burst   int     `json:"burst,omitempty"`    // bucket size
	MaxBody int64   `json:"max_body,omitempty"` // request body bytes
}

func (l *Limits) set() bool { return l != nil && (l.Rate > 0 || l.MaxBody > 0) }

// String is the human form the plan shows ("unlimited", "rate 20/s burst 20, max_body 10MB").
func (l *Limits) String() string {
	if !l.set() {
		return "unlimited"
	}
	var s string
	if l.Rate > 0 {
		s = fmt.Sprintf("rate %s/s burst %d", strconv.FormatFloat(l.Rate, 'g', 6, 64), l.Burst)
	}
	if l.MaxBody > 0 {
		if s != "" {
			s += ", "
		}
		s += "max_body " + Bytes(l.MaxBody)
	}
	return s
}

// Bytes formats a size with the largest 1024-based unit that divides it (10485760 -> 10MB).
func Bytes(n int64) string {
	for _, u := range []struct {
		name string
		size int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if n >= u.size && n%u.size == 0 {
			return strconv.FormatInt(n/u.size, 10) + u.name
		}
	}
	return strconv.FormatInt(n, 10) + "B"
}

// Counters are what the limits refused, per project, since the proxy started.
type Counters struct {
	Limited  int64 `json:"limited"`   // 429
	TooLarge int64 `json:"too_large"` // 413
}

type bucket struct {
	tokens float64
	last   time.Time
}

type limitKey struct{ project, ip string }

// limiter is a token bucket per (project, client ip), in memory; idle buckets are purged.
type limiter struct {
	mu       sync.Mutex
	buckets  map[limitKey]*bucket
	counters sync.Map // project -> *[2]atomic.Int64 (429s, 413s)
	now      func() time.Time
	purged   time.Time
}

func newLimiter() *limiter { return &limiter{buckets: map[limitKey]*bucket{}, now: time.Now} }

// allow takes a token; when there is none it returns how long until there is one.
func (l *limiter) allow(project, ip string, lim *Limits) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.purged) > time.Minute {
		l.purge(now)
	}
	burst := float64(max(lim.Burst, 1))
	k := limitKey{project, ip}
	b := l.buckets[k]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		l.buckets[k] = b
	}
	b.tokens = math.Min(burst, b.tokens+now.Sub(b.last).Seconds()*lim.Rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / lim.Rate * float64(time.Second))
}

// purge drops buckets idle for 10 minutes: any sane rate has refilled them long before.
func (l *limiter) purge(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.buckets, k)
		}
	}
	l.purged = now
}

func (l *limiter) count(project string, i int) {
	c, _ := l.counters.LoadOrStore(project, new([2]atomic.Int64))
	c.(*[2]atomic.Int64)[i].Add(1)
}

func (l *limiter) snapshot() map[string]Counters {
	out := map[string]Counters{}
	l.counters.Range(func(k, v any) bool {
		c := v.(*[2]atomic.Int64)
		out[k.(string)] = Counters{c[0].Load(), c[1].Load()}
		return true
	})
	return out
}

// clientIP is the tcp peer: the proxy is the edge, X-Forwarded-For is whatever the client wrote.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// limit applies a project's limits to a request; false means it already answered (429 or 413).
func (l *limiter) limit(w http.ResponseWriter, r *http.Request, project string, lim *Limits) bool {
	if !lim.set() {
		return true
	}
	if lim.Rate > 0 {
		if ok, wait := l.allow(project, clientIP(r), lim); !ok {
			l.count(project, 0)
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
			http.Error(w, "vops: too many requests", http.StatusTooManyRequests)
			return false
		}
	}
	if lim.MaxBody > 0 {
		if r.ContentLength > lim.MaxBody {
			l.count(project, 1)
			w.Header().Set("Connection", "close")
			http.Error(w, "vops: request body too large", http.StatusRequestEntityTooLarge)
			return false
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &countedBody{http.MaxBytesReader(w, r.Body, lim.MaxBody), func() { l.count(project, 1) }}
		}
	}
	return true
}

// countedBody counts a chunked body going over max_body once.
type countedBody struct {
	rc   io.ReadCloser
	over func()
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	var mbe *http.MaxBytesError
	if err != nil && errors.As(err, &mbe) && b.over != nil {
		b.over()
		b.over = nil
	}
	return n, err
}

func (b *countedBody) Close() error { return b.rc.Close() }
