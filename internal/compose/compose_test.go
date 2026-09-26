package compose

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, path, yml string, env map[string]string, extra ...string) (*Project, error) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(yml), 0o644)
	files := []string{"compose.yml"}
	for i := 0; i+1 < len(extra); i += 2 {
		os.WriteFile(filepath.Join(dir, extra[i]), []byte(extra[i+1]), 0o644)
		if IsComposeFile(extra[i]) {
			files = append(files, extra[i])
		}
	}
	return Load(dir, path, files, env)
}

func TestInterpolate(t *testing.T) {
	env := map[string]string{"A": "a", "EMPTY": "", "N": "5"}
	for in, want := range map[string]string{
		"$A-${A}":              "a-a",
		"$$A":                  "$A",
		"${MISSING:-def}":      "def",
		"${EMPTY:-def}":        "def",
		"${EMPTY-def}":         "",
		"${A:+alt}":            "alt",
		"${MISSING+alt}":       "",
		"${MISSING:-${A}x}":    "ax",
		"x${N}y":               "x5y",
		"$1 $ ${A}":            "$1 $ a",
		"postgres://u:${A}@db": "postgres://u:a@db",
	} {
		var warns []string
		got, err := Interpolate(in, env, &warns)
		if err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, err, want)
		}
	}
	var warns []string
	if _, err := Interpolate("${SECRET:?set the db password}", env, &warns); err == nil || !strings.Contains(err.Error(), "set the db password") {
		t.Errorf("required var: %v", err)
	}
	Interpolate("$NOPE $NOPE", env, &warns)
	if len(warns) != 1 {
		t.Errorf("warnings: %v", warns)
	}
	if _, err := Interpolate("${A", env, &warns); err == nil {
		t.Error("unclosed brace accepted")
	}
}

