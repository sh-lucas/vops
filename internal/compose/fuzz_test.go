package compose

import (
	"os"
	"path/filepath"
	"testing"
)

// The loader takes any file from git: whatever it gets, it errors or loads, never panics.
func FuzzLoad(f *testing.F) {
	for _, s := range []string{
		"services:\n  web:\n    image: x\n    x-vops: {port: 80}\n",
		"x-vops: {rate: 20/s, burst: 5, max_body: 1MB, previews: \"pr-*\"}\nservices:\n  a:\n    image: x\n",
		"services:\n  a:\n    image: ${IMG:-x}\n    depends_on: {b: {condition: service_completed_successfully}}\n  b:\n    image: y\n    command: echo $$HOME\n",
		"services:\n  a: &a\n    image: x\n    volumes: [\"data:/d:U\", \"./f:/f\"]\n    networks: {n: {aliases: [z]}}\n  b:\n    <<: *a\nvolumes:\n  data:\nnetworks:\n  n: {internal: true}\n",
		"services:\n  a:\n    build: {context: ., args: [A=1]}\n    healthcheck: {test: [CMD, true], interval: 1s}\n    ports: [\"8080:80\"]\n",
		"services: [1, 2]\n",
		"{",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, yml string) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"IMG": "y"}
		p, err := Load(dir, "shop/api", []string{"compose.yml"}, env)
		if err != nil {
			return
		}
		p.Specs("example.com", env)
		if pv, err := Load(dir, "shop/api@pr-1", []string{"compose.yml"}, env); err == nil {
			pv.Specs("example.com", env)
		}
	})
}
