package daemon

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/podman"
)

// logQuery is what /api/logs takes.
type logQuery struct {
	N      int
	Follow bool
	Grep   string
	Before string // journald cursor: the n lines older than it (no follow); X-Vops-Before carries the next one
}

// logs streams container logs from journald (history across replicas), or podman logs as a fallback.
// Lines look like "2006-01-02 15:04:05 service | message".
func (d *Daemon) logs(ctx context.Context, w http.ResponseWriter, project, service string, q logQuery) error {
	if project == "" {
		return fmt.Errorf("project is required")
	}
	if q.N <= 0 || q.N > 10000 {
		q.N = 200
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var services []string
	if service != "" {
		services = []string{service}
	} else {
		cs, err := podman.PS(ctx, deploy.LProject+"="+project)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, c := range cs {
			if s := c.Labels[deploy.LService]; !seen[s] {
				seen[s] = true
				services = append(services, s)
			}
		}
		if len(services) == 0 {
			fmt.Fprintln(w, "no containers")
			return nil
		}
	}
	if ok, err := d.journal(ctx, w, project, services, q); ok || err != nil {
		return err
	}
	if q.Before != "" {
		return nil // podman logs has no cursors: nothing older
	}
	return podmanLogs(ctx, flushWriter{w}, project, service, q.N, q.Follow, q.Grep)
}

var journalGrep = sync.OnceValue(func() bool {
	out, _ := exec.Command("journalctl", "--version").Output()
	return strings.Contains(string(out), "+PCRE2")
})

// journal reads the last n lines (older than q.Before when set), then follows from where they ended.
// It returns false when journald has nothing for these services, so podman logs is used instead.
func (d *Daemon) journal(ctx context.Context, w http.ResponseWriter, project string, services []string, q logQuery) (bool, error) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return false, nil
	}
	var matches []string
	for _, s := range services {
		matches = append(matches, "SYSLOG_IDENTIFIER=vops."+compose.Slug(project)+"."+s)
	}
	args := func(user bool, extra ...string) []string {
		a := append([]string{"-o", "json", "--no-pager"}, extra...)
		if user {
			a = append(a, "--user")
		}
		return append(a, matches...)
	}
	// rootless logs land in the user journal on some systems and in the system one on others
	user := false
	if os.Getuid() != 0 {
		out, _ := exec.CommandContext(ctx, "journalctl", args(true, "-n", "1")...).Output()
		user = len(out) > 0
	}
	if !user {
		if out, _ := exec.CommandContext(ctx, "journalctl", args(false, "-n", "1")...).Output(); len(out) == 0 {
			return false, nil
		}
	}
	filter, window := q.Grep, q.N
	var grep []string
	if q.Grep != "" {
		if journalGrep() {
			// journalctl reads backwards until n matches, so the search covers the whole history
			grep, filter = []string{"--case-sensitive=false", "--grep=" + regexp.QuoteMeta(q.Grep)}, ""
		} else {
			window = 20000 // no PCRE2: filter in go over a bigger window
		}
	}
	// newest first, always: --grep with -n reverses on its own, and "after" a cursor in reverse means older
	hist := append([]string{"-r", "-n", strconv.Itoa(window)}, grep...)
	if q.Before != "" {
		hist = append(hist, "--after-cursor="+q.Before)
	}
	var all []jline
	if err := journalRead(ctx, args(user, hist...), filter, func(l jline) { all = append(all, l) }); err != nil {
		return true, err
	}
	slices.Reverse(all)
	var shown []jline
	for _, l := range all {
		if l.match {
			shown = append(shown, l)
		}
	}
	// a full window means there may be older lines: tell the client where to continue
	before := ""
	if len(shown) > q.N {
		shown = shown[len(shown)-q.N:]
		before = shown[0].cursor
	} else if len(all) == window {
		before = all[0].cursor
	}
	if before != "" {
		w.Header().Set("X-Vops-Before", before)
	}
	fw := flushWriter{w}
	for _, l := range shown {
		fmt.Fprintln(fw, l.text)
	}
	if !q.Follow || q.Before != "" {
		return true, nil
	}
	http.NewResponseController(w).Flush() // send the headers even when there is no history yet
	follow := append([]string{"-f"}, grep...)
	if len(all) > 0 {
		follow = append(follow, "--after-cursor="+all[len(all)-1].cursor)
	} else {
		follow = append(follow, "-n", "0")
	}
	journalRead(ctx, args(user, follow...), filter, func(l jline) {
		if l.match {
			fmt.Fprintln(fw, l.text)
		}
	})
	return true, nil
}

type jline struct {
	text, cursor string
	match        bool
}

// journalRead runs journalctl -o json and calls each for every entry, until it exits or ctx is done.
func journalRead(ctx context.Context, args []string, grep string, each func(jline)) error {
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if l, ok := journalLine(sc.Bytes(), grep); ok {
			each(l)
		}
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		// --grep exits 1 when nothing matches, with nothing on stderr
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("journalctl: %s", msg)
		}
	}
	return nil
}

func journalLine(raw []byte, grep string) (jline, bool) {
	var e map[string]any
	if json.Unmarshal(raw, &e) != nil {
		return jline{}, false
	}
	l := jline{cursor: fmt.Sprint(e["__CURSOR"])}
	msg := ""
	switch m := e["MESSAGE"].(type) {
	case string:
		msg = m
	case []any: // journald encodes non-utf8 messages as byte arrays
		b := make([]byte, 0, len(m))
		for _, x := range m {
			if f, ok := x.(float64); ok {
				b = append(b, byte(f))
			}
		}
		msg = string(b)
	}
	msg = strings.TrimRight(msg, "\n")
	if grep != "" && !strings.Contains(strings.ToLower(msg), strings.ToLower(grep)) {
		return l, true
	}
	ts := ""
	if us, err := strconv.ParseInt(fmt.Sprint(e["__REALTIME_TIMESTAMP"]), 10, 64); err == nil {
		ts = time.UnixMicro(us).Format("2006-01-02 15:04:05")
	}
	l.text, l.match = ts+" "+fmt.Sprint(e["CONTAINER_NAME"])+" | "+msg, true
	return l, true
}

func podmanLogs(ctx context.Context, w io.Writer, project, service string, n int, follow bool, grep string) error {
	filters := []string{deploy.LProject + "=" + project}
	if service != "" {
		filters = append(filters, deploy.LService+"="+service)
	}
	cs, err := podman.PS(ctx, filters...)
	if err != nil {
		return err
	}
	args := []string{"logs", "--names", "--timestamps", "--tail", strconv.Itoa(n)}
	if follow {
		args = append(args, "-f")
	}
	for _, c := range cs {
		args = append(args, c.ID)
	}
	cmd := exec.CommandContext(ctx, podman.Bin(), args...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { cmd.Wait(); pw.Close() }()
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		if grep == "" || strings.Contains(strings.ToLower(sc.Text()), strings.ToLower(grep)) {
			fmt.Fprintln(w, sc.Text())
		}
	}
	return nil
}
