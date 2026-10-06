package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every write lands in the audit log through triggers, and no secret does.
func TestAuditTriggers(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "vops.db"), filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.SetDisabled("shop", true)
	db.SetDisabled("shop", false)
	db.SetApplied("shop", "abcdef1234567890")
	db.SetEnv("shop", "DB_PASSWORD", "hunter2")
	db.SetEnv("shop", "DB_PASSWORD", "hunter3")
	db.UnsetEnv("shop", "DB_PASSWORD")
	db.PutUser(User{Name: "ci", Role: Deployer, Repos: []string{"shop/web"}}, "tok-secret")
	db.PutUser(User{Name: "ci", Role: Deployer, Global: true}, "tok-secret-2")
	db.DeleteUser("ci")
	db.PutUser(User{Name: "admin", Role: Admin}, "correct horse battery")
	id, _ := db.NewSession("admin", time.Hour)
	db.DeleteSession(id)
	sid, _ := db.AddSnapshot(Snapshot{Project: "shop", Reason: "manual", Volumes: []SnapshotVolume{{"volume", "v", "/a", "/b"}}})
	db.DeleteSnapshot(sid)
	db.AddDeploy(Deploy{Project: "shop", Commit: "abcdef1234567890", Trigger: "rollback", RestoredID: 7, Result: "ok"})
	db.PutPin(Pin{Project: "shop", Service: "web", Image: "r/shop/web:v1", Digest: "sha256:0123456789abcdef", ComposeImage: "r/shop/web:v2", DeployID: 3})
	db.DeletePin("shop", "web")
	db.DeleteProject("shop")

	logs, err := db.Audit(100)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := len(logs) - 1; i >= 0; i-- {
		l := logs[i]
		lines = append(lines, l.Tbl+" "+l.Op+" "+l.Key+" "+l.Detail)
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"projects insert shop disabled",
		"projects update shop enabled",
		"projects update shop commit  -> abcdef123456",
		"env insert shop DB_PASSWORD",
		"env update shop DB_PASSWORD value changed",
		"env delete shop DB_PASSWORD",
		"users insert ci role=deployer",
		"user_repos insert ci shop/web",
		"users update ci new token global",
		"user_repos delete ci shop/web",
		"users delete ci",
		"users insert admin role=admin global",
		"sessions insert",
		"logout admin",
		"snapshots insert shop #1 manual",
		"snapshots delete shop #1 manual",
		"deploys insert shop #1 rollback commit abcdef123456 ok restored #7",
		"pins insert shop/web r/shop/web:v1@0123456789ab from #3 (compose: r/shop/web:v2)",
		"pins delete shop/web r/shop/web:v1",
		"projects delete shop",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in audit log:\n%s", want, got)
		}
	}
	for _, secret := range []string{"hunter", "tok-secret", Hash("tok-secret"), "correct horse", id, Hash(id)} {
		if strings.Contains(got, secret) {
			t.Errorf("secret %q leaked into the audit log", secret)
		}
	}
	// reopening doesn't rerun migrations
	db.Close()
	db2, err := Open(filepath.Join(dir, "vops.db"), filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	db2.Close()
}

// Upgrading a host: a db made by the previous version (migrations up to 005, with deploys, snapshots and a
// preview in it) opens with the new migrations applied, keeps every row, and has no pins.
func TestUpgradeFromPreviousSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vops.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	for _, f := range []string{"001_init.sql", "002_snapshots.sql", "003_audit.sql", "004_previews.sql", "005_deploys.sql"} {
		b, err := os.ReadFile(filepath.Join("migrations", f))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		raw.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, f)
	}
	for _, q := range []string{
		`INSERT INTO snapshots (project, reason, note, commit_sha, created_at) VALUES ('shop', 'pre-deploy', '', 'abc', 1)`,
		`INSERT INTO deploys (project, commit_sha, trigger, images, snapshot_id, result, started_at, finished_at) VALUES ('shop', 'abc', 'sync', '{"web":{"image":"r/shop/web:v1","digest":"sha256:aa"}}', 0, 'ok', 1, 2)`,
		`INSERT INTO deploys (project, commit_sha, trigger, images, snapshot_id, restored_id, result, started_at, finished_at) VALUES ('shop', 'abc', 'rollback', '{}', 1, 1, 'ok', 3, 4)`,
		`INSERT INTO previews (project, name, images, created_at, updated_at, deploy_id) VALUES ('shop', 'pr-1', '{"web":"r/shop/web@sha256:bb"}', 1, 1, 1)`,
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
	ds, err := db.Deploys("shop", 10)
	if err != nil || len(ds) != 2 || ds[1].Images["web"].Digest != "sha256:aa" || ds[0].RestoredID != 1 || ds[0].BeforeID != 0 || ds[0].Parts != "" {
		t.Fatalf("deploys after upgrade: %v %+v", err, ds)
	}
	if pvs, _ := db.Previews("shop"); len(pvs) != 1 || pvs[0].DeployID != 1 {
		t.Fatalf("previews after upgrade: %+v", pvs)
	}
	if snaps, _ := db.Snapshots("shop"); len(snaps) != 1 {
		t.Fatalf("snapshots after upgrade: %+v", snaps)
	}
	if pins, err := db.Pins(""); err != nil || len(pins) != 0 {
		t.Fatalf("pins after upgrade: %v %+v", err, pins)
	}
	id, _ := db.AddDeploy(Deploy{Project: "shop", Trigger: "rollback", BeforeID: 1, Parts: "images,data", Result: "ok"})
	if d, _, _ := db.Deploy(id); d.BeforeID != 1 || d.Parts != "images,data" {
		t.Fatalf("new columns: %+v", d)
	}
	if logs, _ := db.Audit(5); !strings.Contains(logs[0].Detail, "back to before #1 (images,data)") {
		t.Fatalf("audit of a rollback: %+v", logs[0])
	}
}

// A key file of the wrong size is an error and stays as it is: replacing it would lose every secret.
func TestInvalidKeyIsKept(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	os.WriteFile(key, []byte("short"), 0o600)
	if _, err := Open(filepath.Join(dir, "vops.db"), key); err == nil || !strings.Contains(err.Error(), "want 32") {
		t.Fatalf("open with a 5-byte key: %v", err)
	}
	if b, _ := os.ReadFile(key); string(b) != "short" {
		t.Fatalf("the key was rewritten: %q", b)
	}
	os.Remove(key)
	db, err := Open(filepath.Join(dir, "vops.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if b, _ := os.ReadFile(key); len(b) != 32 {
		t.Fatalf("a missing key is generated: %d bytes", len(b))
	}
}
