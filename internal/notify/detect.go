package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/sysmon"
)

// Detectors' logic, pure: thresholds held for a while, consecutive failures, what a container's death means.

// sustained remembers since when each condition holds.
type sustained map[string]time.Time

// check reports whether cond has held continuously for at least d (d = 0: as soon as it holds).
func (s sustained) check(key string, cond bool, now time.Time, d time.Duration) bool {
	if !cond {
		delete(s, key)
		return false
	}
	since, ok := s[key]
	if !ok {
		s[key], since = now, now
	}
	return now.Sub(since) >= d
}

// check is one threshold evaluated on a sample.
type check struct {
	key, title, body string
	cond             bool
	d                time.Duration
}

// hostChecks are the host-wide resource checks on a sysmon sample.
func hostChecks(st sysmon.Stats, s Settings) []check {
	var out []check
	out = append(out, check{"resources:cpu", "host cpu at " + pct(st.CPU), fmt.Sprintf("cpu above %d%% for %s (load %.2f %.2f %.2f, %d cpus)", s.CPU, span(sec(s.CPUFor)), st.Load[0], st.Load[1], st.Load[2], st.CPUs),
		st.CPU > float64(s.CPU), sec(s.CPUFor)})
	if st.MemTotal > 0 {
		avail := float64(st.MemTotal-st.MemUsed) / float64(st.MemTotal) * 100
		out = append(out, check{"resources:mem", "host memory: " + pct(avail) + " available", fmt.Sprintf("less than %d%% of %s available for %s; swap %s of %s used", s.MemAvail, size(st.MemTotal), span(sec(s.MemFor)), size(st.SwapUsed), size(st.SwapTotal)),
			avail < float64(s.MemAvail), sec(s.MemFor)})
	}
	for _, f := range st.Filesystems {
		if f.Used+f.Avail == 0 {
			continue
		}
		used := float64(f.Used) / float64(f.Used+f.Avail) * 100
		out = append(out, check{"resources:disk:" + f.Paths[0], "disk " + pct(used) + " full", fmt.Sprintf("%s: %s available of %s (holds %s)", f.Paths[0], size(f.Avail), size(f.Total), strings.Join(f.Paths, ", ")),
			used > float64(s.Disk), 0})
	}
	if p, ok := st.Pressure["memory"]; ok {
		out = append(out, check{"resources:psi", fmt.Sprintf("memory pressure %.0f%%", p.Some[1]),
			fmt.Sprintf("tasks waited on memory %.1f%% of the last minute (PSI some avg60). This comes before the kernel OOM killer, systemd-oomd or earlyoom kill something (they may pick the proxy or the daemon): free memory or add swap.", p.Some[1]),
			p.Some[1] > float64(s.PSI), 0})
	}
	return out
}

// cgroup is what vops reads of a container's cgroup v2 (no podman stats: a few small files per container).
type cgroup struct {
	mem, memMax uint64 // memMax 0 = no limit
	cpuUsec     uint64
	cores       float64 // cpu quota in cores, 0 = none
	oomKills    uint64
	at          time.Time
}

func readCgroup(dir string, now time.Time) (cgroup, bool) {
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(b))
	}
	stat := read("cpu.stat")
	if stat == "" {
		return cgroup{}, false
	}
	c := cgroup{at: now}
	c.mem, _ = strconv.ParseUint(read("memory.current"), 10, 64)
	c.memMax, _ = strconv.ParseUint(read("memory.max"), 10, 64) // "max" parses to 0
	c.cpuUsec = field(stat, "usage_usec")
	c.oomKills = field(read("memory.events"), "oom_kill")
	if q, p, ok := strings.Cut(read("cpu.max"), " "); ok && q != "max" {
		qn, _ := strconv.ParseFloat(q, 64)
		pn, _ := strconv.ParseFloat(p, 64)
		if pn > 0 {
			c.cores = qn / pn
		}
	}
	return c, true
}

func field(text, key string) uint64 {
	for line := range strings.SplitSeq(text, "\n") {
		if k, v, ok := strings.Cut(line, " "); ok && k == key {
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// ctrUsage is a container's memory % of its limit (-1: no limit) and cpu % of its quota or of the host, since prev.
func ctrUsage(prev, cur cgroup, hostCPUs int) (mem, cpu float64) {
	mem = -1
	if cur.memMax > 0 {
		mem = float64(cur.mem) / float64(cur.memMax) * 100
	}
	cores := cur.cores
	if cores == 0 {
		cores = float64(max(hostCPUs, 1))
	}
	if dt := cur.at.Sub(prev.at).Seconds(); dt > 0 && cur.cpuUsec >= prev.cpuUsec && !prev.at.IsZero() {
		cpu = float64(cur.cpuUsec-prev.cpuUsec) / (dt * 1e6 * cores) * 100
	}
	return mem, cpu
}

// Death is a container that died, as podman (events and inspect) and the engine describe it.
type Death struct {
	Project, Service string
	Name             string
	ExitCode         int
	OOMKilled        bool
	Job              bool // a compose job (service_completed_successfully): it is meant to exit
	Expected         bool // the engine stopped, removed or restarted it
	Gone             bool // removed right after: only vops (or someone by hand) removes containers
	Stopping         bool // the daemon is shutting down (the host too, probably)
}

// classify says which alerts a death raises: none for anything vops (or the host shutting down) caused, for jobs
// (they exit; their failures are deploy failures) and previews; else a restart, and an oom when it was killed.
func classify(d Death) []string {
	if d.Expected || d.Gone || d.Stopping || d.Job || d.Project == "" || strings.Contains(d.Project, "@") {
		return nil
	}
	if d.OOMKilled || d.ExitCode == 137 {
		return []string{"restart", "oom"}
	}
	return []string{"restart"}
}

func (d Death) cause() string {
	switch {
	case d.OOMKilled:
		return "OOM-killed (its memory limit)"
	case d.ExitCode == 137:
		return "killed by SIGKILL (exit 137): likely the kernel OOM killer, systemd-oomd or earlyoom, or someone ran podman kill"
	case d.ExitCode > 128:
		return fmt.Sprintf("killed by signal %d (exit %d)", d.ExitCode-128, d.ExitCode)
	}
	return fmt.Sprintf("exited with code %d", d.ExitCode)
}

func pct(x float64) string { return fmt.Sprintf("%.0f%%", x) }

func size(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}
