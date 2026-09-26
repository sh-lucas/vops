package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/testenv"
)

// testRegistry: "alice" pushes/pulls app/*, "admin" pulls everything and pushes nothing.
func testRegistry(t *testing.T) (*Registry, *httptest.Server, chan string) {
	t.Helper()
	reg, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pushed := make(chan string, 10)
	reg.OnPush = func(repo, tag, digest string) { pushed <- repo + ":" + tag }
	reg.Auth = func(user, pass string) *Perm {
		switch user + ":" + pass {
		case "alice:secret":
			own := func(r string) bool { return strings.HasPrefix(r, "app/") }
			return &Perm{Name: user, Pull: own, Push: own}
		case "admin:pw":
			return &Perm{Name: user, Pull: func(string) bool { return true }, Push: func(string) bool { return false }}
		}
		return nil
	}
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	return reg, srv, pushed
}

func host(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

// The real thing: podman pushes, the registry stores, podman pulls it back and runs it.
func TestPodmanPushPull(t *testing.T) {
	app := testenv.AppImage(t)
	reg, srv, pushed := testRegistry(t)
	ctx := context.Background()
	ref := host(srv) + "/app/web:v1"
	if _, err := podman.Run(ctx, "tag", app, ref); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { podman.Run(ctx, "rmi", "-f", ref) })

	push := func(creds, ref string) error {
		_, err := podman.Run(ctx, "push", "-q", "--tls-verify=false", "--creds", creds, ref)
		return err
	}
	if err := push("admin:pw", ref); err == nil {
		t.Fatal("admin must not push")
	}
	if err := push("alice:wrong", ref); err == nil {
		t.Fatal("wrong password pushed")
	}
	other := host(srv) + "/other/web:v1"
	podman.Run(ctx, "tag", app, other)
	t.Cleanup(func() { podman.Run(ctx, "rmi", "-f", other) })
	if err := push("alice:secret", other); err == nil {
		t.Fatal("alice pushed outside her pattern")
	}
	if err := push("alice:secret", ref); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pushed:
		if got != "app/web:v1" {
			t.Fatalf("OnPush got %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnPush not called")
	}
	// second push of the same image is all HEADs, must still work
	if err := push("alice:secret", ref); err != nil {
		t.Fatal(err)
	}

	if tags := reg.Tags("app/web"); len(tags) != 1 || tags[0].Name != "v1" || tags[0].Size < 1000 {
		t.Fatalf("tags: %+v", tags)
	}
	if _, err := podman.Run(ctx, "rmi", "-f", ref); err != nil {
		t.Fatal(err)
	}
	if _, err := podman.Run(ctx, "pull", "-q", "--tls-verify=false", "--creds", "admin:pw", ref); err != nil {
		t.Fatal("admin pull:", err)
	}
	out, err := podman.Run(ctx, "run", "--rm", ref, "lookup", "localhost")
	if err != nil || !strings.Contains(out, "127.0.0.1") {
		t.Fatalf("run pulled image: %q %v", out, err)
	}
}

// Multi-arch style: an index pointing at a manifest, pushed with podman manifest push --all.
func TestPodmanManifestList(t *testing.T) {
	app := testenv.AppImage(t)
	reg, srv, _ := testRegistry(t)
	ctx := context.Background()
	list := "localhost/vops-test-list:" + fmt.Sprint(time.Now().UnixNano())
	if _, err := podman.Run(ctx, "manifest", "create", list, "containers-storage:"+app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { podman.Run(ctx, "manifest", "rm", list) })
	ref := host(srv) + "/app/multi:latest"
	if _, err := podman.Run(ctx, "manifest", "push", "-q", "--all", "--tls-verify=false", "--creds", "alice:secret", list, "docker://"+ref); err != nil {
		t.Fatal(err)
	}
	if _, err := podman.Run(ctx, "pull", "-q", "--tls-verify=false", "--creds", "alice:secret", ref); err != nil {
		t.Fatal(err)
	}
	podman.Run(ctx, "rmi", "-f", ref)
	d := reg.Resolve("app/multi", "latest")
	mt, _ := os.ReadFile(reg.repoPath("app/multi", "manifests", hexOf(d)))
	if !strings.Contains(string(mt), "index") && !strings.Contains(string(mt), "list") {
		t.Fatalf("expected an index, got %s", mt)
	}
	// GC must keep the child manifest of a tagged index
	if _, err := reg.GC(0); err != nil {
		t.Fatal(err)
	}
	if _, err := podman.Run(ctx, "pull", "-q", "--tls-verify=false", "--creds", "alice:secret", ref); err != nil {
		t.Fatal("pull after gc:", err)
	}
	podman.Run(ctx, "rmi", "-f", ref)
}

type client struct {
	t    *testing.T
	base string
	user string
}

