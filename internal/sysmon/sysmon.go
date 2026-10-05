// Package sysmon reads the host's load from /proc and /sys (no cgo, works rootless): cpu, memory, disk io and
// usage, network, pressure (PSI), kernel and uptime. Rates come from the delta against the previous sample.
package sysmon

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Stats is what the System page and /api/system show. Rates and percentages are since the previous sample.
type Stats struct {
	Kernel       string         `json:"kernel"`
	Hostname     string         `json:"hostname"`
	Uptime       int64          `json:"uptime"`        // host, seconds
	DaemonUptime int64          `json:"daemon_uptime"` // vops daemon, seconds
	CPUs         int            `json:"cpus"`
	CPU          float64        `json:"cpu"`  // % busy, all cpus
	Load         [3]float64     `json:"load"` // 1, 5, 15 min
	MemTotal     uint64         `json:"mem_total"`
	MemUsed      uint64         `json:"mem_used"` // total - available
	SwapTotal    uint64         `json:"swap_total"`
	SwapUsed     uint64         `json:"swap_used"`
	Disks        []Disk         `json:"disks"`
	Filesystems  []Filesystem   `json:"filesystems"`
	Net          []Net          `json:"net"`
	Pressure     map[string]PSI `json:"pressure,omitempty"` // cpu, memory, io; absent without PSI
	Interval     float64        `json:"interval"`           // seconds between the two samples
	OOMKills     uint64         `json:"oom_kills"`          // processes killed by the kernel OOM killer since boot (/proc/vmstat)
}

type Disk struct {
	Name     string  `json:"name"`
	Busy     float64 `json:"busy"` // % of the time with io in flight (io_ticks)
	ReadBps  float64 `json:"read_bps"`
	WriteBps float64 `json:"write_bps"`
}

type Filesystem struct {
	Paths []string `json:"paths"` // what vops keeps there (~/.vops, the repo)
	Total uint64   `json:"total"`
	Used  uint64   `json:"used"`
	Avail uint64   `json:"avail"` // for unprivileged users
}

type Net struct {
	Name  string  `json:"name"`
	RxBps float64 `json:"rx_bps"` // bytes per second
	TxBps float64 `json:"tx_bps"`
	Speed int64   `json:"speed,omitempty"` // link speed in Mbit/s, when the kernel knows it
}

// PSI is one /proc/pressure file: share of time some (or all, full) tasks stalled, over 10s, 60s, 300s.
type PSI struct {
	Some [3]float64  `json:"some"`
	Full *[3]float64 `json:"full,omitempty"` // cpu has none on older kernels
}

// Sampler keeps the previous sample. Root is "/" outside tests.
type Sampler struct {
	Root    string
	Paths   []string // directories whose filesystems are shown
	started time.Time
	mu      sync.Mutex
	prev    *sample
	last    Stats
}

func New(paths ...string) *Sampler { return &Sampler{Root: "/", Paths: paths, started: time.Now()} }

type sample struct {
	at    time.Time
	cpu   cpuTimes
	disks map[string]diskCounters
	net   map[string]netCounters
}

// Read samples now and computes rates against the previous sample; the first call samples twice, 250ms apart.
// Calls less than a second apart (two open tabs) share a result.
func (s *Sampler) Read() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prev != nil && time.Since(s.prev.at) < time.Second {
		return s.last
	}
	cur := s.sample()
	if s.prev == nil {
		prev := cur
		time.Sleep(250 * time.Millisecond)
		s.prev, cur = &prev, s.sample()
	}
	prev := s.prev
	s.prev = &cur
	st := Stats{Interval: cur.at.Sub(prev.at).Seconds(), DaemonUptime: int64(time.Since(s.started).Seconds()), Pressure: map[string]PSI{}}
	st.Kernel = strings.TrimSpace(s.read("proc/sys/kernel/osrelease"))
	st.Hostname = strings.TrimSpace(s.read("proc/sys/kernel/hostname"))
	if f := strings.Fields(s.read("proc/uptime")); len(f) > 0 {
		v, _ := strconv.ParseFloat(f[0], 64)
		st.Uptime = int64(v)
	}
	st.Load = parseLoadavg(s.read("proc/loadavg"))
	st.CPUs = cur.cpu.n
	st.CPU = cpuPercent(prev.cpu, cur.cpu)
	mem := parseMeminfo(s.read("proc/meminfo"))
	st.MemTotal, st.MemUsed = mem["MemTotal"], mem["MemTotal"]-min(mem["MemAvailable"], mem["MemTotal"])
	st.SwapTotal, st.SwapUsed = mem["SwapTotal"], mem["SwapTotal"]-min(mem["SwapFree"], mem["SwapTotal"])
	dt := st.Interval
	if dt <= 0 {
		dt = 1
	}
	st.Disks = []Disk{}
	for _, name := range sortedKeys(cur.disks) {
		c, p := cur.disks[name], prev.disks[name]
		if c.ioTicks == 0 && c.sectorsRead == 0 && c.sectorsWritten == 0 {
			continue // never used
		}
		if _, ok := prev.disks[name]; !ok {
			p = c
		}
		st.Disks = append(st.Disks, Disk{Name: name, Busy: min(100, delta(c.ioTicks, p.ioTicks)/(dt*1000)*100),
			ReadBps: delta(c.sectorsRead, p.sectorsRead) * 512 / dt, WriteBps: delta(c.sectorsWritten, p.sectorsWritten) * 512 / dt})
	}
	st.Net = []Net{}
	for _, name := range sortedKeys(cur.net) {
		c, p := cur.net[name], prev.net[name]
		if _, ok := prev.net[name]; !ok {
			p = c
		}
		n := Net{Name: name, RxBps: delta(c.rx, p.rx) / dt, TxBps: delta(c.tx, p.tx) / dt}
		if v, err := strconv.ParseInt(strings.TrimSpace(s.read("sys/class/net/"+name+"/speed")), 10, 64); err == nil && v > 0 {
			n.Speed = v
		}
		st.Net = append(st.Net, n)
	}
	for _, r := range []string{"cpu", "memory", "io"} {
		if p, ok := parsePSI(s.read("proc/pressure/" + r)); ok {
			st.Pressure[r] = p
		}
	}
	st.Filesystems = s.filesystems()
	st.OOMKills = parseVmstat(s.read("proc/vmstat"))["oom_kill"]
	s.last = st
	return st
}

