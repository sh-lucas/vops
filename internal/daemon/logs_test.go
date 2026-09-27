package daemon

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestJournalPaging writes lines to the real journal (skipped without one) and pages back through them.
func TestJournalPaging(t *testing.T) {
	if _, err := exec.LookPath("systemd-cat"); err != nil {
		t.Skip("no journald")
	}
	project := fmt.Sprintf("logtest%d", time.Now().UnixNano())
	cat := exec.Command("systemd-cat", "-t", "vops."+project+".svc")
	var in strings.Builder
	for i := range 25 {
		fmt.Fprintf(&in, "line %02d\n", i)
	}
	cat.Stdin = strings.NewReader(in.String())
	if err := cat.Run(); err != nil {
		t.Skip("systemd-cat:", err)
	}
	d := &Daemon{}
	get := func(q logQuery) (lines []string, before string) {
		rec := httptest.NewRecorder()
		ok, err := d.journal(context.Background(), rec, project, []string{"svc"}, q)
		if err != nil || !ok {
			return nil, ""
		}
		for l := range strings.Lines(rec.Body.String()) {
			lines = append(lines, l[strings.Index(l, "| ")+2:len(l)-1])
		}
		return lines, rec.Header().Get("X-Vops-Before")
	}
	var lines []string
	for range 50 { // journald writes asynchronously
		if lines, _ = get(logQuery{N: 100}); len(lines) == 25 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(lines) != 25 {
		t.Skipf("journal not readable here (%d lines)", len(lines))
	}

	var all []string
	page, before := get(logQuery{N: 10})
	for all = page; before != ""; all = append(page, all...) {
		page, before = get(logQuery{N: 10, Before: before})
	}
	if strings.Join(all, ",") != strings.Join(lines, ",") {
		t.Fatalf("paged:\n%v\nwant:\n%v", all, lines)
	}

	pcre := journalGrep
	defer func() { journalGrep = pcre }()
	for _, native := range []bool{true, false} { // journalctl --grep, then the go filter
		journalGrep = func() bool { return native }
		found, before := get(logQuery{N: 2, Grep: "LINE 0"})
		if strings.Join(found, ",") != "line 08,line 09" || before == "" {
			t.Fatalf("grep (native %v): %v %q", native, found, before)
		}
		found, _ = get(logQuery{N: 5, Grep: "LINE 0", Before: before})
		if strings.Join(found, ",") != "line 03,line 04,line 05,line 06,line 07" {
			t.Fatalf("grep before (native %v): %v", native, found)
		}
	}

	// follow: the last lines, then new ones as they come
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		time.Sleep(300 * time.Millisecond)
		late := exec.Command("systemd-cat", "-t", "vops."+project+".svc")
		late.Stdin = strings.NewReader("late line\n")
		late.Run()
	}()
	rec := httptest.NewRecorder()
	d.journal(ctx, rec, project, []string{"svc"}, logQuery{N: 2, Follow: true})
	if out := rec.Body.String(); strings.Count(out, "\n") != 3 || !strings.Contains(out, "line 24") || !strings.HasSuffix(out, "| late line\n") {
		t.Fatalf("follow:\n%s", out)
	}
}