func (c client) do(method, path string, body []byte, hdr ...string) *http.Response {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	req.SetBasicAuth(c.user, map[string]string{"alice": "secret", "admin": "pw"}[c.user])
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (c client) expect(resp *http.Response, status int) *http.Response {
	c.t.Helper()
	if resp.StatusCode != status {
		b, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("%s %s: got %d want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, b)
	}
	return resp
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func pushBlob(c client, repo string, blob []byte) string {
	d := digestOf(blob)
	c.expect(c.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+d, blob), 201)
	return d
}

func pushManifest(c client, repo, ref string, config string, layers ...string) (string, []byte) {
	ls := []string{}
	for _, l := range layers {
		ls = append(ls, fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":%q,"size":1}`, l))
	}
	m := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":2},"layers":[%s]}`, config, strings.Join(ls, ","))
	c.expect(c.do("PUT", "/v2/"+repo+"/manifests/"+ref, m, "Content-Type", ociManifest), 201)
	return digestOf(m), m
}

// Protocol details podman doesn't exercise: chunked uploads, ranges, mounts, pagination, referrers, deletes.
func TestProtocol(t *testing.T) {
	reg, srv, _ := testRegistry(t)
	c := client{t, srv.URL, "alice"}

	// chunked upload with Content-Range, a bad range, status, then finish with the last chunk
	resp := c.expect(c.do("POST", "/v2/app/a/blobs/uploads/", nil), 202)
	loc := resp.Header.Get("Location")
	c.expect(c.do("PATCH", loc, []byte("hello "), "Content-Range", "0-5"), 202)
	c.expect(c.do("PATCH", loc, []byte("x"), "Content-Range", "0-0"), 416)
	if r := c.expect(c.do("GET", loc, nil), 204); r.Header.Get("Range") != "0-5" {
		t.Fatalf("range %q", r.Header.Get("Range"))
	}
	layer := []byte("hello world")
	d := digestOf(layer)
	c.expect(c.do("PUT", loc+"?digest="+d, []byte("world")), 201)
	c.expect(c.do("PUT", loc+"?digest="+d, nil), 404) // upload is gone

	// wrong digest is rejected and nothing is stored
	c.expect(c.do("POST", "/v2/app/a/blobs/uploads/?digest="+digestOf([]byte("x")), []byte("y")), 400)

	// blob reads, with range
	if r := c.expect(c.do("GET", "/v2/app/a/blobs/"+d, nil, "Range", "bytes=6-"), 206); readAll(r) != "world" {
		t.Fatal("range read")
	}
	if r := c.expect(c.do("HEAD", "/v2/app/a/blobs/"+d, nil), 200); r.Header.Get("Content-Length") != "11" || r.Header.Get("Docker-Content-Digest") != d {
		t.Fatalf("head: %v", r.Header)
	}

	// blobs are per repo: app/b can't read it until it mounts it
	c.expect(c.do("GET", "/v2/app/b/blobs/"+d, nil), 404)
	c.expect(c.do("POST", "/v2/app/b/blobs/uploads/?mount="+d+"&from=app/a", nil), 201)
	c.expect(c.do("GET", "/v2/app/b/blobs/"+d, nil), 200)
	// mounting from a repo you can't read starts a normal upload instead
	reg.Auth("alice", "secret") // no-op, documents who is calling
	c.expect(c.do("POST", "/v2/app/b/blobs/uploads/?mount="+d+"&from=other/x", nil), 202)

	// manifests: missing blob is rejected, then tag + digest reads
	cfg := pushBlob(c, "app/a", []byte("{}"))
	missing := digestOf([]byte("nope"))
	m := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":"%s","config":{"mediaType":"x","digest":%q,"size":2},"layers":[{"mediaType":"x","digest":%q,"size":1}]}`, ociManifest, cfg, missing)
	c.expect(c.do("PUT", "/v2/app/a/manifests/v1", m), 400)
	md, body := pushManifest(c, "app/a", "v1", cfg, d)
	if r := c.expect(c.do("GET", "/v2/app/a/manifests/v1", nil), 200); readAll(r) != string(body) || r.Header.Get("Content-Type") != ociManifest || r.Header.Get("Docker-Content-Digest") != md {
		t.Fatal("manifest get")
	}
	c.expect(c.do("HEAD", "/v2/app/a/manifests/"+md, nil), 200)
	c.expect(c.do("PUT", "/v2/app/a/manifests/"+digestOf([]byte("other")), body), 400)

	// tags pagination
	for _, tag := range []string{"v2", "v3", "v4"} {
		pushManifest(c, "app/a", tag, cfg, d)
	}
	r := c.expect(c.do("GET", "/v2/app/a/tags/list?n=2", nil), 200)
	if got := readAll(r); !strings.Contains(got, `"v1","v2"]`) || !strings.Contains(r.Header.Get("Link"), "last=v2") {
		t.Fatalf("page1 %s %s", got, r.Header.Get("Link"))
	}
	if got := readAll(c.expect(c.do("GET", "/v2/app/a/tags/list?n=2&last=v2", nil), 200)); !strings.Contains(got, `"v3","v4"]`) {
		t.Fatalf("page2 %s", got)
	}

	// referrers: an artifact whose subject is v1
	art := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":"%s","artifactType":"application/x-sig","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":2},"layers":[],"subject":{"mediaType":"%s","digest":%q,"size":%d}}`, ociManifest, cfg, ociManifest, md, len(body))
	if r := c.expect(c.do("PUT", "/v2/app/a/manifests/"+digestOf(art), art), 201); r.Header.Get("OCI-Subject") != md {
		t.Fatal("OCI-Subject missing")
	}
	if got := readAll(c.expect(c.do("GET", "/v2/app/a/referrers/"+md, nil), 200)); !strings.Contains(got, digestOf(art)) || !strings.Contains(got, "application/x-sig") {
		t.Fatalf("referrers %s", got)
	}

	// catalog is filtered by permission
	c.expect(c.do("GET", "/v2/_catalog", nil), 200)
	admin := client{t, srv.URL, "admin"}
	pushBlob(client{t, srv.URL, "alice"}, "app/zzz", []byte("z"))
	if got := readAll(admin.expect(admin.do("GET", "/v2/_catalog", nil), 200)); !strings.Contains(got, `"app/a","app/b","app/zzz"`) {
		t.Fatalf("catalog %s", got)
	}
	admin.expect(admin.do("DELETE", "/v2/app/a/manifests/v1", nil), 403)
	c.expect(c.do("GET", "/v2/other/x/tags/list", nil), 403)
	resp = c.expect(client{t, srv.URL, "nobody"}.do("GET", "/v2/", nil), 401)
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("no challenge")
	}

	// delete tag, then delete manifest by digest removes remaining tags pointing at it
	c.expect(c.do("DELETE", "/v2/app/a/manifests/v4", nil), 202)
	c.expect(c.do("GET", "/v2/app/a/manifests/v4", nil), 404)
	c.expect(c.do("DELETE", "/v2/app/a/manifests/"+md, nil), 202)
	for _, tag := range []string{"v1", "v2", "v3"} {
		c.expect(c.do("GET", "/v2/app/a/manifests/"+tag, nil), 404)
	}
}

func readAll(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestGC(t *testing.T) {
	reg, srv, _ := testRegistry(t)
	c := client{t, srv.URL, "alice"}
	cfg := pushBlob(c, "app/a", []byte("{}"))
	shared := pushBlob(c, "app/a", []byte("shared layer"))
	old := pushBlob(c, "app/a", []byte("old layer"))
	pushManifest(c, "app/a", "latest", cfg, shared, old)
	newer := pushBlob(c, "app/a", []byte("new layer"))
	pushManifest(c, "app/a", "latest", cfg, shared, newer) // old manifest is now untagged
	stray := pushBlob(c, "app/a", []byte("never referenced"))
	pushBlob(c, "app/gone", []byte("repo with no manifests"))

	// with a long grace nothing is touched
	if res, _ := reg.GC(time.Hour); res.Blobs != 0 || res.Manifests != 0 {
		t.Fatalf("grace ignored: %+v", res)
	}
	res, err := reg.GC(0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifests != 1 || res.Blobs != 4 { // old manifest, old layer, stray, gone's blob
		t.Fatalf("gc: %+v", res)
	}
	for _, d := range []string{old, stray} {
		if exists(reg.blobPath(d)) {
			t.Fatalf("%s survived", d)
		}
	}
	for _, d := range []string{cfg, shared, newer} {
		c.expect(c.do("GET", "/v2/app/a/blobs/"+d, nil), 200)
	}
	c.expect(c.do("GET", "/v2/app/a/manifests/latest", nil), 200)
	if exists(filepath.Join(reg.Root, "repos", "app", "gone")) {
		t.Fatal("empty repo not removed")
	}
	if got := reg.Repos(); len(got) != 1 || got[0] != "app/a" {
		t.Fatalf("repos %v", got)
	}
}

func TestSplitPath(t *testing.T) {
	for p, want := range map[string]string{
		"a/b/blobs/uploads/":         "a/b uploads ",
		"a/b/blobs/uploads":          "a/b uploads ",
		"a/b/blobs/uploads/abc":      "a/b uploads abc",
		"a/blobs/x/manifests/latest": "a/blobs/x manifests latest",
		"a/manifests/sha256:ab":      "a manifests sha256:ab",
		"a/b/tags/list":              "a/b tags ",
		"a/referrers/sha256:ab":      "a referrers sha256:ab",
	} {
		n, k, r, ok := splitPath(p)
		if got := n + " " + k + " " + r; !ok || got != want {
			t.Errorf("%s: got %q want %q", p, got, want)
		}
	}
	if ValidName("a/blobs/x") {
		t.Error("reserved component accepted")
	}
}
