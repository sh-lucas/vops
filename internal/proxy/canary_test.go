package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tables without canaries hash and serialize as before canaries existed: a daemon and a proxy of different
// versions must agree on them, and routes.json from an older proxy still loads.
func TestHashWithoutCanaries(t *testing.T) {
	routes := []Route{
		{Project: "shop", Service: "web", Domains: []string{"web.shop.x", "shop.x"}, Backends: []string{"127.0.0.1:4000", "127.0.0.1:4001"}, Limits: &Limits{Rate: 20, Burst: 40}},
		{Project: "blog", Service: "app", Domains: []string{"blog.x"}, Backends: []string{"127.0.0.1:5000"}},
	}
	if got := Hash(routes); got != "0c43a4ec44b8be50" {
		t.Fatalf("hash %s", got)
	}
}

// A canary takes its weight of the clients, each one stays in its group (cookie), ?vops-canary=on|off forces a
// group and redirects without the param, a cookie of an earlier canary rolls again, 5xx are counted per group,
// a stable redeploy keeps the canary, and the table round-trips through Routes (what the proxy saves and hashes).
func TestCanaryRouting(t *testing.T) {
	answer := func(body string, code int) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			io.WriteString(w, body)
		}))
		t.Cleanup(s.Close)
		return s
	}
	stable, canary := answer("stable", 200), answer("canary", 503)
	addr := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	tb := NewTable()
	k := Key{"shop", "web"}
	tb.SetRoutes([]Route{{Project: "shop", Service: "web", Domains: []string{"shop.test"}, Backends: []string{addr(stable)}, Canary: &Canary{ID: 7, Weight: 30, Backends: []string{addr(canary)}}}})
	front := httptest.NewServer(Handler(tb))
	t.Cleanup(front.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path, cookie string) (string, *http.Response) {
		t.Helper()
		req, _ := http.NewRequest("GET", front.URL+path, nil)
		req.Host = "shop.test"
		if cookie != "" {
			req.Header.Set("Cookie", CanaryCookie+"="+cookie)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp
	}
	cookieOf := func(resp *http.Response) string {
		for _, c := range resp.Cookies() {
			if c.Name == CanaryCookie {
				if !c.HttpOnly || c.MaxAge != 86400 || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
					t.Fatalf("cookie attributes: %+v", c)
				}
				return c.Value
			}
		}
		return ""
	}
	defer func(f func() int) { canaryRoll = f }(canaryRoll)
	roll := 0
	canaryRoll = func() int { return roll }

	roll = 29 // under the weight: canary, and the cookie says so
	body, resp := get("/", "")
	if body != "canary" || cookieOf(resp) != "7.on" {
		t.Fatalf("roll 29: %q %q", body, cookieOf(resp))
	}
	roll = 30
	if body, resp := get("/", ""); body != "stable" || cookieOf(resp) != "7.off" {
		t.Fatalf("roll 30: %q", body)
	}
	roll = 0 // sticky: the cookie wins over the roll
	if body, resp := get("/", "7.off"); body != "stable" || cookieOf(resp) != "" {
		t.Fatalf("sticky off: %q %q", body, cookieOf(resp))
	}
	if _, resp := get("/x?a=1&"+CanaryParam+"=on", ""); resp.StatusCode != http.StatusFound || cookieOf(resp) != "7.on" {
		t.Fatal("the param must redirect")
	}
	_, resp = get("/x?a=1&"+CanaryParam+"=off", "7.on")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/x?a=1" || cookieOf(resp) != "7.off" {
		t.Fatalf("param: %d %q %q", resp.StatusCode, resp.Header.Get("Location"), cookieOf(resp))
	}
	for _, path := range []string{"//evil.test/", "///evil.test/"} {
		if _, resp := get(path+"?"+CanaryParam+"=on", ""); resp.Header.Get("Location") != "/evil.test/" {
			t.Fatalf("%s: redirect to %q", path, resp.Header.Get("Location"))
		}
	}
	if body, resp := get("/", "6.off"); body != "canary" || cookieOf(resp) != "7.on" {
		t.Fatalf("an earlier canary's cookie rolls again: %q", body)
	}
	st := tb.Canaries()["shop/web"]
	if st.ID != 7 || st.Weight != 30 || st.Canary != (Counts{2, 2}) || st.Stable != (Counts{2, 0}) {
		t.Fatalf("stats: %+v", st)
	}

	// a stable redeploy keeps the canary; the table round-trips
	tb.Set(k, []string{"shop.test"}, []string{addr(stable)})
	if body, _ := get("/", "7.on"); body != "canary" {
		t.Fatal("Set dropped the canary")
	}
	copy := NewTable()
	copy.SetRoutes(tb.Routes())
	if Hash(copy.Routes()) != Hash(tb.Routes()) {
		t.Fatal("routes don't round-trip")
	}
	// no canary: no cookie, the param is the app's, nothing counted
	tb.SetCanary(k, nil)
	if body, resp := get("/?"+CanaryParam+"=on", "7.on"); body != "stable" || cookieOf(resp) != "" || resp.StatusCode != 200 || len(tb.Canaries()) != 0 {
		t.Fatalf("without a canary: %q %d", body, resp.StatusCode)
	}
}

