package store

import (
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
	db.PutUser(User{Name: "ci", Pattern: "shop/.*"}, "tok-secret")
	db.PutUser(User{Name: "ci", Pattern: "shop/.*"}, "tok-secret-2")
	db.DeleteUser("ci")
	db.SetAdminPassword("correct horse battery")
	id, _ := db.NewSession(time.Hour)
	db.DeleteSession(id)
	sid, _ := db.AddSnapshot(Snapshot{Project: "shop", Reason: "manual", Volumes: []SnapshotVolume{{"volume", "v", "/a", "/b"}}})
	db.DeleteSnapshot(sid)
	db.AddDeploy(Deploy{Project: "shop", Commit: "abcdef1234567890", Trigger: "rollback", RestoredID: 7, Result: "ok"})
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
		"users insert ci pattern=shop/.* repos=[]",
		"users update ci new token pattern=shop/.* repos=[]",
		"users delete ci",
		"meta insert admin",
		"sessions insert",
		"logout",
		"snapshots insert shop #1 manual",
		"snapshots delete shop #1 manual",
		"deploys insert shop #1 rollback commit abcdef123456 ok restored #7",
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
