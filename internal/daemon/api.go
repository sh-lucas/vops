package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/registry"
	"github.com/sh-lucas/vops/internal/secrets"
	"github.com/sh-lucas/vops/internal/store"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.MarshalWrite(w, v)
}

func httpErr(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.MarshalWrite(w, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// maxJSON bounds every json request body (a backup upload is the only unbounded body, and it isn't json).
const maxJSON = 1 << 20

var errTooLarge = fmt.Errorf("request body too large (at most %d bytes of json)", maxJSON)

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxJSON+1))
	if err != nil {
		return err
	}
	if len(b) > maxJSON {
		return errTooLarge
	}
	if err := json.Unmarshal(b, v, json.MatchCaseInsensitiveNames(true)); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return nil
}

// API is the json api. trusted=true is the unix socket (ssh already authenticated the caller).
func (d *Daemon) API(trusted bool) http.Handler {
	mux := http.NewServeMux()
	if !trusted {
		d.webRoutes = nil
	}
	h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request) error) {
		if !trusted {
			d.webRoutes = append(d.webRoutes, pattern)
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, errTooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				httpErr(w, status, "%v", err)
			}
		})
	}

	h("GET /api/status", func(w http.ResponseWriter, r *http.Request) error {
		plan, err := d.Engine.Plan(r.Context())
		if err != nil {
			return err
		}
		flags, _ := d.DB.Projects()
		last, _ := d.DB.LatestDeploys()
		type project struct {
			Path       string                `json:"path"`
			Disabled   bool                  `json:"disabled"`
			Gone       bool                  `json:"gone"`
			Error      string                `json:"error,omitempty"`
			Commit     string                `json:"commit"`
			AppliedAt  int64                 `json:"applied_at"`
			Services   []deploy.ServiceState `json:"services"`
			LastDeploy *store.Deploy         `json:"last_deploy"`       // a rollback here: rolled back since the last deploy (the banner)
			Limits     string                `json:"limits,omitempty"`  // proxy limits in effect ("rate 20/s burst 20, max_body 10MB")
			Refused    *proxy.Counters       `json:"refused,omitempty"` // what they refused since the proxy started
		}
		type preview struct {
			Project string `json:"project"`
			Name    string `json:"name"`
		}
		px, pwarns := d.proxyState(r.Context())
		out := struct {
			Domain   string     `json:"domain"`
			Commit   string     `json:"commit"`
			Changes  bool       `json:"changes"`
			Warnings []string   `json:"warnings"`
			Projects []project  `json:"projects"`
			Previews []preview  `json:"previews"`
			Proxy    ProxyState `json:"proxy"`
			User     string     `json:"user"` // who is logged in ("cli" on the socket)
		}{plan.Domain, plan.Commit, plan.Changes(), slices.Concat(pwarns, plan.Warnings, d.backupWarnings()), []project{}, []preview{}, px, actor(r)}
		for _, pp := range plan.Projects {
			f := flags[pp.Path]
			var ld *store.Deploy
			if x, ok := last[pp.Path]; ok {
				ld = &x
			}
			var lim string
			if f.Limits != "" {
				lim = deploy.LimitsOf(f.Limits).String()
			}
			var refused *proxy.Counters
			if c, ok := px.Limited[pp.Path]; ok {
				refused = &c
			}
			out.Projects = append(out.Projects, project{pp.Path, pp.Disabled, pp.Gone, pp.Error, f.Commit, f.AppliedAt, pp.Services(), ld, lim, refused})
		}
		pvs, _ := d.DB.Previews("")
		for _, pv := range pvs {
			out.Previews = append(out.Previews, preview{pv.Project, pv.Name})
		}
		writeJSON(w, out)
		return nil
	})

	h("GET /api/ping", func(w http.ResponseWriter, r *http.Request) error {
		_, err := io.WriteString(w, "ok\n")
		return err
	})

	h("GET /api/plan", func(w http.ResponseWriter, r *http.Request) error {
		plan, err := d.Engine.Plan(r.Context())
		if err != nil {
			return err
		}
		plan.Version = d.Version
		if r.URL.Query().Get("format") == "text" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			plan.Print(w)
			return nil
		}
		writeJSON(w, plan)
		return nil
	})

	// apply streams progress as text; the last line is "==> ok" or "==> error: ...".
	h("POST /api/apply", func(w http.ResponseWriter, r *http.Request) error {
		var opts struct {
			Commit   string   `json:"commit"`
			Projects []string `json:"projects"`
			Trigger  string   `json:"trigger"` // sync | apply | ui, for the history
		}
		if r.ContentLength != 0 {
			if err := readJSON(r, &opts); err != nil {
				return err
			}
		}
		if !slices.Contains([]string{"sync", "apply", "ui"}, opts.Trigger) {
			opts.Trigger = "apply"
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// a client that disconnects must not abort a rollout halfway
		ctx := context.WithoutCancel(r.Context())
		_, err := d.Engine.Apply(ctx, flushWriter{w}, deploy.ApplyOpts{Commit: opts.Commit, Projects: opts.Projects, Trigger: opts.Trigger})
		if err != nil {
			fmt.Fprintf(w, "==> error: %v\n", err)
		} else {
			fmt.Fprintln(w, "==> ok")
		}
		return nil
	})

	h("POST /api/project", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Project  string `json:"project"`
			Disabled bool   `json:"disabled"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if in.Project == "" {
			return errors.New("project is required")
		}
		if err := d.DB.SetDisabled(in.Project, in.Disabled); err != nil {
			return err
		}
		d.DB.Event(in.Project, "config", "project %s", map[bool]string{true: "disabled", false: "enabled"}[in.Disabled])
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})

	h("POST /api/restart", func(w http.ResponseWriter, r *http.Request) error {
		var in struct{ Project, Service string }
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if err := d.Engine.Restart(r.Context(), in.Project, in.Service); err != nil {
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})

	h("GET /api/logs", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		n, _ := strconv.Atoi(q.Get("n"))
		var since, until int64
		if s := q.Get("since"); s != "" {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid since %q: must be unix seconds", s)
			}
			since = v
		}
		if s := q.Get("until"); s != "" {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid until %q: must be unix seconds", s)
			}
			until = v
		}
		return d.logs(r.Context(), w, q.Get("project"), q.Get("service"), logQuery{N: n, Follow: q.Get("follow") == "1", Grep: q.Get("grep"), Before: q.Get("before"), Since: since, Until: until})
	})

	// the project's history: deploys, rollbacks, manual snapshots, previews, newest first
	h("GET /api/timeline", func(w http.ResponseWriter, r *http.Request) error {
		project := r.URL.Query().Get("project")
		if project == "" || strings.Contains(project, "@") {
			return fmt.Errorf("invalid project %q", project)
		}
		t, err := d.Engine.Timeline(r.Context(), project)
		if err != nil {
			return err
		}
		writeJSON(w, t)
		return nil
	})

	h("GET /api/events", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		n, _ := strconv.Atoi(q.Get("n"))
		if n <= 0 || n > 500 {
			n = 50
		}
		before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
		evs, err := d.DB.EventsBefore(q.Get("project"), before, n)
		if err != nil {
			return err
		}
		writeJSON(w, evs)
		return nil
	})

	// ---- env (write-only values). preview=1 (or "preview": true) is the project's preview secrets scope.

	h("GET /api/env", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		scope, err := envScope(q.Get("project"), q.Get("preview") == "1")
		if err != nil {
			return err
		}
		keys, err := d.DB.EnvKeys(scope)
		if err != nil {
			return err
		}
		writeJSON(w, keys)
		return nil
	})
	// where every variable of every service comes from (never values); preview=1: the previews' env
	h("GET /api/env/usage", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		if _, err := envScope(q.Get("project"), false); err != nil {
			return err
		}
		u, err := d.Engine.EnvUsage(r.Context(), q.Get("project"), q.Get("preview") == "1")
		if err != nil {
			return err
		}
		writeJSON(w, u)
		return nil
	})
	h("POST /api/env", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Project, Key, Value string
			Preview             bool
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		scope, err := envScope(in.Project, in.Preview)
		if err != nil {
			return err
		}
		if !envKeyRe.MatchString(in.Key) {
			return fmt.Errorf("invalid key %q (keys match %s)", in.Key, envKeyRe)
		}
		if err := d.DB.SetEnv(scope, in.Key, in.Value); err != nil {
			return err
		}
		d.DB.Event(in.Project, "config", "%s %s set", envLabel(in.Preview), in.Key)
		d.backupNow()
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	h("DELETE /api/env", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		scope, err := envScope(q.Get("project"), q.Get("preview") == "1")
		if err != nil {
			return err
		}
		if err := d.DB.UnsetEnv(scope, q.Get("key")); err != nil {
			return err
		}
		d.DB.Event(q.Get("project"), "config", "%s %s removed", envLabel(q.Get("preview") == "1"), q.Get("key"))
		d.backupNow()
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})

	// the encrypted secrets backup: who can open it, whether git has it. blob=1 adds the armored file
	// (it is encrypted: it goes to git anyway); have=<sha> asks whether a copy is one this host wrote.
	h("GET /api/secrets", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		info, err := d.backupInfo(q.Get("have"), q.Get("blob") == "1")
		if err != nil && info.File == "" {
			return err
		}
		writeJSON(w, info)
		return nil
	})

	// ---- previews

	h("GET /api/previews", func(w http.ResponseWriter, r *http.Request) error {
		out, err := d.Engine.Previews(r.Context(), r.URL.Query().Get("project"))
		if err != nil {
			return err
		}
		writeJSON(w, out)
		return nil
	})
	// up streams progress like apply
	h("POST /api/previews", func(w http.ResponseWriter, r *http.Request) error {
		var in deploy.PreviewOpts
		if err := readJSON(r, &in); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := d.Engine.PreviewUp(context.WithoutCancel(r.Context()), flushWriter{w}, in); err != nil {
			fmt.Fprintf(w, "==> error: %v\n", err)
		} else {
			fmt.Fprintln(w, "==> ok")
		}
		return nil
	})
	h("DELETE /api/previews", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		var log strings.Builder
		if err := d.Engine.PreviewRm(context.WithoutCancel(r.Context()), &log, q.Get("project"), q.Get("name")); err != nil {
			return err
		}
		writeJSON(w, map[string]any{"ok": true, "log": log.String()})
		return nil
	})

	// ---- users: admins (dashboard + every repo), deployers (registry: every repo or a list)

	h("GET /api/users", func(w http.ResponseWriter, r *http.Request) error {
		users, err := d.DB.Users()
		if err != nil {
			return err
		}
		writeJSON(w, users)
		return nil
	})
	// POST /api/users creates a user or changes what is given (role, global, repos, secret); omitted fields stay.
	// Deployers get a generated token (on creation, new_token, or a demotion), or a chosen one; admins a password.
	h("POST /api/users", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Name     string    `json:"name"`
			Role     *string   `json:"role"`
			Global   *bool     `json:"global"`
			Repos    *[]string `json:"repos"`
			NewToken bool      `json:"new_token"`
			Token    string    `json:"token"`    // a deployer's chosen token (migrations); generated otherwise
			Password string    `json:"password"` // an admin's password
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if !userRe.MatchString(in.Name) || in.Name == "vops-internal" {
			return fmt.Errorf("invalid user name %q (must match %s, not vops-internal)", in.Name, userRe)
		}
		old, found, err := d.DB.User(in.Name)
		if err != nil {
			return err
		}
		u := old
		if !found {
			u = store.User{Name: in.Name, Role: store.Deployer}
		}
		if in.Role != nil {
			if *in.Role != store.Admin && *in.Role != store.Deployer {
				return fmt.Errorf("role must be admin or deployer, got %q", *in.Role)
			}
			u.Role = *in.Role
		}
		if in.Global != nil {
			u.Global = *in.Global
		}
		if in.Repos != nil {
			u.Repos = store.NormalizeRepos(*in.Repos)
			for _, rp := range u.Repos {
				if !registry.ValidName(rp) {
					return fmt.Errorf("invalid repository name %q", rp)
				}
			}
		}
		secret, token := "", ""
		if u.Role == store.Admin {
			if in.Token != "" || in.NewToken {
				return errors.New("admins log in with a password, not a token")
			}
			if in.Password != "" && len(in.Password) < 12 {
				return errors.New("password must have at least 12 characters")
			}
			if in.Password == "" && (!found || old.Role != store.Admin) {
				return errors.New("an admin needs a password")
			}
			secret = in.Password
		} else {
			if in.Password != "" {
				return errors.New("deployers use a generated token, not a password")
			}
			if !u.Global && len(u.Repos) == 0 {
				return errors.New("a deployer needs global or at least one repo (what may it push and pull?)")
			}
			switch {
			case in.Token != "":
				if len(in.Token) < 12 {
					return errors.New("a chosen token needs at least 12 characters")
				}
				secret, token = in.Token, in.Token
			case !found || in.NewToken || old.Role == store.Admin:
				secret = store.Token()
				token = secret
			}
		}
		if err := d.DB.PutUser(u, secret); err != nil {
			return err
		}
		d.forgetAuth()
		what := "saved"
		if secret != "" && found {
			what = map[bool]string{true: "saved (new password)", false: "saved (new token)"}[u.Role == store.Admin]
		}
		d.DB.Event("", "auth", "user %s (%s) %s by %s", in.Name, u.Role, what, actor(r))
		if in.Token != "" {
			token = "" // the caller knows it
		}
		writeJSON(w, map[string]string{"name": in.Name, "role": u.Role, "token": token})
		return nil
	})
	h("DELETE /api/users", func(w http.ResponseWriter, r *http.Request) error {
		name := r.URL.Query().Get("name")
		if err := d.DB.DeleteUser(name); err != nil {
			return err
		}
		d.forgetAuth()
		d.DB.Event("", "auth", "user %s deleted by %s", name, actor(r))
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})

	// ---- registry content

	h("GET /api/registry", func(w http.ResponseWriter, r *http.Request) error {
		type repo struct {
			Name string         `json:"name"`
			Tags []registry.Tag `json:"tags"`
		}
		out := []repo{}
		for _, name := range d.Reg.Repos() {
			out = append(out, repo{name, d.Reg.Tags(name)})
		}
		writeJSON(w, map[string]any{"host": d.registryHost(), "repos": out})
		return nil
	})
	h("DELETE /api/registry", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		if err := d.Reg.DeleteTag(q.Get("repo"), q.Get("tag")); err != nil {
			return err
		}
		d.DB.Event("", "registry", "deleted %s:%s", q.Get("repo"), q.Get("tag"))
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	h("POST /api/registry/gc", func(w http.ResponseWriter, r *http.Request) error {
		res, err := d.gc()
		if err != nil {
			return err
		}
		d.DB.Event("", "gc", "registry gc: %d manifests, %d blobs, %d bytes freed", res.Manifests, res.Blobs, res.Freed)
		writeJSON(w, res)
		return nil
	})

	// ---- repo tree (git-tracked only)

	h("GET /api/tree", func(w http.ResponseWriter, r *http.Request) error {
		files, err := d.tree(r.Context())
		if err != nil {
			return err
		}
		writeJSON(w, files)
		return nil
	})
	h("GET /api/file", func(w http.ResponseWriter, r *http.Request) error {
		content, err := d.file(r.Context(), r.URL.Query().Get("path"))
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(content)
		return nil
	})

	// ---- snapshots and rollback

	h("GET /api/snapshots", func(w http.ResponseWriter, r *http.Request) error {
		project := r.URL.Query().Get("project")
		snaps, err := d.DB.Snapshots(project)
		if err != nil {
			return err
		}
		out := map[string]any{"snapshots": snaps}
		if project != "" {
			out["data"] = d.Engine.DataState(r.Context(), project)
		}
		writeJSON(w, out)
		return nil
	})
	h("POST /api/snapshots", func(w http.ResponseWriter, r *http.Request) error {
		var in struct{ Project, Note string }
		if err := readJSON(r, &in); err != nil {
			return err
		}
		var log strings.Builder
		s, err := d.Engine.Snapshot(context.WithoutCancel(r.Context()), &log, in.Project, in.Note)
		if err != nil {
			return err
		}
		writeJSON(w, map[string]any{"snapshot": s, "log": log.String()})
		return nil
	})
	h("DELETE /api/snapshots", func(w http.ResponseWriter, r *http.Request) error {
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid id")
		}
		if err := d.Engine.DeleteSnapshot(r.Context(), id); err != nil {
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	// backups: a snapshot as a .tar.gz (streamed both ways; the upload is the one body without a size limit)
	h("GET /api/snapshots/export", func(w http.ResponseWriter, r *http.Request) error {
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid id")
		}
		s, err := d.Engine.BackupCheck(id)
		if err != nil {
			return err
		}
		if p := r.URL.Query().Get("project"); p != "" && p != s.Project {
			return fmt.Errorf("snapshot #%d is of %s, not %s", id, s.Project, p)
		}
		name := fmt.Sprintf("%s-snapshot-%d-%s.tar.gz", compose.Slug(s.Project), s.ID, time.Unix(s.CreatedAt, 0).UTC().Format("20060102-1504"))
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		if err := d.Engine.ExportSnapshot(r.Context(), w, id, d.Version); err != nil {
			log.Printf("export snapshot #%d: %v", id, err)
			panic(http.ErrAbortHandler) // the status is sent: cut the stream so the download fails instead of looking complete
		}
		return nil
	})
	h("POST /api/snapshots/import", func(w http.ResponseWriter, r *http.Request) error {
		project := r.URL.Query().Get("project")
		var log strings.Builder
		s, err := d.Engine.ImportSnapshot(r.Context(), &log, project, r.Body)
		if err != nil {
			return err
		}
		writeJSON(w, map[string]any{"snapshot": s, "log": log.String()})
		return nil
	})
	// rollback: GET shows what it would do (format=text for the cli), POST does it and streams progress like apply
	rollbackOpts := func(q map[string][]string) (deploy.RollbackOpts, error) {
		get := func(k string) string {
			if len(q[k]) > 0 {
				return q[k][0]
			}
			return ""
		}
		o := deploy.RollbackOpts{Project: get("project"), Images: get("images") == "1", Data: get("data") == "1", Services: q["service"]}
		for k, dst := range map[string]*int64{"before": &o.Before, "snapshot": &o.Snapshot} {
			if v := get(k); v != "" {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					return o, fmt.Errorf("invalid %s %q", k, v)
				}
				*dst = n
			}
		}
		return o, nil
	}
	h("GET /api/rollback", func(w http.ResponseWriter, r *http.Request) error {
		o, err := rollbackOpts(r.URL.Query())
		if err != nil {
			return err
		}
		rp, err := d.Engine.RollbackPlan(r.Context(), o)
		if err != nil {
			return err
		}
		if r.URL.Query().Get("format") == "text" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rp.Print(w)
			return nil
		}
		writeJSON(w, rp)
		return nil
	})
	h("POST /api/rollback", func(w http.ResponseWriter, r *http.Request) error {
		var in deploy.RollbackOpts
		if err := readJSON(r, &in); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := d.Engine.Rollback(context.WithoutCancel(r.Context()), flushWriter{w}, in); err != nil {
			fmt.Fprintf(w, "==> error: %v\n", err)
		} else {
			fmt.Fprintln(w, "==> ok")
		}
		return nil
	})
	// unpin: back to what compose says for these services (all pinned ones if none), streamed like apply
	h("POST /api/unpin", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Project  string   `json:"project"`
			Services []string `json:"services"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := d.Engine.Unpin(context.WithoutCancel(r.Context()), flushWriter{w}, in.Project, in.Services); err != nil {
			fmt.Fprintf(w, "==> error: %v\n", err)
		} else {
			fmt.Fprintln(w, "==> ok")
		}
		return nil
	})

	h("GET /api/audit", func(w http.ResponseWriter, r *http.Request) error {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 || n > 5000 {
			n = 200
		}
		logs, err := d.DB.Audit(n)
		if err != nil {
			return err
		}
		writeJSON(w, logs)
		return nil
	})

	h("GET /api/system", func(w http.ResponseWriter, r *http.Request) error {
		if d.sys == nil {
			return errors.New("no system monitor")
		}
		writeJSON(w, d.sys.Read())
		return nil
	})

	h("GET /api/routes", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, d.Routes.Routes())
		return nil
	})

	if trusted {
		// `vops env restore`: values decrypted on the laptop, sent over ssh; only missing keys are set
		h("POST /api/secrets/restore", func(w http.ResponseWriter, r *http.Request) error {
			var in struct{ Entries []secrets.Entry }
			if err := readJSON(r, &in); err != nil {
				return err
			}
			restored, kept, err := d.restoreEnv(in.Entries)
			if restored == nil {
				restored = []string{}
			}
			if kept == nil {
				kept = []string{}
			}
			if err != nil && len(restored) == 0 {
				return err
			}
			out := map[string]any{"restored": restored, "kept": kept}
			if err != nil {
				out["error"] = err.Error()
			}
			writeJSON(w, out)
			return nil
		})

		// `vops setup` after starting the proxy: rebuild the table from podman and send it now
		h("POST /api/proxy/sync", func(w http.ResponseWriter, r *http.Request) error {
			var err error
			d.Engine.Lock(func() { err = d.Engine.RefreshRoutes(r.Context()) })
			if err != nil {
				return err
			}
			if d.proxyDirty.Load() {
				return errors.New("the proxy did not take the routes")
			}
			writeJSON(w, map[string]int{"routes": len(d.Routes.Routes())})
			return nil
		})

	}
	return mux
}

