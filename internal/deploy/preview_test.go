package deploy

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/snapshot"
	"github.com/sh-lucas/vops/internal/store"
)

// A preview of a project with a real postgres: its data is a copy of production's, a destructive migration
// in it leaves production untouched, its env comes only from preview.env and the preview secrets, it is
// served on its own domain, off the shared network, with only what web declares (x-vops.preview): db's data
// is copied, web's own volume starts empty, the undeclared worker doesn't run; preview_max holds; rm and the
// ttl leave nothing behind; --from starts a preview from a snapshot.
func TestPreviewWithPostgres(t *testing.T) {
	v := newEnv(t)
	if !snapshot.Supported(v.e.SnapshotDir) {
		t.Skip("not on btrfs")
	}
	ctx := context.Background()
	const pg = "docker.io/library/postgres:16-alpine"
	if podman.ImageID(ctx, pg) == "" {
		if _, err := podman.Run(ctx, "pull", "-q", pg); err != nil {
			t.Skipf("can't pull %s: %v", pg, err)
		}
	}
	p := v.ns + "/shop"
	domain := v.ns + ".test"
	prodHost, previewHost := "web.shop."+v.ns+"."+domain, "web.pr-1.shop."+v.ns+"."+domain
	v.e.DB.SetEnv(p, "MSG", "prod")
	v.e.DB.SetEnv(p, "SECRET", "prod-secret")
	v.e.DB.SetEnv(p, "MIGRATION", "CREATE TABLE users (name text); INSERT INTO users VALUES ('alice'), ('bob')")
	v.e.DB.SetEnv(PreviewEnvScope(p), "MSG", "preview-secret")
	v.commit(map[string]string{
		"vops.yml": "domain: " + domain + "\n",
		p + "/compose.yml": `services:
  db:
    image: ` + pg + `
    environment: {POSTGRES_HOST_AUTH_METHOD: trust}
    volumes: ["pg:/var/lib/postgresql/data"]
    healthcheck: {test: ["CMD", "pg_isready", "-h", "127.0.0.1", "-U", "postgres"]}
  migrate:
    image: ` + pg + `
    command: ["psql", "-h", "db", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", "${MIGRATION}"]
    depends_on: {db: {condition: service_healthy}}
  web:
    image: APP
    environment: [MSG, SECRET, SMTP_HOST]
    depends_on: {migrate: {condition: service_completed_successfully}}
    volumes: ["files:/files"]
    x-vops: {port: 8080, preview: {copy: [db], with: [migrate]}}
  worker:
    image: APP
    environment: {PORT: "9000"}
    volumes: ["jobs:/jobs"]
volumes:
  pg:
  files:
  jobs:
`,
		p + "/preview.env": "MSG=from-file\nSMTP_HOST=mailpit\nMIGRATION=DELETE FROM users WHERE name = 'bob'\n",
	})
	v.mustApply()
	psql := func(project, sql string) string {
		t.Helper()
		cs, _ := podman.PS(ctx, LProject+"="+project, LService+"=db")
		if len(cs) == 0 {
			t.Fatalf("no db in %s", project)
		}
		out, err := podman.Run(ctx, "exec", cs[0].ID, "psql", "-U", "postgres", "-tAc", sql)
		if err != nil {
			t.Fatalf("psql in %s: %v", project, err)
		}
		return out
	}
	if n := psql(p, "SELECT count(*) FROM users"); n != "2" {
		t.Fatalf("prod users: %s", n)
	}
	if code, body := v.get(prodHost); code != 200 || body != "prod" {
		t.Fatalf("prod: %d %q", code, body)
	}
	ids := func(project string) []string {
		var out []string
		for _, c := range v.containers(project) {
			out = append(out, c.ID)
		}
		slices.Sort(out)
		return out
	}
	prodIDs := ids(p)
	inWeb := func(project string, args ...string) (string, error) {
		t.Helper()
		cs, _ := podman.PS(ctx, LProject+"="+project, LService+"=web")
		if len(cs) == 0 {
			t.Fatalf("no web in %s", project)
		}
		return podman.Run(ctx, append([]string{"exec", cs[0].ID, "/app"}, args...)...)
	}
	if _, err := inWeb(p, "write", "/files/f", "prod-file"); err != nil {
		t.Fatal(err)
	}

	// up: data copied, destructive migration runs in the preview only
	volumesBefore, _ := podman.Run(ctx, "volume", "ls", "-q")
	path := PreviewPath(p, "pr-1")
	var buf strings.Builder
	if err := v.e.PreviewUp(ctx, &buf, PreviewOpts{Project: p, Name: "pr-1"}); err != nil {
		t.Fatalf("preview up: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "copy of production data") || !strings.Contains(buf.String(), "serving "+previewHost) {
		t.Fatalf("preview up output:\n%s", buf.String())
	}
	if n := psql(path, "SELECT string_agg(name, ',') FROM users"); n != "alice" {
		t.Fatalf("preview should have production's rows minus its migration: %q", n)
	}
	if n := psql(p, "SELECT string_agg(name, ',' ORDER BY name) FROM users"); n != "alice,bob" {
		t.Fatalf("production data changed by the preview: %q", n)
	}
	if !slices.Equal(ids(p), prodIDs) {
		t.Fatal("production containers were touched")
	}
	if plan, _ := v.e.Plan(ctx); plan.Changes() {
		t.Fatalf("a preview must not show up in the plan: %s", actions(plan))
	}
	// served on its own domain, env = preview.env + preview secrets, never production's env
	if code, body := v.get(previewHost); code != 200 || body != "preview-secret" {
		t.Fatalf("preview: %d %q", code, body)
	}
	if code, body := v.get(prodHost); code != 200 || body != "prod" {
		t.Fatalf("prod after preview: %d %q", code, body)
	}
	var web, services []string
	for _, c := range v.containers(path) {
		services = append(services, c.Labels[LService])
		if c.Labels[LService] == "web" {
			web = append(web, c.ID)
		}
	}
	slices.Sort(services)
	if strings.Join(services, ",") != "db,migrate,web" {
		t.Fatalf("preview services (worker declares nothing): %v", services)
	}
	// only db (copy) has production's data: web's volume starts empty, the worker's isn't created
	if out, err := inWeb(path, "read", "/files/f"); err == nil {
		t.Fatalf("web's volume was copied: %q", out)
	}
	if vols, _ := podman.Run(ctx, "volume", "ls", "-q", "--filter", "label="+LProject+"="+path); strings.Contains(vols, "-jobs") || !strings.Contains(vols, "-pg") {
		t.Fatalf("preview volumes: %s", vols)
	}
	envs, _ := podman.Run(ctx, "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", web[0])
	if !strings.Contains(envs, "MSG=preview-secret\n") || !strings.Contains(envs, "SMTP_HOST=mailpit\n") || strings.Contains(envs, "SECRET=") {
		t.Fatalf("preview env:\n%s", envs)
	}
	// off the shared network: production's db is out of reach
	if nets, _ := podman.Run(ctx, "inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", web[0]); slices.Contains(strings.Fields(nets), "vops") {
		t.Fatalf("preview joined the shared network: %s", nets)
	}
	if out, err := podman.Run(ctx, "exec", web[0], "/app", "lookup", "db.shop."+v.ns); err == nil {
		t.Fatalf("preview reaches production's db: %s", out)
	}

	// update: a new secret redeploys web; the data stays (the migration isn't rerun)
	v.e.DB.SetEnv(PreviewEnvScope(p), "MSG", "preview-2")
	if err := v.e.PreviewUp(ctx, io.Discard, PreviewOpts{Project: p, Name: "pr-1"}); err != nil {
		t.Fatal(err)
	}
	if _, body := v.get(previewHost); body != "preview-2" {
		t.Fatalf("preview after update: %q", body)
	}
	if n := psql(path, "SELECT count(*) FROM users"); n != "1" {
		t.Fatalf("update lost the preview's data: %s", n)
	}
	if st, _ := v.e.Previews(ctx, p); len(st) != 1 || st[0].Path != path || len(st[0].Services) != 3 || st[0].Error != "" {
		t.Fatalf("previews: %+v", st)
	}

	// preview_max
	v.e.PreviewMax = 1
	if err := v.e.PreviewUp(ctx, io.Discard, PreviewOpts{Project: p, Name: "pr-2"}); err == nil || !strings.Contains(err.Error(), "preview_max") {
		t.Fatalf("preview_max not enforced: %v", err)
	}
	v.e.PreviewMax = 0

	// rm leaves nothing behind
	gone := func(path string) {
		t.Helper()
		label := "label=" + LProject + "=" + path
		for _, cmd := range [][]string{{"ps", "-aq", "--filter", label}, {"network", "ls", "-q", "--filter", label}, {"volume", "ls", "-q", "--filter", label}} {
			if out, _ := podman.Run(ctx, cmd...); out != "" {
				t.Fatalf("%s %s left behind: %s", path, cmd[0], out)
			}
		}
		if _, err := os.Stat(v.e.previewRoot(path)); !os.IsNotExist(err) {
			t.Fatalf("%s: worktree left behind", path)
		}
		if wt := run(t, v.repo, "git", "worktree", "list"); strings.Count(wt, "\n") != 1 {
			t.Fatalf("git still knows the worktree:\n%s", wt)
		}
		if all, _ := v.e.DB.Previews(p); len(all) != 0 {
			t.Fatalf("preview still recorded: %+v", all)
		}
		// anonymous volumes (postgres' VOLUME in the migrate job) have no label: compare everything
		if vols, _ := podman.Run(ctx, "volume", "ls", "-q"); vols != volumesBefore {
			t.Fatalf("volumes left behind:\nbefore:\n%s\nafter:\n%s", volumesBefore, vols)
		}
	}
	if err := v.e.PreviewRm(ctx, io.Discard, p, "pr-1"); err != nil {
		t.Fatal(err)
	}
	gone(path)
	if code, _ := v.get(previewHost); code != 404 {
		t.Fatalf("removed preview still served: %d", code)
	}
	if n := psql(p, "SELECT count(*) FROM users"); n != "2" || !slices.Equal(ids(p), prodIDs) {
		t.Fatalf("production after rm: %s users", n)
	}

	// --from: a preview of a snapshot; then the ttl removes it
	psql(p, "INSERT INTO users VALUES ('carol')")
	snap, err := v.e.Snapshot(ctx, io.Discard, p, "")
	if err != nil {
		t.Fatal(err)
	}
	psql(p, "DELETE FROM users")
	path3 := PreviewPath(p, "pr-3")
	if err := v.e.PreviewUp(ctx, io.Discard, PreviewOpts{Project: p, Name: "pr-3", From: snap.ID}); err != nil {
		t.Fatal(err)
	}
	if n := psql(path3, "SELECT string_agg(name, ',' ORDER BY name) FROM users"); n != "alice,carol" {
		t.Fatalf("preview from snapshot #%d: %q", snap.ID, n)
	}
	v.e.PreviewTTL = time.Millisecond
	v.e.ExpirePreviews(ctx, io.Discard)
	gone(path3)
	if evs, _ := v.e.DB.Events(p, 20); !slices.ContainsFunc(evs, func(e store.Event) bool { return strings.Contains(e.Message, "pr-3 removed: not updated") }) {
		t.Fatalf("no expiry event: %+v", evs)
	}
}
