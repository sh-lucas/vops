package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sh-lucas/vops/internal/deploy"
	"github.com/sh-lucas/vops/internal/registry"
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

func readJSON(r *http.Request, v any) error {
	return json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), v, json.MatchCaseInsensitiveNames(true))
}

// API is the json api. trusted=true is the unix socket (ssh already authenticated the caller).
func (d *Daemon) API(trusted bool) http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				httpErr(w, http.StatusBadRequest, "%v", err)
			}
		})
	}

	h("GET /api/status", func(w http.ResponseWriter, r *http.Request) error {
		plan, err := d.Engine.Plan(r.Context())
		if err != nil {
			return err
		}
		flags, _ := d.DB.Projects()
		type project struct {
			Path      string                `json:"path"`
			Disabled  bool                  `json:"disabled"`
			Gone      bool                  `json:"gone"`
			Error     string                `json:"error,omitempty"`
			Commit    string                `json:"commit"`
			AppliedAt int64                 `json:"applied_at"`
			Services  []deploy.ServiceState `json:"services"`
		}
		out := struct {
			Domain   string    `json:"domain"`
			Commit   string    `json:"commit"`
			Changes  bool      `json:"changes"`
			Warnings []string  `json:"warnings"`
			Projects []project `json:"projects"`
		}{plan.Domain, plan.Commit, plan.Changes(), plan.Warnings, []project{}}
		for _, pp := range plan.Projects {
			f := flags[pp.Path]
			out.Projects = append(out.Projects, project{pp.Path, pp.Disabled, pp.Gone, pp.Error, f.Commit, f.AppliedAt, pp.Services()})
		}
		writeJSON(w, out)
		return nil
	})

	h("GET /api/plan", func(w http.ResponseWriter, r *http.Request) error {
		plan, err := d.Engine.Plan(r.Context())
		if err != nil {
			return err
		}
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
		}
		if r.ContentLength != 0 {
			if err := readJSON(r, &opts); err != nil {
				return err
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// a client that disconnects must not abort a rollout halfway
		ctx := context.WithoutCancel(r.Context())
		_, err := d.Engine.Apply(ctx, flushWriter{w}, deploy.ApplyOpts{Commit: opts.Commit, Projects: opts.Projects})
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
		return d.logs(r.Context(), w, q.Get("project"), q.Get("service"), logQuery{N: n, Follow: q.Get("follow") == "1", Grep: q.Get("grep"), Before: q.Get("before")})
	})

	h("GET /api/events", func(w http.ResponseWriter, r *http.Request) error {
		evs, err := d.DB.Events(r.URL.Query().Get("project"), 100)
		if err != nil {
			return err
		}
		writeJSON(w, evs)
		return nil
	})

	// ---- env (write-only values)

	h("GET /api/env", func(w http.ResponseWriter, r *http.Request) error {
		keys, err := d.DB.EnvKeys(r.URL.Query().Get("project"))
		if err != nil {
			return err
		}
		writeJSON(w, keys)
		return nil
	})
	h("POST /api/env", func(w http.ResponseWriter, r *http.Request) error {
		var in struct{ Project, Key, Value string }
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if in.Project == "" || !envKeyRe.MatchString(in.Key) {
			return fmt.Errorf("invalid project or key %q (keys match %s)", in.Key, envKeyRe)
		}
		if err := d.DB.SetEnv(in.Project, in.Key, in.Value); err != nil {
			return err
		}
		d.DB.Event(in.Project, "config", "env %s set", in.Key)
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	h("DELETE /api/env", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		if err := d.DB.UnsetEnv(q.Get("project"), q.Get("key")); err != nil {
			return err
		}
		d.DB.Event(q.Get("project"), "config", "env %s removed", q.Get("key"))
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})

	// ---- registry users

	h("GET /api/users", func(w http.ResponseWriter, r *http.Request) error {
		users, err := d.DB.Users()
		if err != nil {
			return err
		}
		writeJSON(w, users)
		return nil
	})
	h("POST /api/users", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Name     string   `json:"name"`
			Pattern  string   `json:"pattern"`
			Repos    []string `json:"repos"`
			NewToken bool     `json:"new_token"`
			Token    string   `json:"token"` // optional: keep an existing password (migrations); generated otherwise
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if !userRe.MatchString(in.Name) || in.Name == "admin" || in.Name == "vops-internal" {
			return fmt.Errorf("invalid user name %q (must match %s, not admin)", in.Name, userRe)
		}
		if in.Pattern != "" {
			if _, err := regexp.Compile(in.Pattern); err != nil {
				return fmt.Errorf("pattern: %w", err)
			}
		}
		var repos []string
		for _, rp := range in.Repos {
			if rp = strings.TrimSpace(rp); rp == "" {
				continue
			}
			if !registry.ValidName(rp) {
				return fmt.Errorf("invalid repository name %q", rp)
			}
			repos = append(repos, rp)
		}
		exists := slices.ContainsFunc(must(d.DB.Users()), func(u store.User) bool { return u.Name == in.Name })
		token := ""
		if !exists || in.NewToken {
			token = store.Token()
		}
		if in.Token != "" {
			if len(in.Token) < 12 {
				return errors.New("a chosen token needs at least 12 characters")
			}
			token = in.Token
		}
		if err := d.DB.PutUser(store.User{Name: in.Name, Pattern: in.Pattern, Repos: repos}, token); err != nil {
			return err
		}
		d.forgetAuth()
		d.DB.Event("", "config", "registry user %s saved", in.Name)
		writeJSON(w, map[string]string{"name": in.Name, "token": token})
		return nil
	})
	h("DELETE /api/users", func(w http.ResponseWriter, r *http.Request) error {
		name := r.URL.Query().Get("name")
		if err := d.DB.DeleteUser(name); err != nil {
			return err
		}
		d.forgetAuth()
		d.DB.Event("", "config", "registry user %s deleted", name)
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
		res, err := d.Reg.GC(time.Hour)
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
	// rollback streams progress like apply; id 0 means the latest snapshot that isn't a pre-rollback one
	h("POST /api/rollback", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Project string `json:"project"`
			ID      int64  `json:"id"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if in.ID == 0 {
			s, err := d.Engine.LatestSnapshot(in.Project)
			if err != nil {
				return err
			}
			in.ID = s.ID
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := d.Engine.Rollback(context.WithoutCancel(r.Context()), flushWriter{w}, in.Project, in.ID); err != nil {
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

	h("GET /api/routes", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, d.Routes.Routes())
		return nil
	})

	if trusted {
		h("POST /api/admin/password", func(w http.ResponseWriter, r *http.Request) error {
			var in struct{ Password string }
			if err := readJSON(r, &in); err != nil {
				return err
			}
			if len(in.Password) < 12 {
				return errors.New("password must have at least 12 characters")
			}
			if err := d.DB.SetAdminPassword(in.Password); err != nil {
				return err
			}
			d.forgetAuth()
			writeJSON(w, map[string]bool{"ok": true})
			return nil
		})
		h("GET /api/admin", func(w http.ResponseWriter, r *http.Request) error {
			writeJSON(w, map[string]bool{"has_password": d.DB.HasAdmin()})
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

var loginMu sync.Mutex

func (d *Daemon) sessionAuth(api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get("X-Vops") != "1" {
			httpErr(w, http.StatusForbidden, "missing X-Vops header")
			return
		}
		switch r.URL.Path {
		case "/api/login":
			var in struct{ Password string }
			if err := readJSON(r, &in); err != nil {
				httpErr(w, 400, "%v", err)
				return
			}
			loginMu.Lock() // one attempt at a time, and a failed one costs a second
			ok := d.DB.HasAdmin() && d.DB.CheckAdmin(in.Password)
			if !ok {
				time.Sleep(time.Second)
			}
			loginMu.Unlock()
			if !ok {
				d.DB.Event("", "auth", "failed dashboard login from %s", r.RemoteAddr)
				httpErr(w, http.StatusUnauthorized, "wrong password")
				return
			}
			id, err := d.DB.NewSession(7 * 24 * time.Hour)
			if err != nil {
				httpErr(w, 500, "%v", err)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 7 * 24 * 3600})
			writeJSON(w, map[string]bool{"ok": true})
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
		if err != nil || !d.DB.CheckSession(c.Value) {
			httpErr(w, http.StatusUnauthorized, "login required")
			return
		}
		api.ServeHTTP(w, r)
	})
}
