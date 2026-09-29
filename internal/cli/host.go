package cli

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/store"
)

// parseLogTime accepts 2006-01-02, "2006-01-02 15:04", RFC3339, or a duration like "2h" (meaning now minus it).
// Dates without a zone are read in local time. Empty input means unset.
func parseLogTime(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d).Unix(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix(), nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("invalid time %q: want 2006-01-02, \"2006-01-02 15:04\", RFC3339, or a duration like 2h", s)
}

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
	trigger := fs.String("trigger", "apply", "")
	follow := fs.Bool("f", false, "")
	n := fs.Int("n", 200, "")
	grep := fs.String("grep", "", "")
	since := fs.String("since", "", "")
	until := fs.String("until", "", "")
	pattern := fs.String("pattern", "", "")
	var repos multi
	fs.Var(&repos, "repo", "")
	fromStdin := fs.Bool("stdin", false, "")
	tokenStdin := fs.Bool("token-stdin", false, "")
	note := fs.String("m", "", "")
	preview := fs.Bool("preview", false, "")
	name := fs.String("name", "", "")
	ref := fs.String("ref", "", "")
	from := fs.Int64("from", 0, "")
	var images multi
	fs.Var(&images, "image", "")
	imagesOnly := fs.Bool("images", false, "")
	dataOnly := fs.Bool("data", false, "")
	var services multi
	fs.Var(&services, "service", "")
	snapshotID := fs.Int64("snapshot", 0, "")
	planOnly := fs.Bool("plan", false, "")
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
		resp, err := c.do("POST", "/api/apply", jsonBody(map[string]any{"commit": *commit, "projects": pos, "trigger": *trigger}))
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
		sinceU, err := parseLogTime(*since)
		if err != nil {
			return err
		}
		untilU, err := parseLogTime(*until)
		if err != nil {
			return err
		}
		q := url.Values{"project": {project}, "service": {service}, "n": {fmt.Sprint(*n)}, "grep": {*grep}}
		if *follow {
			q.Set("follow", "1")
		}
		if sinceU != 0 {
			q.Set("since", fmt.Sprint(sinceU))
		}
		if untilU != 0 {
			q.Set("until", fmt.Sprint(untilU))
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
		scope, applyHint := "", "run `vops apply` to deploy"
		if *preview {
			scope, applyHint = "&preview=1", "previews get it on their next `vops preview up`"
		}
		switch sub {
		case "ls":
			var keys []store.EnvKey
			if err := getJSON(c, "/api/env?project="+url.QueryEscape(project)+scope, &keys); err != nil {
				return err
			}
			for _, k := range keys {
				fmt.Fprintf(out, "%s\t(set %s)\n", k.Key, time.Unix(k.UpdatedAt, 0).Format("2006-01-02 15:04"))
			}
			// then what each service gets and from where (values never leave the host)
			var u deploy.EnvUsage
			if err := getJSON(c, "/api/env/usage?project="+url.QueryEscape(project)+scope, &u); err != nil {
				fmt.Fprintf(out, "\n(per-service view unavailable: %v)\n", err)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			for _, svc := range slices.Sorted(maps.Keys(u.Services)) {
				fmt.Fprintf(tw, "\n%s\t\t\n", svc)
				for _, en := range u.Services[svc] {
					fmt.Fprintf(tw, "  %s\t%s\t%s\n", en.Key, strings.TrimSpace(en.Source+" "+en.File), envHint(en, project, *preview))
				}
			}
			if len(u.Unused) > 0 {
				fmt.Fprintf(tw, "\nunused (set, but no service uses them): %s\t\t\n", strings.Join(u.Unused, ", "))
			}
			return tw.Flush()
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
				if err := post(c, "POST", "/api/env", map[string]any{"project": project, "key": strings.TrimSpace(k), "value": v, "preview": *preview}, nil); err != nil {
					return err
				}
				count++
			}
			fmt.Fprintf(out, "%d %s variable(s) set on %s; %s\n", count, map[bool]string{true: "preview", false: "env"}[*preview], project, applyHint)
			return sc.Err()
		case "rm":
			for _, k := range pos[2:] {
				if err := post(c, "DELETE", "/api/env?project="+url.QueryEscape(project)+"&key="+url.QueryEscape(k)+scope, nil, nil); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "removed; %s\n", applyHint)
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
			chosen := ""
			if *tokenStdin {
				b, err := io.ReadAll(stdin)
				if err != nil {
					return err
				}
				chosen = strings.TrimRight(string(b), "\r\n")
				body["token"] = chosen
			}
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
			if chosen != "" {
				fmt.Fprintf(out, "user %s saved with the token you gave\n", name)
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
		project, err := arg(0, "project")
		if err != nil {
			return err
		}
		o := deploy.RollbackOpts{Project: project, Images: *imagesOnly, Data: *dataOnly, Services: services, Snapshot: *snapshotID}
		if len(services) > 0 && *dataOnly {
			return errors.New("--service picks images; data always rolls back for the whole project (drop --data, or --service)")
		}
		if id, _ := arg(1, ""); id != "" {
			if o.Before, err = strconv.ParseInt(strings.TrimPrefix(id, "#"), 10, 64); err != nil {
				return fmt.Errorf("invalid deploy id %q (ids from vops history %s)", id, project)
			}
		}
		if *planOnly {
			q := url.Values{"project": {project}, "service": services, "before": {fmt.Sprint(o.Before)}, "snapshot": {fmt.Sprint(o.Snapshot)}}
			if o.Images {
				q.Set("images", "1")
			}
			if o.Data {
				q.Set("data", "1")
			}
			if !*asJSON {
				q.Set("format", "text")
			}
			resp, err := c.do("GET", "/api/rollback?"+q.Encode(), nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, err = io.Copy(out, resp.Body)
			return err
		}
		if !*yes {
			return errors.New("rollback on the host needs --yes (the cli shows what it does first)")
		}
		resp, err := c.do("POST", "/api/rollback", jsonBody(o))
		if err != nil {
			return err
		}
		return stream(resp, out)

	case "unpin":
		project, err := arg(0, "project")
		if err != nil {
			return err
		}
		resp, err := c.do("POST", "/api/unpin", jsonBody(map[string]any{"project": project, "services": pos[1:]}))
		if err != nil {
			return err
		}
		return stream(resp, out)

	case "preview":
		sub, err := arg(0, "ls|up|rm")
		if err != nil {
			return err
		}
		switch sub {
		case "ls":
			project, _ := arg(1, "")
			var res []deploy.PreviewState
			if err := getJSON(c, "/api/previews?project="+url.QueryEscape(project), &res); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PROJECT\tNAME\tCOMMIT\tRUNNING\tUPDATED\tEXPIRES\tURL\tIMAGES\tDATA")
			for _, p := range res {
				running, total, url := 0, 0, ""
				for _, s := range p.Services {
					for _, ct := range s.Containers {
						total++
						if ct.State == "running" || ct.Labels["vops.job"] != "" && ct.ExitCode == 0 {
							running++
						}
					}
					if len(s.Domains) > 0 && url == "" {
						url = s.Domains[len(s.Domains)-1]
					}
				}
				state := fmt.Sprintf("%d/%d", running, total)
				if p.Error != "" {
					state += " ✗ " + p.Error
				}
				fmt.Fprintf(tw, "%s\t%s\t%.12s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Project, p.Name, p.Commit, state, ago(p.UpdatedAt), in(p.ExpiresAt), orDash(url), orDash(strings.Join(sortedImages(p.Images), ",")), p.Data)
			}
			return tw.Flush()
		case "up":
			project, err := arg(1, "project")
			if err != nil {
				return err
			}
			if *name == "" {
				return errors.New("preview up needs --name (a dns label, e.g. pr-42)")
			}
			imgs := map[string]string{}
			for _, i := range images {
				svc, img, ok := strings.Cut(i, "=")
				if !ok || svc == "" || img == "" {
					return fmt.Errorf("--image %q: expected service=image", i)
				}
				imgs[svc] = img
			}
			resp, err := c.do("POST", "/api/previews", jsonBody(deploy.PreviewOpts{Project: project, Name: *name, Ref: *ref, Images: imgs, From: *from}))
			if err != nil {
				return err
			}
			return stream(resp, out)
		case "rm":
			project, err := arg(1, "project")
			if err != nil {
				return err
			}
			nm, err := arg(2, "name")
			if err != nil {
				return err
			}
			var res struct{ Log string }
			if err := post(c, "DELETE", "/api/previews?project="+url.QueryEscape(project)+"&name="+url.QueryEscape(nm), nil, &res); err != nil {
				return err
			}
			fmt.Fprint(out, res.Log)
			return nil
		}
		return fmt.Errorf("preview %s: unknown", sub)

	case "history":
		project, err := arg(0, "project")
		if err != nil {
			return err
		}
		var t deploy.Timeline
		if err := getJSON(c, "/api/timeline?project="+url.QueryEscape(project), &t); err != nil {
			return err
		}
		if *asJSON {
			b, _ := json.Marshal(t)
			_, err := fmt.Fprintln(out, string(b))
			return err
		}
		fmt.Fprintf(out, "now: %.12s %s\n", orDash(t.Commit), t.Subjects[t.Commit])
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tWHEN\tTRIGGER\tCOMMIT\tRESULT\tDATA BEFORE\tWHAT")
		shown := 0
		for _, nd := range t.Nodes {
			if shown == *n {
				break
			}
			shown++
			snap := "-"
			if nd.Snapshot != nil {
				snap = fmt.Sprintf("snapshot #%d", nd.Snapshot.ID)
			}
			if nd.Kind == "snapshot" {
				s := nd.Snapshot
				fmt.Fprintf(tw, "\t%s\t%s\t%.12s\t\t%s\t%s\n", ago(s.CreatedAt), s.Reason, s.Commit, snap, s.Note)
				continue
			}
			d := nd.Deploy
			var what []string
			for _, ch := range nd.Changes {
				what = append(what, ch.Text())
			}
			if len(what) == 0 {
				what = []string{d.Summary}
			}
			if d.Trigger == "rollback" {
				what = []string{"rolled back " + strings.ReplaceAll(cmp.Or(d.Parts, "data"), ",", " and ") + ": " + d.Summary}
			}
			if d.Error != "" {
				what = append(what, d.Error)
			}
			if len(nd.Previews) > 0 {
				what = append(what, "previews: "+strings.Join(nd.Previews, ","))
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%.12s\t%s\t%s\t%s\n", d.ID, ago(d.StartedAt), d.Trigger, d.Commit, d.Result, snap, strings.Join(what, "; "))
		}
		return tw.Flush()

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

// envHint is the short advice next to a variable in `env ls`.
func envHint(en compose.EnvEntry, project string, preview bool) string {
	flag := ""
	if preview {
		flag = "--preview "
	}
	switch {
	case en.Source == "missing":
		return "not set anywhere, empty: vops env set " + project + " " + flag + en.Key
	case en.Secret:
		return "committed to git: move to vops env (environment: [" + en.Key + "] + vops env set)"
	case en.File == "builtin":
		return "set by vops"
	case len(en.Vars) > 0:
		return "via ${" + strings.Join(en.Vars, "}, ${") + "}"
	}
	return ""
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

// in says when a future unix time comes.
func in(unix int64) string {
	d := time.Until(time.Unix(unix, 0)).Round(time.Minute)
	switch {
	case d <= 0:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	}
	return fmt.Sprintf("in %dd", int(d.Hours()/24))
}

func sortedImages(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
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
		Proxy struct {
			Up      bool
			Version int
			Routes  int
			InSync  bool `json:"in_sync"`
		}
	}
	if err := getJSON(c, "/api/status", &st); err != nil {
		return err
	}
	proxyNote := "down"
	if p := st.Proxy; p.Up {
		proxyNote = fmt.Sprintf("up (v%d, %d routes", p.Version, p.Routes)
		if !p.InSync {
			proxyNote += ", out of sync"
		}
		proxyNote += ")"
	}
	fmt.Fprintf(out, "commit %.12s  domain %s  proxy %s\n", st.Commit, orDash(st.Domain), proxyNote)
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
			if s.Pin != nil {
				info = strings.TrimSpace(fmt.Sprintf("%s  pinned to #%d, compose says %s", info, s.Pin.DeployID, s.Pin.ComposeImage))
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
