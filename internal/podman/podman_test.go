package podman

import (
	"strings"
	"testing"
	"time"
)

func TestTimeout(t *testing.T) {
	for args, want := range map[string]time.Duration{
		"ps -a":                2 * time.Minute,
		"pull -q x":            30 * time.Minute,
		"build -q -t img .":    30 * time.Minute,
		"rm -f -t 30 abc":      2*time.Minute + 30*time.Second,
		"stop -t":              2 * time.Minute,
		"run -d -t --name x y": 2 * time.Minute,
	} {
		if got := Timeout(strings.Fields(args)); got != want {
			t.Errorf("%s: %s, want %s", args, got, want)
		}
	}
}
