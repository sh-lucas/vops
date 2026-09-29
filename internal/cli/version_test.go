package cli

import (
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]semver{
		"v1.2.0": {1, 2}, "1.3": {1, 3}, "v1.2.0+dirty": {1, 2}, "v1.2.0-rc1": {1, 2}, "vops v2.10.3": {2, 10},
		"v1.2.1-0.20260929164028-e25b19b88724+dirty": {1, 2},
	} {
		if got, ok := parseVersion(in); !ok || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"dev", "(devel)", "", "v1", "vx.y"} {
		if _, ok := parseVersion(in); ok {
			t.Errorf("%q should not parse", in)
		}
	}
}

func TestVersionMismatch(t *testing.T) {
	if msg := versionMismatch("v1.3.0", "v1.3.7+dirty"); msg != "" {
		t.Fatalf("patch differences are fine: %s", msg)
	}
	if msg := versionMismatch("v1.3.0", "v1.2.0"); !strings.Contains(msg, "the host runs vops v1.3, you have v1.2: update yours") {
		t.Fatalf("older cli: %s", msg)
	}
	if msg := versionMismatch("v1.3.0", "v2.0.0-rc1"); !strings.Contains(msg, "run `vops install` to upgrade it") {
		t.Fatalf("newer cli: %s", msg)
	}
	if msg := versionMismatch("dev", "v1.2.0"); msg != "" {
		t.Fatalf("dev builds skip: %s", msg)
	}
	if checkPlanVersion("v1.3.0") != nil {
		t.Fatal("a host that reports its version already checked itself")
	}
}