func TestLoadFull(t *testing.T) {
	p, err := load(t, "shop/api", `
version: "3.9"
x-common: &common
  restart: always
services:
  web:
    <<: *common
    build: ./app
    command: serve --addr ":${PORT:-8080}" 'a b'
    environment:
      - PLAIN=1
      - DB_PASSWORD
      - NOT_SET_ANYWHERE
    env_file: .env.web
    depends_on:
      db:
        condition: service_healthy
    volumes:
      - ./uploads:/srv/uploads:Z
      - cache:/cache
    x-vops:
      port: ${PORT:-8080}
      health: /health
      replicas: 2
      domains: [shop.example.org]
    x-something-else: ignored
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
      EMPTY:
    volumes: [pg:/var/lib/postgresql/data]
    ports: ["127.0.0.1:5432:5432", "53:53/udp"]
    healthcheck:
      test: ["CMD", "pg_isready"]
      interval: 5s
      retries: 3
    ulimits:
      nofile: {soft: 1024, hard: 2048}
      nproc: 512
    deploy:
      resources:
        limits: {memory: 512m, cpus: "0.5"}
volumes:
  pg:
  cache: {}
`, map[string]string{"DB_PASSWORD": "s3cret"}, ".env.web", "# comment\nexport FROM_FILE=\"x y\"\nPLAIN=from-file\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 0 {
		t.Fatalf("warnings: %v", p.Warnings)
	}
	specs, err := p.Specs("example.com", map[string]string{"DB_PASSWORD": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Service != "db" || specs[1].Service != "web" {
		t.Fatalf("order: %s %s", specs[0].Service, specs[1].Service)
	}
	db, web := specs[0], specs[1]
	has := func(s *Spec, args ...string) {
		t.Helper()
		j := strings.Join(s.Args, " ")
		if !strings.Contains(j, strings.Join(args, " ")) {
			t.Errorf("%s: %q not in\n%s", s.Service, strings.Join(args, " "), j)
		}
	}
	has(db, "-e POSTGRES_PASSWORD=s3cret")
	has(db, "-p 127.0.0.1:5432:5432 -p 53:53/udp")
	has(db, "-v vops-shop.api-pg:/var/lib/postgresql/data")
	has(db, `--health-cmd ["pg_isready"] --health-interval 5s --health-retries 3`)
	has(db, "--restart unless-stopped")
	has(db, "--memory 512m --cpus 0.5")
	has(db, "--ulimit nofile=1024:2048 --ulimit nproc=512:512")
	if slices.Contains(db.Args, "EMPTY=") || db.Strategy != "recreate" || !db.Healthcheck {
		t.Errorf("db: %+v", db)
	}
	has(web, "-e DB_PASSWORD=s3cret -e FROM_FILE=x y -e PLAIN=1")
	has(web, "-v "+filepath.Join(p.Dir, "uploads")+":/srv/uploads:Z")
	has(web, "--restart always")
	if slices.ContainsFunc(web.Args, func(a string) bool { return strings.Contains(a, "NOT_SET_ANYWHERE") }) {
		t.Error("unset pass-through var leaked")
	}
	if want := []string{"serve", "--addr", ":8080", "a b"}; !slices.Equal(web.Cmd, want) {
		t.Errorf("cmd %q", web.Cmd)
	}
	if web.Port != 8080 || web.Replicas != 2 || web.Strategy != "rolling" || web.Build.Context != filepath.Join(p.Dir, "app") || web.Image != "" {
		t.Errorf("web: %+v", web)
	}
	if want := []string{"shop.example.org", "api.shop.example.com", "web.api.shop.example.com"}; !slices.Equal(web.Domains, want) {
		t.Errorf("domains %v", web.Domains)
	}
	if !slices.Equal(web.Volumes, []string{"vops-shop.api-cache"}) || !slices.Equal(web.Binds, []string{filepath.Join(p.Dir, "uploads")}) {
		t.Errorf("volumes %v binds %v", web.Volumes, web.Binds)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, c := range map[string]struct{ path, yml, want string }{
		"unknown key":      {"p", "services:\n  a:\n    image: x\n    networks: [foo]\n", "networks"},
		"container_name":   {"p", "services:\n  a:\n    image: x\n    container_name: foo\n", "container_name"},
		"no image":         {"p", "services:\n  a:\n    command: x\n", "needs image or build"},
		"cycle":            {"p", "services:\n  a:\n    image: x\n    depends_on: [b]\n  b:\n    image: x\n    depends_on: [a]\n", "cycle"},
		"unknown dep":      {"p", "services:\n  a:\n    image: x\n    depends_on: [c]\n", "unknown service"},
		"undeclared vol":   {"p", "services:\n  a:\n    image: x\n    volumes: [data:/d]\n", "not declared"},
		"rolling + ports":  {"p", "services:\n  a:\n    image: x\n    ports: [80:80]\n    x-vops: {port: 80, strategy: rolling}\n", "rolling"},
		"replicas":         {"p", "services:\n  a:\n    image: x\n    x-vops: {replicas: 2}\n", "replicas"},
		"bad project":      {"my.app", "services:\n  a:\n    image: x\n", "directory"},
		"required var":     {"p", "services:\n  a:\n    image: x:${TAG:?pick a tag}\n", "pick a tag"},
		"reserved label":   {"p", "services:\n  a:\n    image: x\n    labels: {vops.project: x}\n", "reserved"},
		"bad port":         {"p", "services:\n  a:\n    image: x\n    x-vops: {port: 70000}\n", "range"},
		"bad duration":     {"p", "services:\n  a:\n    image: x\n    stop_grace_period: soon\n", "duration"},
		"network_mode":     {"p", "services:\n  a:\n    image: x\n    network_mode: service:b\n", "network_mode"},
		"top-level secret": {"p", "services:\n  a:\n    image: x\nsecrets: {}\n", "secrets"},
	} {
		p, err := load(t, c.path, c.yml, nil)
		if err == nil {
			_, err = p.Specs("", nil)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want error with %q", name, err, c.want)
		}
	}
}

func TestMergeFilesAndDomains(t *testing.T) {
	p, err := load(t, "My_Shop", "services:\n  web:\n    image: x\n    x-vops: {port: 80}\n", nil,
		"worker.compose.yml", "services:\n  worker:\n    image: y\n  admin_ui:\n    image: z\n    x-vops: {port: 81}\n")
	if err != nil {
		t.Fatal(err)
	}
	specs, _ := p.Specs("example.com", nil)
	got := map[string][]string{}
	for _, s := range specs {
		got[s.Service] = s.Domains
	}
	// two routed services: nobody gets the bare project domain
	if !slices.Equal(got["web"], []string{"web.my-shop.example.com"}) || !slices.Equal(got["admin_ui"], []string{"admin-ui.my-shop.example.com"}) || got["worker"] != nil {
		t.Fatalf("domains %v", got)
	}
	if _, err := load(t, "p", "services:\n  a:\n    image: x\n", nil, "b.compose.yml", "services:\n  a:\n    image: y\n"); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate: %v", err)
	}
	if Slug("My_Shop/Api") != "my_shop.api" || DNSName("My_Shop/Api") != "api.my-shop" {
		t.Fatal("naming")
	}
}

func TestSplitWords(t *testing.T) {
	for in, want := range map[string][]string{
		`a b  c`:              {"a", "b", "c"},
		`sh -c "echo \"hi\""`: {"sh", "-c", `echo "hi"`},
		`'it''s' x\ y`:        {"its", "x y"},
		`""`:                  {""},
	} {
		got, err := SplitWords(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := SplitWords(`"open`); err == nil {
		t.Error("unterminated quote")
	}
}

func TestNetworks(t *testing.T) {
	p, err := load(t, "shop", `
services:
  web:
    image: x
    networks: [default, backend, vops]
    x-vops: {port: 80}
  db:
    image: x
    networks:
      backend:
        aliases: [database]
        ipv4_address: 10.99.0.10
  plain:
    image: x
networks:
  backend:
    internal: true
    driver_opts: {mtu: "1400"}
    ipam:
      config: [{subnet: 10.99.0.0/24}]
  ext:
    external: true
    name: legacy-net
`, nil)
	if err != nil {
		t.Fatal(err)
	}
	specs, err := p.Specs("", nil)
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string][]string{}
	for _, s := range specs {
		for _, n := range s.Networks {
			flags[s.Service] = append(flags[s.Service], n.Flag())
		}
	}
	want := map[string][]string{
		"web":   {"vops-shop-backend:alias=web", "vops-shop:alias=web", "vops:alias=web.shop"},
		"db":    {"vops-shop-backend:alias=db,alias=database,ip=10.99.0.10"},
		"plain": {"vops-shop:alias=plain", "vops:alias=plain.shop"},
	}
	for svc, w := range want {
		if !slices.Equal(flags[svc], w) {
			t.Errorf("%s: got %v want %v", svc, flags[svc], w)
		}
	}
	defs := p.NetworkDefs()
	if len(defs) != 2 || defs[0].Key != "backend" || defs[1].Key != "default" {
		t.Fatalf("defs %+v", defs)
	}
	if got := strings.Join(defs[0].CreateArgs(), " "); got != "--internal --opt mtu=1400 --subnet 10.99.0.0/24" {
		t.Errorf("create args %q", got)
	}
	if !defs[1].Plain() || defs[0].Plain() {
		t.Error("plain")
	}

	for name, c := range map[string]struct{ yml, want string }{
		"undeclared":    {"services:\n  a:\n    image: x\n    networks: [nope]\n", "not declared"},
		"reserved":      {"services:\n  a:\n    image: x\nnetworks:\n  vops: {}\n", "reserved"},
		"internal only": {"services:\n  a:\n    image: x\n    networks: [b]\n    x-vops: {port: 80}\nnetworks:\n  b: {internal: true}\n", "not internal"},
		"with mode":     {"services:\n  a:\n    image: x\n    network_mode: host\n    networks: [default]\n", "together"},
		"external+conf": {"services:\n  a:\n    image: x\nnetworks:\n  b: {external: true, internal: true}\n", "external"},
		"bad option":    {"services:\n  a:\n    image: x\n    networks: {default: {priority: 3}}\n", "priority"},
		"bad driver":    {"services:\n  a:\n    image: x\nnetworks:\n  b: {driver: overlay}\n", "overlay"},
	} {
		if _, err := load(t, "p", c.yml, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestProfilesAndJobs(t *testing.T) {
	yml := `
services:
  web:
    image: x
    depends_on:
      migrate: {condition: service_completed_successfully}
      cache: {condition: service_started, required: false}
  migrate:
    image: x
  cache:
    image: x
    profiles: [cache]
  debug:
    image: x
    profiles: [debug, tools]
`
	p, err := load(t, "p", yml, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Services["debug"]; ok || !slices.Equal(p.Inactive, []string{"cache", "debug"}) {
		t.Fatalf("inactive %v", p.Inactive)
	}
	specs, _ := p.Specs("", nil)
	byName := map[string]*Spec{}
	for _, s := range specs {
		byName[s.Service] = s
	}
	m := byName["migrate"]
	if !m.Job || m.Timeout != 10*time.Minute || !strings.Contains(strings.Join(m.Args, " "), "--restart no") || byName["web"].Job {
		t.Fatalf("migrate %+v", m)
	}
	if !slices.Equal(byName["web"].DependsOn, []string{"migrate"}) {
		t.Fatalf("web deps %v", byName["web"].DependsOn)
	}
	p, err = load(t, "p", yml, map[string]string{"COMPOSE_PROFILES": "tools, cache"})
	if err != nil || len(p.Services) != 4 {
		t.Fatalf("profiles on: %v %d", err, len(p.Services))
	}
	if _, err := load(t, "p", "services:\n  a:\n    image: x\n    depends_on: [b]\n  b:\n    image: x\n    profiles: [x]\n", nil); err == nil || !strings.Contains(err.Error(), "profile(s) x") {
		t.Fatalf("required dep in profile: %v", err)
	}
	for name, c := range map[string]struct{ yml, want string }{
		"job restart": {"services:\n  a:\n    image: x\n    depends_on: {b: {condition: service_completed_successfully}}\n  b:\n    image: x\n    restart: always\n", "loop"},
		"condition":   {"services:\n  a:\n    image: x\n    depends_on: {b: {condition: whenever}}\n  b:\n    image: x\n", "whenever"},
		"restart key": {"services:\n  a:\n    image: x\n    depends_on: {b: {restart: true}}\n  b:\n    image: x\n", "restart"},
	} {
		if _, err := load(t, "p", c.yml, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}
