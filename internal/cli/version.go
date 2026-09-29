package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// exitVersion is the exit code when the cli and the host differ in major.minor.
const exitVersion = 3

const updateCmd = "go install github.com/sh-lucas/vops/cmd/vops@latest"

// versionError refuses a command because the cli and the host run different major.minor versions.
type versionError struct{ msg string }

func (e *versionError) Error() string { return e.msg }

type semver struct{ major, minor int }

func (v semver) String() string { return fmt.Sprintf("v%d.%d", v.major, v.minor) }

func (v semver) less(o semver) bool {
	return v.major < o.major || v.major == o.major && v.minor < o.minor
}

// parseVersion reads the major.minor of "v1.2.0", "1.2", "v1.2.0-rc1", "v1.2.0+dirty", "vops v1.2.0".
// Dev builds ("dev", "(devel)") are not versions.
func parseVersion(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "vops ")
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return semver{}, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || major < 0 || minor < 0 {
		return semver{}, false
	}
	return semver{major, minor}, true
}

// versionMismatch is the refusal for a cli at version client talking to a host at version host, or "".
func versionMismatch(host, client string) string {
	h, ok1 := parseVersion(host)
	c, ok2 := parseVersion(client)
	switch {
	case !ok1 || !ok2 || h == c:
		return ""
	case c.less(h):
		return fmt.Sprintf("the host runs vops %s, you have %s: update yours (%s)", h, c, updateCmd)
	default:
		return fmt.Sprintf("the host runs vops %s, you have %s: run `vops install` to upgrade it", h, c)
	}
}

// checkClient runs on the host when a cli forwarded a command (VOPS_CLIENT is its version).
func checkClient(client string) error {
	_, ok1 := parseVersion(Version)
	_, ok2 := parseVersion(client)
	if !ok1 || !ok2 {
		fmt.Fprintf(os.Stderr, "vops: version check skipped (dev build: host %s, cli %s)\n", Version, client)
		return nil
	}
	if msg := versionMismatch(Version, client); msg != "" {
		return &versionError{msg}
	}
	return nil
}

// checkPlanVersion catches hosts from before the check (they ignore VOPS_CLIENT): their plan has no version.
func checkPlanVersion(host string) error {
	if host != "" {
		return nil
	}
	if c, ok := parseVersion(Version); ok && (semver{1, 2}).less(c) {
		return &versionError{fmt.Sprintf("the host runs vops v1.2 or older, you have %s: run `vops install` to upgrade it", c)}
	}
	return nil
}