// With a canary, both groups still stream (SSE: the first event arrives before the response ends) and upgrade
// (101, then raw bytes both ways): statusWriter passes Flush and Hijack through, and a stream the client leaves
// is still counted.
func TestCanaryStreams(t *testing.T) {
	backend := func(name string) string {
		release := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Upgrade") == "echo" {
				w.Header().Set("Connection", "Upgrade")
				w.Header().Set("Upgrade", "echo")
				w.WriteHeader(http.StatusSwitchingProtocols)
				conn, brw, err := http.NewResponseController(w).Hijack()
				if err != nil {
					return
				}
				defer conn.Close()
				line, _ := brw.ReadString('\n')
				brw.WriteString(name + ":" + line)
				brw.Flush()
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: "+name+"\n\n")
			http.NewResponseController(w).Flush()
			select {
			case <-release:
			case <-r.Context().Done(): // the client left: the proxy closed its upstream request
			}
		}))
		t.Cleanup(func() { close(release); s.Close() })
		return strings.TrimPrefix(s.URL, "http://")
	}
	tb := NewTable()
	tb.SetRoutes([]Route{{Project: "shop", Service: "web", Domains: []string{"shop.test"}, Backends: []string{backend("stable")}, Canary: &Canary{ID: 1, Weight: 50, Backends: []string{backend("canary")}}}})
	front := httptest.NewServer(Handler(tb))
	t.Cleanup(front.Close)
	for _, group := range []string{"on", "off"} {
		want := map[string]string{"on": "canary", "off": "stable"}[group]
		// SSE
		req, _ := http.NewRequest("GET", front.URL, nil)
		req.Host = "shop.test"
		req.Header.Set("Cookie", CanaryCookie+"=1."+group)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		line := make(chan string, 1)
		go func() {
			b := make([]byte, 64)
			n, _ := resp.Body.Read(b)
			line <- string(b[:n])
		}()
		select {
		case got := <-line:
			if got != "data: "+want+"\n\n" {
				t.Fatalf("%s: SSE %q", group, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the first event never arrived (no flush)", group)
		}
		resp.Body.Close()

		// upgrade
		conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: shop.test\r\nConnection: Upgrade\r\nUpgrade: echo\r\nCookie: %s=1.%s\r\n\r\n", CanaryCookie, group)
		br := bufio.NewReader(conn)
		up, err := http.ReadResponse(br, nil)
		if err != nil || up.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("%s: upgrade: %v %+v", group, err, up)
		}
		io.WriteString(conn, "ping\n")
		if got, _ := br.ReadString('\n'); got != want+":ping\n" {
			t.Fatalf("%s: echo %q", group, got)
		}
		conn.Close()
	}
	// counted when each response ends (a stream or an upgraded connection closes)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		st := tb.Canaries()["shop/web"]
		if st.Canary.Requests == 2 && st.Stable.Requests == 2 && st.Canary.Errors+st.Stable.Errors == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats: %+v", st)
		}
	}
}