var (
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	userRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

func must[T any](v T, _ error) T { return v }

// envScope is where env vars of a project live: the project, or its previews' secrets.
func envScope(project string, preview bool) (string, error) {
	if project == "" || strings.ContainsAny(project, "@*") {
		return "", fmt.Errorf("invalid project %q (for previews use the project and preview=true)", project)
	}
	if preview {
		return deploy.PreviewEnvScope(project), nil
	}
	return project, nil
}

func envLabel(preview bool) string {
	if preview {
		return "preview env"
	}
	return "env"
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// ---- sessions

const sessionCookie = "vops_session"

var (
	loginMu    sync.Mutex
	loginDelay = time.Second // what a failed login costs (tests shorten it)
)

type userKey struct{}

// actor is who made a request: the logged-in admin, or "cli" on the socket (ssh authenticated it).
func actor(r *http.Request) string {
	if u, ok := r.Context().Value(userKey{}).(string); ok {
		return u
	}
	return "cli"
}

func (d *Daemon) sessionAuth(api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get("X-Vops") != "1" {
			httpErr(w, http.StatusForbidden, "missing X-Vops header")
			return
		}
		switch r.URL.Path {
		case "/api/login":
			var in struct{ User, Password string }
			if err := readJSON(r, &in); err != nil {
				httpErr(w, 400, "%v", err)
				return
			}
			if in.User == "" {
				in.User = "admin"
			}
			// only admins log in; a deployer's right token fails exactly like a wrong password
			loginMu.Lock() // one attempt at a time, and a failed one costs a second
			u, ok := d.DB.CheckUser(in.User, in.Password)
			ok = ok && u.Role == store.Admin
			if !ok {
				time.Sleep(loginDelay)
			}
			loginMu.Unlock()
			if !ok {
				d.DB.Event("", "auth", "failed dashboard login as %q from %s", in.User, r.RemoteAddr)
				httpErr(w, http.StatusUnauthorized, "wrong user or password")
				return
			}
			id, err := d.DB.NewSession(u.Name, 7*24*time.Hour)
			if err != nil {
				httpErr(w, 500, "%v", err)
				return
			}
			d.DB.Event("", "auth", "dashboard login: %s from %s", u.Name, r.RemoteAddr)
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 7 * 24 * 3600})
			writeJSON(w, map[string]any{"ok": true, "user": u.Name})
			return
		case "/api/logout":
			if c, err := r.Cookie(sessionCookie); err == nil {
				d.DB.DeleteSession(c.Value)
			}
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			httpErr(w, http.StatusUnauthorized, "login required")
			return
		}
		user, ok := d.DB.CheckSession(c.Value)
		if !ok {
			httpErr(w, http.StatusUnauthorized, "login required")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
		api.ServeHTTP(w, r)
	})
}
