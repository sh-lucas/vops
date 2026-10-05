package sysmon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParsers(t *testing.T) {
	stat := `cpu  100 0 50 800 50 0 0 0 10 0
cpu0 50 0 25 400 25 0 0 0 5 0
cpu1 50 0 25 400 25 0 0 0 5 0
intr 1 2 3
`
	c := parseStat(stat)
	if c.n != 2 || c.total != 1000 || c.busy != 150 {
		t.Fatalf("stat: %+v", c)
	}
	if p := cpuPercent(cpuTimes{busy: 100, total: 900}, c); p != 50 {
		t.Fatalf("cpu %%: %v", p)
	}
	m := parseMeminfo("MemTotal:       16000 kB\nMemAvailable:    4000 kB\nHugePages_Total:       0\n")
	if m["MemTotal"] != 16000*1024 || m["MemAvailable"] != 4000*1024 || m["HugePages_Total"] != 0 {
		t.Fatalf("meminfo: %v", m)
	}
	if l := parseLoadavg("0.50 1.25 2.00 1/300 4242\n"); l != [3]float64{0.5, 1.25, 2} {
		t.Fatalf("loadavg: %v", l)
	}
	ds := parseDiskstats(`   8       0 sda 1000 10 20000 500 2000 20 40000 900 0 1500 1400 0 0 0 0 0 0
   8       1 sda1 900 10 18000 450 1900 20 38000 850 0 1400 1300
   7       0 loop0 0 0 0 0 0 0 0 0 0 0 0
`)
	if d := ds["sda"]; d.sectorsRead != 20000 || d.sectorsWritten != 40000 || d.ioTicks != 1500 || len(ds) != 3 {
		t.Fatalf("diskstats: %+v", ds)
	}
	nd := parseNetDev(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:    5000      10    0    0    0     0          0         0     5000      10    0    0    0     0       0          0
  eth0: 1000000    800    0    0    0     0          0         0   250000     600    0    0    0     0       0          0
`)
	if nd["eth0"] != (netCounters{1000000, 250000}) || nd["lo"].rx != 5000 || len(nd) != 2 {
		t.Fatalf("net/dev: %+v", nd)
	}
	p, ok := parsePSI("some avg10=1.50 avg60=0.75 avg300=0.10 total=12345\nfull avg10=0.50 avg60=0.25 avg300=0.00 total=678\n")
	if !ok || p.Some != [3]float64{1.5, 0.75, 0.1} || p.Full == nil || *p.Full != [3]float64{0.5, 0.25, 0} {
		t.Fatalf("psi: %+v", p)
	}
	if p, ok := parsePSI("some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"); !ok || p.Full != nil {
		t.Fatalf("psi without full: %+v", p)
	}
	if _, ok := parsePSI(""); ok {
		t.Fatal("missing psi file parsed")
	}
}

// A fake /proc and /sys: rates from two samples, whole disks and physical links only, no PSI = no pressure.
func TestRead(t *testing.T) {
	root := t.TempDir()
	write := func(files map[string]string) {
		for name, text := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755)
			os.WriteFile(filepath.Join(root, name), []byte(text), 0o644)
		}
	}
	for _, d := range []string{"sys/block/sda", "sys/class/net/eth0/device", "sys/class/net/br0"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	write(map[string]string{
		"proc/sys/kernel/osrelease": "6.8.0-test\n", "proc/sys/kernel/hostname": "box\n", "proc/uptime": "3600.5 100.0\n",
		"proc/loadavg": "0.1 0.2 0.3 1/1 1\n", "proc/meminfo": "MemTotal: 1000 kB\nMemAvailable: 250 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"proc/stat":                "cpu  100 0 0 900 0 0 0 0 0 0\ncpu0 100 0 0 900 0 0 0 0 0 0\n",
		"proc/diskstats":           "8 0 sda 0 0 0 0 0 0 0 0 0 0 0\n8 1 sda1 0 0 0 0 0 0 0 0 0 0 0\n",
		"proc/net/dev":             "h\nh\n  eth0: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n   br0: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n",
		"sys/class/net/eth0/speed": "1000\n", "proc/vmstat": "pgfault 10\noom_kill 3\n",
	})
	s := &Sampler{Root: root, Paths: []string{root}, started: time.Now()}
	s.Read()
	write(map[string]string{
		"proc/stat":      "cpu  200 0 0 1000 0 0 0 0 0 0\ncpu0 200 0 0 1000 0 0 0 0 0 0\n",
		"proc/diskstats": "8 0 sda 0 0 100 0 0 0 200 0 0 500 0\n8 1 sda1 0 0 100 0 0 0 200 0 0 500 0\n",
		"proc/net/dev":   "h\nh\n  eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0\n   br0: 9 0 0 0 0 0 0 0 9 0 0 0 0 0 0 0\n",
	})
	s.prev.at = time.Now().Add(-2 * time.Second) // the last sample was 2s ago
	st := s.Read()
	if st.Kernel != "6.8.0-test" || st.Hostname != "box" || st.Uptime != 3600 || st.CPUs != 1 || st.MemTotal != 1000*1024 || st.MemUsed != 750*1024 {
		t.Fatalf("stats: %+v", st)
	}
	if st.CPU != 50 || st.OOMKills != 3 {
		t.Fatalf("cpu: %v oom kills %d", st.CPU, st.OOMKills)
	}
	if len(st.Disks) != 1 || st.Disks[0].Name != "sda" || st.Disks[0].Busy < 24 || st.Disks[0].Busy > 26 || st.Disks[0].ReadBps < 25000 || st.Disks[0].ReadBps > 26000 {
		t.Fatalf("disks: %+v", st.Disks)
	}
	if len(st.Net) != 1 || st.Net[0].Name != "eth0" || st.Net[0].Speed != 1000 || st.Net[0].RxBps < 490 || st.Net[0].RxBps > 510 {
		t.Fatalf("net: %+v", st.Net)
	}
	if len(st.Pressure) != 0 || len(st.Filesystems) != 1 || st.Filesystems[0].Total == 0 {
		t.Fatalf("pressure %v filesystems %+v", st.Pressure, st.Filesystems)
	}
	// the real host: whatever it has, it reads without panicking
	if live := New(t.TempDir()).Read(); live.MemTotal == 0 || live.Kernel == "" {
		t.Fatalf("live: %+v", live)
	}
}
