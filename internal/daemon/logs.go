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
	"strconv"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/podman"
)

// logs streams container logs from journald (history across replicas), or podman logs as a fallback.
// Lines look like "2006-01-02 15:04:05 service | message".
func (d *Daemon) logs(ctx context.Context, w http.ResponseWriter, project, service string, n int, follow bool, grep string) error {
	if project == "" {
		return fmt.Errorf("project is required")
	}
	if n <= 0 || n > 10000 {
		n = 200
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
	fw := flushWriter{w}
	if ok := d.journal(ctx, fw, project, services, n, follow, grep); ok {
		return nil
	}
	return podmanLogs(ctx, fw, project, service, n, follow, grep)
}

func (d *Daemon) journal(ctx context.Context, w io.Writer, project string, services []string, n int, follow bool, grep string) bool {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return false
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
			return false
		}
	}
	window := n
	if grep != "" {
		window = 20000 // filter in go over a bigger window
	}
	extra := []string{"-n", strconv.Itoa(window)}
	if follow {
		extra = append(extra, "-f")
	}
	cmd := exec.CommandContext(ctx, "journalctl", args(user, extra...)...)
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return false
	}
	defer cmd.Wait()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	// until journalctl caught up (history), keep only the last n matches; then stream
	var hist []string
	streaming := false
	flush := func() {
		if len(hist) > n {
			hist = hist[len(hist)-n:]
		}
		for _, l := range hist {
			fmt.Fprintln(w, l)
		}
		hist, streaming = nil, true
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		for sc.Scan() {
			if line, ok := journalLine(sc.Bytes(), grep); ok {
				lines <- line
			} else {
				lines <- ""
			}
		}
	}()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				flush()
				return true
			}
			if line == "" {
				continue
			}
			if streaming {
				fmt.Fprintln(w, line)
			} else {
				hist = append(hist, line)
			}
		case <-time.After(300 * time.Millisecond):
			if !streaming {
				flush()
			}
		case <-ctx.Done():
			return true
		}
	}
}

func journalLine(raw []byte, grep string) (string, bool) {
	var e map[string]any
	if json.Unmarshal(raw, &e) != nil {
		return "", false
	}
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
		return "", false
	}
	ts := ""
	if us, err := strconv.ParseInt(fmt.Sprint(e["__REALTIME_TIMESTAMP"]), 10, 64); err == nil {
		ts = time.UnixMicro(us).Format("2006-01-02 15:04:05")
	}
	name := fmt.Sprint(e["CONTAINER_NAME"])
	return ts + " " + name + " | " + msg, true
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
