package deploy

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/store"
)

// The env view says, per service, where every variable comes from (compose literal, env_file, vops env
// directly or through ${VAR}, or nowhere), which vops keys nothing uses, flags committed secrets, and
// never carries a value. The preview scope reads preview.env and the preview secrets instead.
func TestEnvUsage(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "vops")
	files := map[string]string{
		"vops.yml":         "domain: example.com\n",
		"shop/api.env":     "SMTP_HOST=mail\n",
		"shop/preview.env": "DATABASE_URL=postgres://preview\n",
		"shop/compose.yml": `services:
  api:
    image: x
    env_file: [api.env]
    environment:
      - JWT_SECRET=hunter2
      - LOG_LEVEL=debug
      - DATABASE_URL
      - STRIPE_KEY
      - REDIS_URL=redis://${REDIS_HOST}:6379
      - SENTRY_DSN=${SENTRY_DSN}
      - SITE=https://${VOPS_PROJECT_DOMAIN}
  worker:
    image: x
    environment: [DATABASE_URL]
`}
	for p, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0o755)
		os.WriteFile(filepath.Join(repo, p), []byte(c), 0o644)
	}
	run(t, repo, "git", "init", "-q", "-b", "main")
	run(t, repo, "git", "add", "-A")
	run(t, repo, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "c")
	db, err := store.Open(filepath.Join(dir, "vops.db"), filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetEnv("shop", "DATABASE_URL", "postgres://prod-value")
	db.SetEnv("shop", "REDIS_HOST", "redis-prod-value")
	db.SetEnv("shop", "OLD_TOKEN", "old-value")
	db.SetEnv(PreviewEnvScope("shop"), "STRIPE_KEY", "sk_test_value")
	db.SetEnv(PreviewEnvScope("shop"), "NOBODY", "x")
	e := &Engine{Repo: repo, DB: db}
	ctx := context.Background()

	view := func(u EnvUsage) string {
		var lines []string
		for _, svc := range sortedKeys(u.Services) {
			for _, en := range u.Services[svc] {
				lines = append(lines, strings.Join(strings.Fields(svc+" "+en.Key+" "+en.Source+" "+en.File+" "+strings.Join(en.Vars, ",")+map[bool]string{true: " secret"}[en.Secret]), " "))
			}
		}
		return strings.Join(append(lines, "unused: "+strings.Join(u.Unused, ",")), "\n")
	}
	u, err := e.EnvUsage(ctx, "shop", false)
	if err != nil {
		t.Fatal(err)
	}
	want := `api DATABASE_URL vops
api JWT_SECRET compose secret
api LOG_LEVEL compose
api REDIS_URL vops REDIS_HOST
api SENTRY_DSN missing SENTRY_DSN
api SITE compose VOPS_PROJECT_DOMAIN
api SMTP_HOST env_file api.env
api STRIPE_KEY missing
worker DATABASE_URL vops
unused: OLD_TOKEN`
	if got := view(u); got != want {
		t.Fatalf("production:\n%s\nwant:\n%s", got, want)
	}
	b, _ := json.Marshal(u)
	for _, value := range []string{"hunter2", "debug", "prod-value", "old-value", "mail"} {
		if strings.Contains(string(b), value) {
			t.Fatalf("a value leaked: %s in %s", value, b)
		}
	}

	// previews: preview.env and preview secrets, never production's env
	u, err = e.EnvUsage(ctx, "shop", true)
	if err != nil {
		t.Fatal(err)
	}
	got := view(u)
	for _, line := range []string{"api DATABASE_URL env_file preview.env", "api STRIPE_KEY vops", "api REDIS_URL compose REDIS_HOST", "worker DATABASE_URL env_file preview.env", "unused: NOBODY"} {
		if !strings.Contains(got, line+"\n") && !strings.HasSuffix(got, line) {
			t.Fatalf("preview scope: missing %q in\n%s", line, got)
		}
	}
	if !slices.ContainsFunc(u.Services["api"], func(en compose.EnvEntry) bool { return en.Key == "REDIS_URL" }) {
		t.Fatal("REDIS_URL gone in the preview scope")
	}
}
