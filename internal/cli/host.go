package cli

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/store"
)

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func getJSON(c *client, path string, v any) error {
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.UnmarshalRead(resp.Body, v, json.MatchCaseInsensitiveNames(true))
}

func post(c *client, method, path string, body any, v any) error {
	var r io.Reader
	if body != nil {
		r = jsonBody(body)
	}
	resp, err := c.do(method, path, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if v == nil {
		return nil
	}
	return json.UnmarshalRead(resp.Body, v, json.MatchCaseInsensitiveNames(true))
}

// runHere runs a host command against the local daemon socket.
func runHere(cmd string, args []string, stdin io.Reader, out io.Writer) error {
	c, err := socketClient()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	yes := fs.Bool("yes", false, "")
	fs.BoolVar(yes, "y", false, "")
	commit := fs.String("commit", "", "")
	follow := fs.Bool("f", false, "")
	n := fs.Int("n", 200, "")
	grep := fs.String("grep", "", "")
	pattern := fs.String("pattern", "", "")
	var repos multi
	fs.Var(&repos, "repo", "")
	fromStdin := fs.Bool("stdin", false, "")
	note := fs.String("m", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	arg := func(i int, name string) (string, error) {
		if i >= len(pos) {
			return "", fmt.Errorf("%s: missing %s", cmd, name)
		}
		return pos[i], nil
	}

	switch cmd {
	case "status":
		if *asJSON {
			resp, err := c.do("GET", "/api/status", nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, err = io.Copy(out, resp.Body)
			return err
		}
		return printStatus(c, out)

	case "plan":
		q := "?format=text"
		if *asJSON {
			q = ""
		}
		resp, err := c.do("GET", "/api/plan"+q, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(out, resp.Body)
		return err

	case "apply":
		if !*yes {
			return errors.New("apply on the host needs --yes (the cli shows the plan first)")
		}
		resp, err := c.do("POST", "/api/apply", jsonBody(map[string]any{"commit": *commit, "projects": pos}))
		if err != nil {
			return err
		}
		return stream(resp, out)

	case "logs":
		project, err := arg(0, "project")
		if err != nil {
			return err
		}
		service, _ := arg(1, "")
		q := url.Values{"project": {project}, "service": {service}, "n": {fmt.Sprint(*n)}, "grep": {*grep}}
		if *follow {
			q.Set("follow", "1")
		}
		resp, err := c.do("GET", "/api/logs?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(out, resp.Body)
		return err

	case "restart":
		project, err := arg(0, "project")
		if err != nil {
			return err
		}
		service, _ := arg(1, "")
		if err := post(c, "POST", "/api/restart", map[string]string{"project": project, "service": service}, nil); err != nil {
			return err
		}
		fmt.Fprintln(out, "restarted")
		return nil

	case "enable", "disable":
		project, err := arg(0, "project")
		if err != nil {
			return err
		}
		if err := post(c, "POST", "/api/project", map[string]any{"project": project, "disabled": cmd == "disable"}, nil); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %sd; run `vops apply` to act on it\n", project, cmd)
		return nil

	case "events":
		project, _ := arg(0, "")
		var evs []store.Event
		if err := getJSON(c, "/api/events?project="+url.QueryEscape(project), &evs); err != nil {
			return err
		}
		for i := len(evs) - 1; i >= 0; i-- {
			e := evs[i]
			fmt.Fprintf(out, "%s  %-8s %-20s %s\n", time.Unix(e.At, 0).Format("2006-01-02 15:04:05"), e.Kind, e.Project, e.Message)
		}
		return nil

	case "env":
		sub, err := arg(0, "ls|set|rm")
		if err != nil {
			return err
		}
		project, err := arg(1, "project")
		if err != nil {
			return err
		}
		switch sub {
		case "ls":
			var keys []store.EnvKey
			if err := getJSON(c, "/api/env?project="+url.QueryEscape(project), &keys); err != nil {
				return err
			}
			for _, k := range keys {
				fmt.Fprintf(out, "%s\t(set %s)\n", k.Key, time.Unix(k.UpdatedAt, 0).Format("2006-01-02 15:04"))
			}
			return nil
		case "set":
			if !*fromStdin {
				return errors.New("env set on the host reads KEY=VALUE lines with --stdin")
			}
			sc := bufio.NewScanner(stdin)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			count := 0
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
				if !ok {
					return fmt.Errorf("expected KEY=VALUE, got %q", k)
				}
				v = strings.TrimSpace(v)
				if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
					v = v[1 : len(v)-1]
				}
				if err := post(c, "POST", "/api/env", map[string]string{"project": project, "key": strings.TrimSpace(k), "value": v}, nil); err != nil {
					return err
				}
				count++
			}
			fmt.Fprintf(out, "%d variable(s) set on %s; run `vops apply` to deploy them\n", count, project)
			return sc.Err()
		case "rm":
			for _, k := range pos[2:] {
				if err := post(c, "DELETE", "/api/env?project="+url.QueryEscape(project)+"&key="+url.QueryEscape(k), nil, nil); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "removed; run `vops apply` to deploy\n")
			return nil
		}
		return fmt.Errorf("env %s: unknown", sub)

	case "user":
		sub, err := arg(0, "ls|add|rm|token")
		if err != nil {
			return err
		}
		switch sub {
		case "ls":
			var users []store.User
			if err := getJSON(c, "/api/users", &users); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPATTERN\tREPOS")
			for _, u := range users {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Name, u.Pattern, strings.Join(u.Repos, ","))
			}
			return tw.Flush()
		case "add", "token":
			name, err := arg(1, "name")
			if err != nil {
				return err
			}
			body := map[string]any{"name": name, "pattern": *pattern, "repos": []string(repos), "new_token": sub == "token"}
			if sub == "token" {
				var users []store.User
				getJSON(c, "/api/users", &users)
				found := false
				for _, u := range users {
					if u.Name == name {
						body["pattern"], body["repos"], found = u.Pattern, u.Repos, true
					}
				}
				if !found {
					return fmt.Errorf("no user %q", name)
				}
			} else if *pattern == "" && len(repos) == 0 {
				return errors.New("user add needs --pattern and/or --repo (what may this user push?)")
			}
			var res struct{ Name, Token string }
			if err := post(c, "POST", "/api/users", body, &res); err != nil {
				return err
			}
			if res.Token == "" {
				fmt.Fprintf(out, "updated %s (token unchanged)\n", name)
				return nil
			}
			fmt.Fprintf(out, "user:  %s\ntoken: %s\n\nshown once. log in with:\n  podman login -u %s registry.<your domain>\n", name, res.Token, name)
			return nil
		case "rm":
			name, err := arg(1, "name")
			if err != nil {
				return err
			}
			return post(c, "DELETE", "/api/users?name="+url.QueryEscape(name), nil, nil)
		}
		return fmt.Errorf("user %s: unknown", sub)

	case "registry":
		sub, err := arg(0, "ls|rm|gc")
		if err != nil {
			return err
		}
		switch sub {
		case "ls":
			var res struct {
				Host  string
				Repos []struct {
					Name string
					Tags []registry.Tag
				}
			}
			if err := getJSON(c, "/api/registry", &res); err != nil {
				return err
			}
			filter, _ := arg(1, "")
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "IMAGE\tDIGEST\tSIZE\tPUSHED")
			for _, r := range res.Repos {
				if filter != "" && r.Name != filter {
					continue
				}
				for _, t := range r.Tags {
					fmt.Fprintf(tw, "%s:%s\t%.19s\t%s\t%s\n", r.Name, t.Name, t.Digest, humanSize(t.Size), t.PushedAt.Format("2006-01-02 15:04"))
				}
			}
			return tw.Flush()
		case "rm":
			ref, err := arg(1, "repo:tag")
			if err != nil {
				return err
			}
			i := strings.LastIndex(ref, ":")
			if i < 0 {
				return errors.New("expected repo:tag")
			}
			if err := post(c, "DELETE", "/api/registry?repo="+url.QueryEscape(ref[:i])+"&tag="+url.QueryEscape(ref[i+1:]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(out, "deleted; space is freed by `vops registry gc`")
			return nil
		case "gc":
			var res registry.GCResult
			if err := post(c, "POST", "/api/registry/gc", nil, &res); err != nil {
				return err
			}
			fmt.Fprintf(out, "removed %d manifests and %d blobs, freed %s\n", res.Manifests, res.Blobs, humanSize(res.Freed))
			return nil
		}
		return fmt.Errorf("registry %s: unknown", sub)

	case "snapshot":
		sub, err := arg(0, "ls|create|rm")
		if err != nil {
			return err
		}
		switch sub {
		case "ls":
			project, _ := arg(1, "")
			var res struct {
				Snapshots []store.Snapshot  `json:"snapshots"`
				Data      *deploy.DataState `json:"data"`
			}
			if err := getJSON(c, "/api/snapshots?project="+url.QueryEscape(project), &res); err != nil {
				return err
			}
			if *asJSON {
				b, _ := json.Marshal(res)
				_, err := fmt.Fprintln(out, string(b))
				return err
			}
			if d := res.Data; d != nil {
				switch {
				case !d.Supported:
					fmt.Fprintf(out, "snapshots unavailable: %s\n", d.Reason)
				case len(d.Protected) > 0:
					fmt.Fprintf(out, "protected: %s\n", strings.Join(d.Protected, ", "))
				}
				if len(d.Unprotected) > 0 {
					fmt.Fprintf(out, "not covered (not btrfs subvolumes): %s\n", strings.Join(d.Unprotected, ", "))
				}
				fmt.Fprintln(out)
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tPROJECT\tREASON\tTAKEN\tCOMMIT\tDATA\tNOTE")
			for _, s := range res.Snapshots {
				var names []string
				for _, v := range s.Volumes {
					names = append(names, v.Name)
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%.12s\t%s\t%s\n", s.ID, s.Project, s.Reason, ago(s.CreatedAt), s.Commit, strings.Join(names, ","), s.Note)
			}
			return tw.Flush()
		case "create":
			project, err := arg(1, "project")
			if err != nil {
				return err
			}
			var res struct {
				Snapshot store.Snapshot `json:"snapshot"`
				Log      string         `json:"log"`
			}
			if err := post(c, "POST", "/api/snapshots", map[string]string{"project": project, "note": *note}, &res); err != nil {
				return err
			}
			fmt.Fprint(out, res.Log)
			return nil
		case "rm":
			id, err := arg(1, "id")
			if err != nil {
				return err
			}
			if err := post(c, "DELETE", "/api/snapshots?id="+url.QueryEscape(id), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(out, "snapshot #%s deleted\n", id)
			return nil
		}
		return fmt.Errorf("snapshot %s: unknown", sub)

	case "rollback":
		if !*yes {
			return errors.New("rollback on the host needs --yes (the cli shows what it restores first)")
		}
		project, err := arg(0, "project")
		if err != nil {
			return err
		}
		idArg, _ := arg(1, "")
		var id int64
		fmt.Sscan(idArg, &id)
		resp, err := c.do("POST", "/api/rollback", jsonBody(map[string]any{"project": project, "id": id}))
		if err != nil {
			return err
		}
		return stream(resp, out)

	case "audit":
		var logs []store.Audit
		if err := getJSON(c, fmt.Sprintf("/api/audit?n=%d", *n), &logs); err != nil {
			return err
		}
		for i := len(logs) - 1; i >= 0; i-- {
			l := logs[i]
			fmt.Fprintf(out, "%s  %-9s %-6s %s  %s\n", time.Unix(l.At, 0).Format("2006-01-02 15:04:05"), l.Tbl, l.Op, l.Key, l.Detail)
		}
		return nil

	case "admin":
		if sub, _ := arg(0, ""); sub != "password" || !*fromStdin {
			return errors.New("usage: admin password")
		}
		pw, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && pw == "" {
			return err
		}
		if err := post(c, "POST", "/api/admin/password", map[string]string{"password": strings.TrimRight(pw, "\r\n")}, nil); err != nil {
			return err
		}
		fmt.Fprintln(out, "dashboard password changed; every session was logged out")
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

func ago(unix int64) string {
	if unix == 0 {
		return "never"
	}
	d := time.Since(time.Unix(unix, 0)).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func printStatus(c *client, out io.Writer) error {
	var st struct {
		Domain   string
		Commit   string
		Changes  bool
		Warnings []string
		Projects []struct {
			Path      string
			Disabled  bool
			Gone      bool
			Error     string
			Commit    string
			AppliedAt int64 `json:"applied_at"`
			Services  []deploy.ServiceState
		}
	}
	if err := getJSON(c, "/api/status", &st); err != nil {
		return err
	}
	fmt.Fprintf(out, "commit %.12s  domain %s\n", st.Commit, orDash(st.Domain))
	for _, w := range st.Warnings {
		fmt.Fprintf(out, "! %s\n", w)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, p := range st.Projects {
		note := fmt.Sprintf("applied %.12s %s", p.Commit, ago(p.AppliedAt))
		switch {
		case p.Gone:
			note = "removed from git"
		case p.Disabled:
			note = "disabled"
		}
		fmt.Fprintf(tw, "%s\t\t%s\n", p.Path, note)
		if p.Error != "" {
			fmt.Fprintf(tw, "  ✗ %s\t\t\n", p.Error)
		}
		for _, s := range p.Services {
			running, state := 0, "running"
			for _, ct := range s.Containers {
				if ct.State == "running" {
					running++
				}
				if ct.Labels["vops.job"] != "" {
					state = "done"
					if (ct.State == "exited" || ct.State == "stopped") && ct.ExitCode == 0 {
						running++
					}
				}
			}
			info := ""
			if len(s.Domains) > 0 {
				info = "http://" + s.Domains[0]
				if st.Domain != "" {
					info = "https://" + s.Domains[0]
				}
			}
			if s.Pending != "" {
				info = strings.TrimSpace(info + "  pending: " + s.Pending + " " + s.Reason)
			}
			fmt.Fprintf(tw, "  %s\t%d/%d %s\t%s\n", s.Name, running, len(s.Containers), state, info)
		}
	}
	tw.Flush()
	if st.Changes {
		fmt.Fprintln(out, "\nthe host differs from git: run `vops apply`")
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
