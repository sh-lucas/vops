package notify

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/store"
	"github.com/sh-lucas/vops/internal/sysmon"
)

// An event alert: sent at once, coalesced, re-sent only on the repeat schedule, expired, then over after a quiet period.
func TestEventAlertLifecycle(t *testing.T) {
	s := Defaults
	t0 := time.Unix(1_000_000, 0)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	o := Obs{Kind: "restart", Key: "restart:shop/web", Project: "shop", Service: "web", Title: "shop/web restarted", Body: "exit 1"}

	a := Seen(nil, o, t0)
	a, send, _ := Step(a, t0, s)
	if !send || a.Sends != 1 || a.State != "open" || a.Occurrences != 1 {
		t.Fatalf("new alert: send=%v %+v", send, a)
	}
	for i := range 4 { // 4 more restarts within 12 minutes: no new push
		a = Seen(&a, o, at(time.Duration(i+1)*3*time.Minute))
		if a, send, _ = Step(a, at(time.Duration(i+1)*3*time.Minute), s); send {
			t.Fatalf("occurrence %d was sent", i+2)
		}
	}
	if a.Occurrences != 5 || a.Title != "shop/web restarted (5 times in 12m)" {
		t.Fatalf("coalesced: %d %q", a.Occurrences, a.Title)
	}
	// still happening an hour later: the repeat is due
	a = Seen(&a, o, at(59*time.Minute))
	if a, send, _ = Step(a, at(61*time.Minute), s); !send || a.Sends != 2 {
		t.Fatalf("repeat: %v %+v", send, a)
	}
	// keeps happening for a day: expired, no more sends, still live (no new alert for the key)
	for m := 90; m <= 24*60+30; m += 20 {
		a = Seen(&a, o, at(time.Duration(m)*time.Minute))
		a, _, _ = Step(a, at(time.Duration(m)*time.Minute), s)
	}
	if a.State != "expired" || a.EndedAt != 0 || a.Sends > 25 {
		t.Fatalf("expire: %+v", a)
	}
	if _, send, _ := Step(a, at(24*time.Hour+40*time.Minute), s); send {
		t.Fatal("an expired alert was sent")
	}
	// quiet for 30 minutes: over. Expired, so no "resolved" either.
	a, send, resolved := Step(a, at(24*time.Hour+31*time.Minute+30*time.Minute), s)
	if send || resolved || a.EndedAt == 0 || a.State != "expired" {
		t.Fatalf("quiet: %v %v %+v", send, resolved, a)
	}
}

// A condition alert resolves when it is no longer seen; a silenced one stops sending but stays live until then.
func TestConditionAlertAndSilence(t *testing.T) {
	s := Defaults
	s.ProbeInterval = 10
	t0 := time.Unix(2_000_000, 0)
	o := Obs{Kind: "down", Key: "down:shop/web", Project: "shop", Title: "shop/web is down"}
	a := Seen(nil, o, t0)
	a, send, _ := Step(a, t0, s)
	if !send {
		t.Fatal("not sent")
	}
	a = Seen(&a, o, t0.Add(10*time.Second))
	if a.Occurrences != 1 || a.Title != "shop/web is down" {
		t.Fatalf("a condition counts no occurrences: %+v", a)
	}
	// not seen for 3 probe intervals: resolved, with the resolved notification (it was sent)
	b, send, resolved := Step(a, t0.Add(10*time.Second+36*time.Second), s)
	if send || !resolved || b.State != "resolved" || b.EndedAt == 0 {
		t.Fatalf("resolve: %v %v %+v", send, resolved, b)
	}
	// silenced: no sends even when the repeat is due, no resolved notification at the end, who is recorded
	c, err := Silence(a, "ops", t0.Add(20*time.Second))
	if err != nil || c.ClosedBy != "ops" || c.State != "silenced" {
		t.Fatalf("silence: %v %+v", err, c)
	}
	if _, err := Silence(c, "ops", t0); err == nil {
		t.Fatal("silenced twice")
	}
	c = Seen(&c, o, t0.Add(2*time.Hour))
	if c, send, _ = Step(c, t0.Add(2*time.Hour), s); send || c.State != "silenced" {
		t.Fatalf("silenced alert sent: %+v", c)
	}
	if c, _, resolved = Step(c, t0.Add(3*time.Hour), s); resolved || c.EndedAt == 0 || c.State != "silenced" {
		t.Fatalf("silenced end: %v %+v", resolved, c)
	}
	// End (a deploy succeeded) closes an open alert at once
	if e, resolved := End(a, t0.Add(time.Minute)); !resolved || e.State != "resolved" {
		t.Fatalf("end: %+v", e)
	}
}

func TestCanSee(t *testing.T) {
	repos := map[string][]string{"shop": {"shop/web", "shop/api"}, "blog": {"blog/site"}}
	admin := store.User{Name: "admin", Role: store.Admin, Global: true}
	ci := store.User{Name: "ci", Role: store.Deployer, Repos: []string{"shop/api"}}
	for _, c := range []struct {
		u       store.User
		project string
		ok      bool
	}{
		{admin, "shop", true}, {admin, "", true}, {admin, "unknown", true},
		{ci, "shop", true}, {ci, "shop@pr-1", true}, {ci, "blog", false}, {ci, "", false}, {ci, "unknown", false},
	} {
		if got := CanSee(c.u, c.project, repos); got != c.ok {
			t.Errorf("%s sees %q: %v", c.u.Name, c.project, got)
		}
	}
}

