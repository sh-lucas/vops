// Package notify decides what is worth telling an admin (alerts: deploy failures, restarts, oom kills, services down
// or unhealthy, resources running out), keeps each alert's state (repeat, expire, silence, resolve) and delivers it
// with Web Push to every device of the users who can see the project.
package notify

import (
	"fmt"
	"slices"
	"time"
)

// Kind is a kind of notification; users toggle each one.
type Kind struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Desc    string `json:"desc"`
	Default bool   `json:"default"`
	Event   bool   `json:"-"` // coalesced occurrences that resolve after a quiet period (else: a condition that clears)
}

var Kinds = []Kind{
	{"deploy_failed", "Deploy failed", "An apply, push, rollback or preview failed.", true, true},
	{"restart", "Container restarted", "A container died or restarted without vops stopping it.", true, true},
	{"oom", "Out of memory", "A container was OOM-killed or SIGKILLed, or the kernel OOM killer ran on the host.", true, true},
	{"down", "Service down", "A routed service has no running replica, or doesn't answer its probe.", true, false},
	{"health", "Unhealthy", "The health path answers 4xx/5xx, or the compose healthcheck fails.", true, false},
	{"resources", "Resources", "Host cpu, memory, disk or memory pressure high for a while; containers near their limits.", true, false},
	{"resolved", "Resolved", "Also tell me when an alert I got is over.", false, false},
	{"deploy_ok", "Deploy succeeded", "Every successful deploy.", false, false},
}

func kindOf(id string) (Kind, bool) {
	i := slices.IndexFunc(Kinds, func(k Kind) bool { return k.ID == id })
	if i < 0 {
		return Kind{}, false
	}
	return Kinds[i], true
}

// Settings are the global thresholds and intervals (notify_settings; a missing key is its default).
type Settings struct {
	Repeat        int64 `json:"repeat"`         // seconds between re-sends of an open alert
	Expire        int64 `json:"expire"`         // seconds after the first occurrence when sending stops
	Quiet         int64 `json:"quiet"`          // seconds without a new occurrence before an event alert resolves
	ProbeInterval int64 `json:"probe_interval"` // seconds between probes of routed services
	DownAfter     int64 `json:"down_after"`     // consecutive failed probes before down/health
	SampleEvery   int64 `json:"sample_every"`   // seconds between resource samples
	CPU           int64 `json:"cpu"`            // host cpu %
	CPUFor        int64 `json:"cpu_for"`        // seconds
	MemAvail      int64 `json:"mem_avail"`      // host memory available below this %
	MemFor        int64 `json:"mem_for"`        // seconds
	Disk          int64 `json:"disk"`           // filesystem used %
	PSI           int64 `json:"psi"`            // memory pressure some avg60 %
	CtrMem        int64 `json:"ctr_mem"`        // container memory % of its limit
	CtrMemFor     int64 `json:"ctr_mem_for"`
	CtrCPU        int64 `json:"ctr_cpu"` // container cpu % of its quota (or of the host)
	CtrCPUFor     int64 `json:"ctr_cpu_for"`
}

var Defaults = Settings{Repeat: 3600, Expire: 86400, Quiet: 1800, ProbeInterval: 30, DownAfter: 3, SampleEvery: 30,
	CPU: 90, CPUFor: 600, MemAvail: 10, MemFor: 300, Disk: 90, PSI: 20, CtrMem: 90, CtrMemFor: 300, CtrCPU: 90, CtrCPUFor: 600}

// fields lists every setting with its bounds, in the order the dashboard shows them.
func (s *Settings) fields() []struct {
	key      string
	v        *int64
	min, max int64
} {
	type f = struct {
		key      string
		v        *int64
		min, max int64
	}
	const day = 86400
	return []f{{"repeat", &s.Repeat, 60, 7 * day}, {"expire", &s.Expire, 60, 30 * day}, {"quiet", &s.Quiet, 10, 7 * day},
		{"probe_interval", &s.ProbeInterval, 1, 3600}, {"down_after", &s.DownAfter, 1, 100}, {"sample_every", &s.SampleEvery, 1, 3600},
		{"cpu", &s.CPU, 1, 100}, {"cpu_for", &s.CPUFor, 0, day}, {"mem_avail", &s.MemAvail, 1, 99}, {"mem_for", &s.MemFor, 0, day},
		{"disk", &s.Disk, 1, 100}, {"psi", &s.PSI, 1, 100}, {"ctr_mem", &s.CtrMem, 1, 100}, {"ctr_mem_for", &s.CtrMemFor, 0, day},
		{"ctr_cpu", &s.CtrCPU, 1, 1000}, {"ctr_cpu_for", &s.CtrCPUFor, 0, day}}
}

// SettingsFrom applies stored values on top of the defaults (unknown or out of range values are ignored).
func SettingsFrom(m map[string]int64) Settings {
	s := Defaults
	for _, f := range s.fields() {
		if v, ok := m[f.key]; ok && v >= f.min && v <= f.max {
			*f.v = v
		}
	}
	return s
}

// Overrides validates s and returns what differs from the defaults (what the db stores).
func (s Settings) Overrides() (map[string]int64, error) {
	out := map[string]int64{}
	d := Defaults
	df := d.fields()
	for i, f := range s.fields() {
		if *f.v < f.min || *f.v > f.max {
			return nil, fmt.Errorf("%s must be between %d and %d", f.key, f.min, f.max)
		}
		if *f.v != *df[i].v {
			out[f.key] = *f.v
		}
	}
	return out, nil
}

func sec(n int64) time.Duration { return time.Duration(n) * time.Second }

// hold is how long an alert stays live without being seen again: the quiet period for event kinds,
// a few detector cycles for conditions (a condition is re-seen every cycle while it holds).
func (s Settings) hold(kind string) time.Duration {
	switch kind {
	case "down", "health":
		return 3*sec(s.ProbeInterval) + 5*time.Second
	case "resources":
		return 3*sec(s.SampleEvery) + 5*time.Second
	}
	return sec(s.Quiet)
}
