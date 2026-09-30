package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "vops.db"), filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustPut(t *testing.T, db *DB, u User, secret string) {
	t.Helper()
	if err := db.PutUser(u, secret); err != nil {
		t.Fatalf("put %s: %v", u.Name, err)
	}
}

func sessions(t *testing.T, db *DB, user string) int {
	t.Helper()
	var n int
	db.sql.QueryRow(`SELECT count(*) FROM sessions WHERE user = ?`, user).Scan(&n)
	return n
}

// The db keeps users consistent by itself: triggers and checks, whatever the code path.
func TestUserRules(t *testing.T) {
	db := open(t)
	mustPut(t, db, User{Name: "admin", Role: Admin}, "admin password 1")
	mustPut(t, db, User{Name: "ci", Role: Deployer, Repos: []string{" shop/web:latest", "shop/web", "shop/api:v1", ""}}, "token-ci-0123")

	if u, _, _ := db.User("ci"); !slices.Equal(u.Repos, []string{"shop/api", "shop/web"}) || u.Global {
		t.Fatalf("normalized repos: %+v", u)
	}
	if err := db.DeleteUser("admin"); err == nil || err.Error() != "the last admin can't be deleted" {
		t.Fatalf("delete last admin: %v", err)
	}
	if err := db.PutUser(User{Name: "admin", Role: Deployer, Global: true}, "a-new-token-123"); err == nil || err.Error() != "the last admin can't be demoted" {
		t.Fatalf("demote last admin: %v", err)
	}
	if err := db.PutUser(User{Name: "ci", Role: Admin}, ""); err == nil {
		t.Fatal("a deployer became admin without a password")
	}
	if !db.HasAdmin() {
		t.Fatal("no admin")
	}

	// raw sql can't break the rules either
	for q, want := range map[string]string{
		`UPDATE users SET global = 0 WHERE name = 'admin'`:                                                           "CHECK",
		`INSERT INTO users (name, role, global, secret, created_at) VALUES ('x', 'root', 1, 'h', 0)`:                 "CHECK",
		`INSERT INTO users (name, role, global, secret, created_at) VALUES ('vops-internal', 'deployer', 1, 'h', 0)`: "CHECK",
		`INSERT INTO users (name, role, global, secret, created_at) VALUES ('x', 'admin', 1, 'plainhash', 0)`:        "CHECK",
		`INSERT INTO user_repos (user, repo) VALUES ('ci', 'shop/db:latest')`:                                        "CHECK",
		`INSERT INTO user_repos (user, repo) VALUES ('ci', '')`:                                                      "CHECK",
		`INSERT INTO user_repos (user, repo) VALUES ('ghost', 'shop/db')`:                                            "FOREIGN KEY",
		`INSERT INTO user_repos (user, repo) VALUES ('admin', 'shop/db')`:                                            "a global user has no repo list",
		`DELETE FROM users WHERE name = 'admin'`:                                                                     "the last admin",
	} {
		if _, err := db.sql.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}

	// sessions go with a new secret, a demotion and a deletion
	mustPut(t, db, User{Name: "ops", Role: Admin}, "ops password 12")
	db.NewSession("ops", time.Hour)
	db.NewSession("admin", time.Hour)
	mustPut(t, db, User{Name: "ops", Role: Admin}, "ops password 13")
	if sessions(t, db, "ops") != 0 || sessions(t, db, "admin") != 1 {
		t.Fatal("a new password kept the sessions (or dropped someone else's)")
	}
	db.NewSession("ops", time.Hour)
	mustPut(t, db, User{Name: "ops", Role: Deployer, Global: true}, "ops-token-0123")
	if sessions(t, db, "ops") != 0 {
		t.Fatal("a demoted admin kept its sessions")
	}
	if u, ok := db.CheckUser("ops", "ops-token-0123"); !ok || u.Role != Deployer {
		t.Fatalf("demoted user: %+v %v", u, ok)
	}
	if _, ok := db.CheckUser("ops", "ops password 13"); ok {
		t.Fatal("the old password still works")
	}
	id, _ := db.NewSession("admin", time.Hour)
	mustPut(t, db, User{Name: "ops", Role: Admin}, "ops password 14")
	if err := db.DeleteUser("admin"); err != nil {
		t.Fatalf("delete a non-last admin: %v", err)
	}
	if _, ok := db.CheckSession(id); ok || sessions(t, db, "admin") != 0 {
		t.Fatal("a deleted user kept its session")
	}

	// global drops the list; deleting cascades to repos
	mustPut(t, db, User{Name: "ci", Role: Deployer, Global: true}, "")
	if u, _, _ := db.User("ci"); !u.Global || len(u.Repos) != 0 || !u.Allows("any/thing") {
		t.Fatalf("global: %+v", u)
	}
	mustPut(t, db, User{Name: "ci", Role: Deployer, Repos: []string{"a/b"}}, "")
	if u, _, _ := db.User("ci"); u.Global || !u.Allows("a/b") || u.Allows("a/c") {
		t.Fatalf("back to a list: %+v", u)
	}
	db.DeleteUser("ci")
	var n int
	db.sql.QueryRow(`SELECT count(*) FROM user_repos`).Scan(&n)
	if n != 0 {
		t.Fatalf("repos left after delete: %d", n)
	}
	if err := db.DeleteUser("ci"); err == nil {
		t.Fatal("deleting a missing user")
	}
}