// delta is cur - prev, 0 when a counter went back (device re-added, reset).
func delta(cur, prev uint64) float64 {
	if cur < prev {
		return 0
	}
	return float64(cur - prev)
}

func (s *Sampler) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(s.Root, rel))
	return string(b)
}

func (s *Sampler) sample() sample {
	out := sample{at: time.Now(), cpu: parseStat(s.read("proc/stat")), disks: map[string]diskCounters{}, net: map[string]netCounters{}}
	for name, d := range parseDiskstats(s.read("proc/diskstats")) {
		// whole devices only (sda, nvme0n1, vda, dm-0), not partitions or loop/ram/zram
		if _, err := os.Stat(filepath.Join(s.Root, "sys/block", name)); err != nil || strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") {
			continue
		}
		out.disks[name] = d
	}
	all := parseNetDev(s.read("proc/net/dev"))
	for name, n := range all {
		// physical links (they have a device); virtual ones (bridges, veths) count the same bytes again
		if _, err := os.Stat(filepath.Join(s.Root, "sys/class/net", name, "device")); err == nil {
			out.net[name] = n
		}
	}
	if len(out.net) == 0 { // containers, some VMs: everything but loopback
		for name, n := range all {
			if name != "lo" {
				out.net[name] = n
			}
		}
	}
	return out
}

func (s *Sampler) filesystems() []Filesystem {
	out := []Filesystem{}
	seen := map[uint64]int{}
	for _, p := range s.Paths {
		var st syscall.Stat_t
		var fs syscall.Statfs_t
		if syscall.Stat(p, &st) != nil || syscall.Statfs(p, &fs) != nil {
			continue
		}
		if i, ok := seen[uint64(st.Dev)]; ok {
			out[i].Paths = append(out[i].Paths, p)
			continue
		}
		bs := uint64(fs.Bsize)
		seen[uint64(st.Dev)] = len(out)
		out = append(out, Filesystem{Paths: []string{p}, Total: fs.Blocks * bs, Used: (fs.Blocks - fs.Bfree) * bs, Avail: fs.Bavail * bs})
	}
	return out
}

// ---- parsers (pure, tested with fixtures)

type cpuTimes struct {
	busy, total uint64
	n           int // cpus
}

// parseStat reads the aggregate "cpu" line of /proc/stat; idle = idle + iowait.
func parseStat(text string) cpuTimes {
	var c cpuTimes
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			c.n++
			continue
		}
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			if i >= 8 { // guest and guest_nice are already in user and nice
				break
			}
			c.total += n
			if i != 3 && i != 4 {
				c.busy += n
			}
		}
	}
	return c
}

func cpuPercent(prev, cur cpuTimes) float64 {
	if cur.total <= prev.total {
		return 0
	}
	return min(100, delta(cur.busy, prev.busy)/float64(cur.total-prev.total)*100)
}

// parseMeminfo returns /proc/meminfo in bytes.
func parseMeminfo(text string) map[string]uint64 {
	out := map[string]uint64{}
	for line := range strings.SplitSeq(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		f := strings.Fields(v)
		if !ok || len(f) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(f[0], 10, 64)
		if len(f) > 1 && f[1] == "kB" {
			n *= 1024
		}
		out[k] = n
	}
	return out
}

func parseLoadavg(text string) [3]float64 {
	var l [3]float64
	f := strings.Fields(text)
	for i := 0; i < 3 && i < len(f); i++ {
		l[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return l
}

func parseVmstat(text string) map[string]uint64 {
	out := map[string]uint64{}
	for line := range strings.SplitSeq(text, "\n") {
		if k, v, ok := strings.Cut(line, " "); ok {
			out[k], _ = strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return out
}

type diskCounters struct{ sectorsRead, sectorsWritten, ioTicks uint64 }

// parseDiskstats: major minor name reads merged sectors_read ms writes merged sectors_written ms in_flight io_ticks ...
func parseDiskstats(text string) map[string]diskCounters {
	out := map[string]diskCounters{}
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 13 {
			continue
		}
		u := func(i int) uint64 { n, _ := strconv.ParseUint(f[i], 10, 64); return n }
		out[f[2]] = diskCounters{sectorsRead: u(5), sectorsWritten: u(9), ioTicks: u(12)}
	}
	return out
}

type netCounters struct{ rx, tx uint64 }

func parseNetDev(text string) map[string]netCounters {
	out := map[string]netCounters{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		f := strings.Fields(rest)
		if !ok || len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		out[strings.TrimSpace(name)] = netCounters{rx, tx}
	}
	return out
}

// parsePSI reads "some avg10=0.00 avg60=0.00 avg300=0.00 total=0" (and "full ...").
func parsePSI(text string) (PSI, bool) {
	var p PSI
	found := false
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		var v [3]float64
		for i, kv := range f[1:4] {
			_, num, _ := strings.Cut(kv, "=")
			v[i], _ = strconv.ParseFloat(num, 64)
		}
		switch f[0] {
		case "some":
			p.Some, found = v, true
		case "full":
			p.Full = &v
		}
	}
	return p, found
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
