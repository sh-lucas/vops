package proxy

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
)

// CanaryCookie keeps a client in its group (stable or canary) for a day; its value names the canary (release id),
// so a cookie from an earlier canary is rolled again.
const CanaryCookie = "vops_canary"

// CanaryParam on a routed host with a canary (?vops-canary=on|off) sets the cookie and redirects without it:
// the dashboard's "Open the canary" link. Anyone can use it; it only picks between versions the service serves.
const CanaryParam = "vops-canary"

// canaryRoll is a number in [0, 100) (tests replace it).
var canaryRoll = func() int { return rand.IntN(100) }

// canaryGroup picks the group of a request: the cookie's, else by weight (and sets the cookie). done: it answered
// (a redirect after ?vops-canary=).
func canaryGroup(w http.ResponseWriter, r *http.Request, c *Canary) (on, done bool) {
	set := func(on bool) {
		v := fmt.Sprintf("%d.%s", c.ID, map[bool]string{true: "on", false: "off"}[on])
		http.SetCookie(w, &http.Cookie{Name: CanaryCookie, Value: v, Path: "/", MaxAge: 86400, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
	}
	if q := r.URL.Query(); q.Has(CanaryParam) {
		v := q.Get(CanaryParam)
		if v == "on" || v == "off" {
			set(v == "on")
			q.Del(CanaryParam)
			loc := "/" + strings.TrimLeft(r.URL.EscapedPath(), "/\\") // never "//other.host"
			if len(q) > 0 {
				loc += "?" + q.Encode()
			}
			http.Redirect(w, r, loc, http.StatusFound)
			return false, true
		}
	}
	if ck, err := r.Cookie(CanaryCookie); err == nil {
		if id, group, ok := strings.Cut(ck.Value, "."); ok && id == fmt.Sprint(c.ID) && (group == "on" || group == "off") {
			return group == "on", false
		}
	}
	on = canaryRoll() < c.Weight
	set(on)
	return on, false
}

// statusWriter remembers the status a response got (502s of the ErrorHandler included).
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Counts are the requests and 5xx answers of one group.
type Counts struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"` // 5xx
}

// CanaryStats compare a canary with its stable group, since the canary (its release) started or the proxy did.
type CanaryStats struct {
	ID     int64  `json:"id"`
	Weight int    `json:"weight"`
	Stable Counts `json:"stable"`
	Canary Counts `json:"canary"`
}

type groupStats struct {
	id    int64
	count [2][2]atomic.Int64 // [stable, canary][requests, 5xx]
}

func (t *Table) count(k Key, c *Canary, on bool, code int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if current := t.services[k].canary; current == nil || current.ID != c.ID {
		return // a slow response from a release that is no longer routed
	}
	v, _ := t.groups.Load(k)
	g, _ := v.(*groupStats)
	if g == nil || g.id != c.ID {
		g = &groupStats{id: c.ID}
		t.groups.Store(k, g)
	}
	i := 0
	if on {
		i = 1
	}
	g.count[i][0].Add(1)
	if code >= 500 {
		g.count[i][1].Add(1)
	}
}

// Canaries are the counts of the canaries routed now, by "project/service".
func (t *Table) Canaries() map[string]CanaryStats {
	out := map[string]CanaryStats{}
	for _, r := range t.Routes() {
		if r.Canary == nil {
			continue
		}
		st := CanaryStats{ID: r.Canary.ID, Weight: r.Canary.Weight}
		if v, ok := t.groups.Load(Key{r.Project, r.Service}); ok {
			if g := v.(*groupStats); g.id == r.Canary.ID {
				st.Stable = Counts{g.count[0][0].Load(), g.count[0][1].Load()}
				st.Canary = Counts{g.count[1][0].Load(), g.count[1][1].Load()}
			}
		}
		out[r.Project+"/"+r.Service] = st
	}
	return out
}