func TestSettings(t *testing.T) {
	s := SettingsFrom(map[string]int64{"repeat": 600, "cpu": 500, "nope": 1})
	if s.Repeat != 600 || s.CPU != Defaults.CPU {
		t.Fatalf("from db: %+v", s)
	}
	if o, err := s.Overrides(); err != nil || len(o) != 1 || o["repeat"] != 600 {
		t.Fatalf("overrides: %v %v", o, err)
	}
	s.DownAfter = 0
	if _, err := s.Overrides(); err == nil || !strings.Contains(err.Error(), "down_after") {
		t.Fatalf("validation: %v", err)
	}
	if Wants(nil, "x", "resolved") || Wants(nil, "x", "deploy_ok") || !Wants(nil, "x", "down") || Wants(map[string]map[string]bool{"x": {"down": false}}, "x", "down") {
		t.Fatal("kind defaults")
	}
}

func TestSustainedAndHostChecks(t *testing.T) {
	s := Defaults
	t0 := time.Unix(0, 0)
	h := sustained{}
	busy := sysmon.Stats{CPU: 95, CPUs: 2, MemTotal: 1000, MemUsed: 950, Filesystems: []sysmon.Filesystem{{Paths: []string{"/home"}, Total: 100, Used: 95, Avail: 5}},
		Pressure: map[string]sysmon.PSI{"memory": {Some: [3]float64{30, 25, 10}}}}
	fired := func(st sysmon.Stats, at time.Duration) []string {
		var out []string
		for _, c := range hostChecks(st, s) {
			if h.check(c.key, c.cond, t0.Add(at), c.d) {
				out = append(out, c.key)
			}
		}
		return out
	}
	if got := fired(busy, 0); !slices.Equal(got, []string{"resources:disk:/home", "resources:psi"}) {
		t.Fatalf("at once: %v", got)
	}
	if got := fired(busy, 5*time.Minute); !slices.Contains(got, "resources:mem") || slices.Contains(got, "resources:cpu") {
		t.Fatalf("5m: %v", got)
	}
	calm := busy
	calm.CPU = 10
	fired(calm, 6*time.Minute) // a dip resets the cpu window
	if got := fired(busy, 11*time.Minute); slices.Contains(got, "resources:cpu") {
		t.Fatalf("cpu fired after a dip: %v", got)
	}
	if got := fired(busy, 21*time.Minute); !slices.Contains(got, "resources:cpu") {
		t.Fatalf("cpu 10m: %v", got)
	}
	for _, c := range hostChecks(busy, s) {
		if c.key == "resources:psi" && !strings.Contains(c.body, "OOM killer") {
			t.Fatalf("psi body: %s", c.body)
		}
	}
}

func TestCgroup(t *testing.T) {
	dir := t.TempDir()
	write := func(files map[string]string) {
		for k, v := range files {
			os.WriteFile(filepath.Join(dir, k), []byte(v), 0o644)
		}
	}
	write(map[string]string{"cpu.stat": "usage_usec 1000000\nuser_usec 1\n", "memory.current": "950\n", "memory.max": "1000\n", "cpu.max": "50000 100000\n", "memory.events": "low 0\noom 1\noom_kill 1\n"})
	t0 := time.Unix(100, 0)
	a, ok := readCgroup(dir, t0)
	if !ok || a.memMax != 1000 || a.cores != 0.5 || a.oomKills != 1 {
		t.Fatalf("read: %+v", a)
	}
	write(map[string]string{"cpu.stat": "usage_usec 5500000\n"})
	b, _ := readCgroup(dir, t0.Add(10*time.Second))
	if mem, cpu := ctrUsage(a, b, 4); mem != 95 || cpu < 89.9 || cpu > 90.1 {
		t.Fatalf("usage: %v %v", mem, cpu)
	}
	write(map[string]string{"memory.max": "max\n", "cpu.max": "max 100000\n"})
	c, _ := readCgroup(dir, t0.Add(20*time.Second))
	if mem, cpu := ctrUsage(b, c, 4); mem != -1 || cpu != 0 || c.cores != 0 {
		t.Fatalf("no limits: %v %v %+v", mem, cpu, c)
	}
	if _, ok := readCgroup(filepath.Join(dir, "gone"), t0); ok {
		t.Fatal("a missing cgroup read fine")
	}
}

// What a death means: vops' own stops, jobs, previews and shutdowns are never alerts.
func TestClassify(t *testing.T) {
	base := Death{Project: "shop", Service: "web", Name: "vops-shop-web-1", ExitCode: 1}
	for _, c := range []struct {
		name string
		mod  func(*Death)
		want []string
	}{
		{"crash", func(*Death) {}, []string{"restart"}},
		{"sigkill", func(d *Death) { d.ExitCode = 137 }, []string{"restart", "oom"}},
		{"oom-killed", func(d *Death) { d.OOMKilled = true }, []string{"restart", "oom"}},
		{"stopped by the engine", func(d *Death) { d.Expected = true }, nil},
		{"removed right after (rolling release, rm)", func(d *Death) { d.Gone = true }, nil},
		{"job done", func(d *Death) { d.Job, d.ExitCode = true, 0 }, nil},
		{"daemon stopping", func(d *Death) { d.Stopping = true }, nil},
		{"preview", func(d *Death) { d.Project = "shop@pr-1" }, nil},
	} {
		d := base
		c.mod(&d)
		if got := classify(d); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	if d := (Death{ExitCode: 137}); !strings.Contains(d.cause(), "systemd-oomd") {
		t.Fatal(d.cause())
	}
}