func TestSessions(t *testing.T) {
	db := open(t)
	mustPut(t, db, User{Name: "admin", Role: Admin}, "admin password 1")
	id, _ := db.NewSession("admin", time.Hour)
	if u, ok := db.CheckSession(id); !ok || u != "admin" {
		t.Fatalf("session: %q %v", u, ok)
	}
	old, _ := db.NewSession("admin", -time.Minute)
	if _, ok := db.CheckSession(old); ok {
		t.Fatal("expired session accepted")
	}
	if n, err := db.PurgeSessions(); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, ok := db.CheckSession(id); !ok {
		t.Fatal("purge dropped a live session")
	}
	db.DeleteSession(id)
	if _, ok := db.CheckSession(id); ok {
		t.Fatal("deleted session accepted")
	}
}

// A host at migration 006: registry users with '.*', a repo list and a custom regex, and the dashboard password
// in meta. After 007 they are deployers (global, a list, nothing + an event) and admin is a user with the same password.
func TestUsersMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vops.db")
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	raw.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	files, _ := filepath.Glob("migrations/00[1-6]_*.sql")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if _, err := raw.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		raw.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, filepath.Base(f))
	}
	adminHash, err := secretHash(Admin, "the old admin password")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (name, token_hash, pattern, repos, created_at) VALUES ('all', '` + Hash("tok-all") + `', '.*', '["x/y"]', 1)`,
		`INSERT INTO users (name, token_hash, pattern, repos, created_at) VALUES ('list', '` + Hash("tok-list") + `', '', '["shop/web:latest","shop/api"," shop/web"]', 1)`,
		`INSERT INTO users (name, token_hash, pattern, repos, created_at) VALUES ('regex', '` + Hash("tok-regex") + `', 'shop/.*', '[]', 1)`,
		`INSERT INTO meta (key, value) VALUES ('admin', '` + adminHash + `')`,
		`INSERT INTO sessions (id_hash, expires) VALUES ('abc', 9999999999)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	db, err := Open(path, filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	users, _ := db.Users()
	got := map[string]User{}
	for _, u := range users {
		got[u.Name] = u
	}
	if u := got["all"]; u.Role != Deployer || !u.Global || len(u.Repos) != 0 {
		t.Errorf("all: %+v", u)
	}
	if u := got["list"]; u.Role != Deployer || u.Global || !slices.Equal(u.Repos, []string{"shop/api", "shop/web"}) {
		t.Errorf("list: %+v", u)
	}
	if u := got["regex"]; u.Global || len(u.Repos) != 0 {
		t.Errorf("regex: %+v", u)
	}
	if u, ok := db.CheckUser("admin", "the old admin password"); !ok || u.Role != Admin || !u.Global {
		t.Errorf("admin after migration: %+v %v", u, ok)
	}
	if _, ok := db.CheckUser("list", "tok-list"); !ok {
		t.Error("a deployer's token stopped working")
	}
	if db.Meta("admin") != "" {
		t.Error("meta admin kept")
	}
	evs, _ := db.Events("", 10)
	if len(evs) != 1 || evs[0].Kind != "auth" || !strings.Contains(evs[0].Message, "regex lost its pattern shop/.*") {
		t.Errorf("events: %+v", evs)
	}
	var n int
	db.sql.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Error("old sessions kept")
	}
	logs, _ := db.Audit(100)
	for _, l := range logs {
		if strings.Contains(l.Detail, adminHash) || strings.Contains(l.Detail, Hash("tok-all")) {
			t.Errorf("a secret in the audit log: %+v", l)
		}
	}
}

// Names are data, never SQL: injection-looking strings round-trip and hurt nothing.
func TestInjectionStringsRoundTrip(t *testing.T) {
	db := open(t)
	for _, s := range []string{`x'; DROP TABLE env;--`, `a"b`, `' OR '1'='1`, `x'); DELETE FROM users;--`, "tab\tnew\nline", `%_\`, "ünï 😀"} {
		if err := db.SetEnv(s, s, s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		env, err := db.Env(s)
		if err != nil || env[s] != s {
			t.Fatalf("env %q: %v %v", s, env, err)
		}
		if keys, _ := db.EnvKeys(s); len(keys) != 1 || keys[0].Key != s {
			t.Fatalf("keys %q: %+v", s, keys)
		}
		mustPut(t, db, User{Name: s, Role: Deployer, Repos: []string{s}}, "token-0123456789")
		if u, ok := db.CheckUser(s, "token-0123456789"); !ok || u.Name != s || len(u.Repos) != 1 || u.Repos[0] != strings.TrimSpace(s) {
			t.Fatalf("user %q: %+v %v", s, u, ok)
		}
		db.SetDisabled(s, true)
		db.Event(s, "test", "%s", s)
		if evs, _ := db.Events(s, 1); len(evs) != 1 || evs[0].Message != s {
			t.Fatalf("events %q: %+v", s, evs)
		}
	}
	if len(must(db.Users())) != 7 {
		t.Fatal("users lost")
	}
	if ps, _ := db.Projects(); len(ps) != 7 {
		t.Fatalf("projects: %d", len(ps))
	}
}

func must[T any](v T, _ error) T { return v }
