package e2e

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sh-lucas/vops/internal/podman"
)

// Previews from registry tags, through the real binary and daemon: pushing shop/web:preview-pr-7 creates
// preview pr-7 of shop with that image, pushing shop/api:preview-pr-7 lands in the same preview, pushing web
// again updates it, and production is never redeployed. Plus env --preview, preview up/ls/rm and the api.
func TestPreviewsFromRegistry(t *testing.T) {
	w := setup(t)
	dev := filepath.Join(t.TempDir(), "infra")
	os.MkdirAll(dev, 0o755)
	w.vops(dev, "init", "--domain", "vops.test", "--email", "me@vops.test")
	w.setConfig(dev, map[string]string{"http": w.httpAddr, "https": "off", "ui": w.uiAddr, "tls": "off"})
	out := w.vops(dev, "install", "dev@fakehost", "--binary", w.bin)
	m := regexp.MustCompile(`dashboard login: admin / (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no admin password:\n%s", out)
	}
	w.startDaemon()
	tm := regexp.MustCompile(`token: (\S+)`).FindStringSubmatch(w.vops(dev, "user", "add", "ci", "--pattern", "shop/.*"))
	ctx := context.Background()
	push := func(repo, tag, msg string) {
		t.Helper()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+w.app+"\nENV MSG="+msg+"\n"), 0o644)
		ref := w.uiAddr + "/" + repo + ":" + tag
		if _, err := podman.Run(ctx, "build", "-q", "-t", ref, dir); err != nil {
			t.Fatal(err)
		}
		if _, err := podman.Run(ctx, "push", "-q", "--tls-verify=false", "--creds", "ci:"+tm[1], ref); err != nil {
			t.Fatal(err)
		}
	}
	body := func(host string) string { _, b := w.get(host); return b }
	push("shop/web", "v1", "web-prod")
	push("shop/api", "v1", "api-prod")
	w.vops(dev, "env", "set", "shop", "SECRET=prod-secret")
	w.vops(dev, "env", "set", "shop", "--preview", "PREVIEW_ONLY=yes")
	if keys := w.vops(dev, "env", "ls", "shop", "--preview"); !strings.Contains(keys, "PREVIEW_ONLY") || strings.Contains(keys, "SECRET") {
		t.Fatalf("preview env ls: %s", keys)
	}
	compose := `services:
  web:
    image: UI/shop/web:v1
    environment: [SECRET, PREVIEW_ONLY]
    x-vops: {port: 8080}
  api:
    image: UI/shop/api:v1
    x-vops: {port: 8080}
`
	w.write(dev, map[string]string{"shop/compose.yml": compose})
	w.vops(dev, "sync", "--yes")
	if body("web.shop.vops.test") != "web-prod" || body("api.shop.vops.test") != "api-prod" {
		t.Fatal("production not serving")
	}
	prodIDs := func() string {
		cs, _ := podman.PS(ctx, "vops.project=shop")
		var ids []string
		for _, c := range cs {
			ids = append(ids, c.ID)
		}
		slices.Sort(ids)
		return strings.Join(ids, ",")
	}
	prod := prodIDs()

	// a preview tag creates the preview; a second repo lands in the same one; a second push updates it
	push("shop/web", "preview-pr-7", "web-pr7")
	w.eventually("preview pr-7 from a push", func() bool { return body("web.pr-7.shop.vops.test") == "web-pr7" })
	if b := body("api.pr-7.shop.vops.test"); b != "api-prod" {
		t.Fatalf("api of the preview should run production's image: %q", b)
	}
	push("shop/api", "preview-pr-7", "api-pr7")
	w.eventually("api pushed into the same preview", func() bool { return body("api.pr-7.shop.vops.test") == "api-pr7" })
	if b := body("web.pr-7.shop.vops.test"); b != "web-pr7" {
		t.Fatalf("web override lost: %q", b)
	}
	push("shop/web", "preview-pr-7", "web-pr7-b")
	w.eventually("preview updated by a second push", func() bool { return body("web.pr-7.shop.vops.test") == "web-pr7-b" })
	if prodIDs() != prod || body("web.shop.vops.test") != "web-prod" || body("api.shop.vops.test") != "api-prod" {
		t.Fatal("a preview tag touched production")
	}
	// env: the preview secrets, never production's
	var cs []podman.Container
	w.eventually("the old replica drained", func() bool {
		cs, _ = podman.PS(ctx, "vops.project=shop@pr-7", "vops.service=web")
		return len(cs) == 1
	})
	envs, _ := podman.Run(ctx, "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", cs[0].ID)
	if !strings.Contains(envs, "PREVIEW_ONLY=yes") || strings.Contains(envs, "SECRET=") {
		t.Fatalf("preview env:\n%s", envs)
	}
	if ls := w.vops(dev, "preview", "ls", "shop"); !strings.Contains(ls, "pr-7") || !strings.Contains(ls, "api="+w.uiAddr+"/shop/api:preview-pr-7") || !strings.Contains(ls, "web=") {
		t.Fatalf("preview ls:\n%s", ls)
	}

	// from the cli: production's images
	if out := w.vops(dev, "preview", "up", "shop", "--name", "manual"); !strings.Contains(out, "serving") {
		t.Fatalf("preview up:\n%s", out)
	}
	if b := body("web.manual.shop.vops.test"); b != "web-prod" {
		t.Fatalf("manual preview: %q", b)
	}
	if out, err := w.try(dev, "", "preview", "up", "shop", "--name", "Bad_Name"); err == nil {
		t.Fatalf("invalid name accepted:\n%s", out)
	}
	// --ref: a local branch is pushed to the host and checked out in the preview's worktree
	w.git(dev, "checkout", "-q", "-b", "feature")
	w.write(dev, map[string]string{"shop/compose.yml": strings.Replace(compose, "[SECRET, PREVIEW_ONLY]", "{MSG: from-branch}", 1)})
	w.git(dev, "checkout", "-q", "main")
	w.vops(dev, "preview", "up", "shop", "--name", "feat", "--ref", "feature")
	if b := body("web.feat.shop.vops.test"); b != "from-branch" || body("web.shop.vops.test") != "web-prod" {
		t.Fatalf("preview of a branch: %q", b)
	}
	w.vops(dev, "preview", "rm", "shop", "feat")

	// dashboard api: list, remove
	client := &http.Client{}
	call := func(method, path, body string, cookie *http.Cookie) (int, string) {
		req, _ := http.NewRequest(method, "http://"+w.uiAddr+path, strings.NewReader(body))
		req.Header.Set("X-Vops", "1")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if cookie == nil && len(resp.Cookies()) > 0 {
			return resp.StatusCode, resp.Cookies()[0].Value
		}
		return resp.StatusCode, string(b)
	}
	_, session := call("POST", "/api/login", `{"password":"`+m[1]+`"}`, nil)
	cookie := &http.Cookie{Name: "vops_session", Value: session}
	if code, b := call("GET", "/api/previews?project=shop", "", cookie); code != 200 || !strings.Contains(b, `"name":"pr-7"`) || !strings.Contains(b, `"name":"manual"`) || !strings.Contains(b, `"expires_at"`) {
		t.Fatalf("GET /api/previews: %d %s", code, b)
	}
	if code, b := call("DELETE", "/api/previews?project=shop&name=manual", "", cookie); code != 200 {
		t.Fatalf("DELETE /api/previews: %d %s", code, b)
	}
	if code, _ := w.get("web.manual.shop.vops.test"); code != 404 {
		t.Fatalf("removed preview still served: %d", code)
	}

	// rm from the cli leaves nothing behind
	w.vops(dev, "preview", "rm", "shop", "pr-7")
	if code, _ := w.get("web.pr-7.shop.vops.test"); code != 404 {
		t.Fatalf("removed preview still served: %d", code)
	}
	for _, p := range []string{"shop@pr-7", "shop@manual"} {
		for _, cmd := range [][]string{{"ps", "-aq"}, {"network", "ls", "-q"}, {"volume", "ls", "-q"}} {
			if out, _ := podman.Run(ctx, append(cmd, "--filter", "label=vops.project="+p)...); out != "" {
				t.Fatalf("%s: %s left behind: %s", p, cmd[0], out)
			}
		}
	}
	if left, _ := os.ReadDir(filepath.Join(w.hostHome, ".vops", "previews")); len(left) != 0 {
		t.Fatalf("worktrees left behind: %v", left)
	}
	if ev := w.vops(dev, "events", "shop"); !strings.Contains(ev, "preview pr-7 created") || !strings.Contains(ev, "preview pr-7 removed") {
		t.Fatalf("events:\n%s", ev)
	}
	if prodIDs() != prod {
		t.Fatal("production containers changed")
	}

	// history: a push of the tag production runs is a deploy with trigger push and the new digest
	push("shop/web", "v1", "web-prod-2")
	w.eventually("push-triggered redeploy", func() bool { return body("web.shop.vops.test") == "web-prod-2" })
	var h string
	w.eventually("the push deploy recorded", func() bool { h = w.vops(dev, "history", "shop"); return strings.Contains(h, "push") })
	if !regexp.MustCompile(`(?m)^\d+\s+.+?\s+push\s+\w+\s+ok\s+-\s+web: @\w+ → @\w+`).MatchString(h) || !strings.Contains(h, "sync") {
		t.Fatalf("history:\n%s", h)
	}
	type image struct{ Image, Digest string }
	var tl struct {
		Nodes []struct {
			Kind     string
			Previews []string
			Deploy   struct {
				ID      int64
				Trigger string
				Commit  string
				Images  map[string]image
			}
		}
	}
	_, b := call("GET", "/api/timeline?project=shop", "", cookie)
	if err := json.Unmarshal([]byte(b), &tl, json.MatchCaseInsensitiveNames(true)); err != nil || len(tl.Nodes) != 2 || tl.Nodes[0].Deploy.Trigger != "push" || tl.Nodes[1].Deploy.Trigger != "sync" {
		t.Fatalf("timeline: %v %s", err, b)
	}
	before, after := tl.Nodes[1].Deploy, tl.Nodes[0].Deploy
	if before.Images["web"].Digest == "" || before.Images["web"].Digest == after.Images["web"].Digest || before.Images["api"] != after.Images["api"] {
		t.Fatalf("digests: %+v -> %+v", before.Images, after.Images)
	}
	// a preview from the sync deploy runs the web image by the digest it ran then, not what v1 is now
	pinned := map[string]string{}
	for svc, img := range before.Images {
		pinned[svc] = strings.Split(img.Image, ":v1")[0] + "@" + img.Digest
	}
	name := fmt.Sprintf("at-%d", before.ID)
	req, _ := json.Marshal(map[string]any{"project": "shop", "name": name, "ref": before.Commit, "images": pinned, "deploy": before.ID})
	if code, b := call("POST", "/api/previews", string(req), cookie); code != 200 || !strings.HasSuffix(b, "==> ok\n") {
		t.Fatalf("preview from a deploy: %d %s", code, b)
	}
	if b := body("web." + name + ".shop.vops.test"); b != "web-prod" || body("web.shop.vops.test") != "web-prod-2" {
		t.Fatalf("preview from deploy #%d serves %q", before.ID, b)
	}
	if code, _ := call("GET", "/api/logs?n=5&project=shop@"+name, "", cookie); code != 200 {
		t.Fatalf("preview logs: %d", code)
	}
	_, b = call("GET", "/api/timeline?project=shop", "", cookie)
	json.Unmarshal([]byte(b), &tl, json.MatchCaseInsensitiveNames(true))
	if len(tl.Nodes) != 2 || !slices.Equal(tl.Nodes[1].Previews, []string{name}) || len(tl.Nodes[0].Previews) != 0 {
		t.Fatalf("preview chip: %s", b)
	}
	w.vops(dev, "preview", "rm", "shop", name)
}
