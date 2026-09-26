// Package registry is a small OCI distribution-spec registry on the filesystem.
//
// Layout under root:
//
//	blobs/sha256/<ab>/<hex>        blob content (layers, configs, manifests)
//	repos/<name>/blobs/<hex>       empty: blob is linked to this repo (access control)
//	repos/<name>/manifests/<hex>   content = media type: manifest belongs to this repo
//	repos/<name>/tags/<tag>        content = "sha256:<hex>"
//	uploads/<uuid>                 in-progress upload
//	uploads/<uuid>.repo            repo of that upload
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Perm is what an authenticated caller may do.
type Perm struct {
	Name string
	Pull func(repo string) bool
	Push func(repo string) bool
}

type Registry struct {
	Root string
	// Auth returns nil when credentials are wrong.
	Auth func(user, pass string) *Perm
	// OnPush is called after a tag is written.
	OnPush func(repo, tag, digest string)

	mu sync.Mutex // guards tag/manifest writes against GC
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
	tagRe    = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	uuidRe   = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

const maxManifest = 4 << 20

func New(root string) (*Registry, error) {
	for _, d := range []string{"blobs/sha256", "repos", "uploads"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Registry{Root: root}, nil
}

// ---- errors

type apiErr struct {
	status int
	code   string
	msg    string
}

func (e *apiErr) Error() string { return e.code + ": " + e.msg }

func errf(status int, code, format string, args ...any) *apiErr {
	return &apiErr{status, code, fmt.Sprintf(format, args...)}
}

func writeErr(w http.ResponseWriter, e *apiErr) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	json.MarshalWrite(w, map[string]any{"errors": []map[string]string{{"code": e.code, "message": e.msg}}})
}

// ---- routing

func (reg *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	perm := reg.authenticate(r)
	if perm == nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="vops"`)
		writeErr(w, errf(401, "UNAUTHORIZED", "authentication required"))
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	if p == "" || r.URL.Path == "/v2" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	}
	if p == "_catalog" {
		reg.catalog(w, r, perm)
		return
	}
	name, kind, rest, ok := splitPath(p)
	if !ok || !ValidName(name) {
		writeErr(w, errf(404, "NAME_INVALID", "invalid repository path %q", p))
		return
	}
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	if write && !perm.Push(name) || !write && !perm.Pull(name) {
		writeErr(w, errf(403, "DENIED", "%s may not %s %s", perm.Name, map[bool]string{true: "push to", false: "pull from"}[write], name))
		return
	}
	var err *apiErr
	switch kind {
	case "blobs":
		err = reg.blob(w, r, name, rest)
	case "uploads":
		err = reg.upload(w, r, perm, name, rest)
	case "manifests":
		err = reg.manifest(w, r, name, rest)
	case "tags":
		err = reg.tags(w, r, name)
	case "referrers":
		err = reg.referrers(w, r, name, rest)
	}
	if err != nil {
		writeErr(w, err)
	}
}

// splitPath splits "a/b/blobs/uploads/x" into ("a/b", "uploads", "x").
func splitPath(p string) (name, kind, rest string, ok bool) {
	if n, ok := strings.CutSuffix(p, "/blobs/uploads"); ok {
		return n, "uploads", "", true
	}
	if n, ok := strings.CutSuffix(p, "/blobs/uploads/"); ok {
		return n, "uploads", "", true
	}
	if n, ok := strings.CutSuffix(p, "/tags/list"); ok {
		return n, "tags", "", true
	}
	// the rest never contains a slash, so the right-most marker wins
	best := -1
	for _, k := range []string{"/blobs/uploads/", "/blobs/", "/manifests/", "/referrers/"} {
		if i := strings.LastIndex(p, k); i > 0 && !strings.Contains(p[i+len(k):], "/") && i+len(k) > best {
			best = i + len(k)
			name, kind, rest = p[:i], map[string]string{"/blobs/uploads/": "uploads", "/blobs/": "blobs", "/manifests/": "manifests", "/referrers/": "referrers"}[k], p[i+len(k):]
		}
	}
	return name, kind, rest, best > 0
}

func (reg *Registry) authenticate(r *http.Request) *Perm {
	user, pass, ok := r.BasicAuth()
	if !ok || reg.Auth == nil {
		return nil
	}
	return reg.Auth(user, pass)
}

// ---- paths

func hexOf(digest string) string { return strings.TrimPrefix(digest, "sha256:") }

func (reg *Registry) blobPath(digest string) string {
	h := hexOf(digest)
	return filepath.Join(reg.Root, "blobs", "sha256", h[:2], h)
}

func (reg *Registry) repoPath(name string, parts ...string) string {
	return filepath.Join(append([]string{reg.Root, "repos", filepath.FromSlash(name)}, parts...)...)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func touch(p string, content string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return writeAtomic(p, []byte(content))
}

func writeAtomic(p string, data []byte) error {
	tmp := p + ".tmp" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (reg *Registry) linked(name, digest string) bool {
	return exists(reg.repoPath(name, "blobs", hexOf(digest))) && exists(reg.blobPath(digest))
}

// ---- blobs

func (reg *Registry) blob(w http.ResponseWriter, r *http.Request, name, digest string) *apiErr {
	if !digestRe.MatchString(digest) {
		return errf(400, "DIGEST_INVALID", "invalid digest %q", digest)
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !reg.linked(name, digest) {
			return errf(404, "BLOB_UNKNOWN", "blob %s not in %s", digest, name)
		}
		f, err := os.Open(reg.blobPath(digest))
		if err != nil {
			return errf(404, "BLOB_UNKNOWN", "blob %s", digest)
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Etag", `"`+digest+`"`)
		http.ServeContent(w, r, "", st.ModTime(), f)
		return nil
	case http.MethodDelete:
		if !reg.linked(name, digest) {
			return errf(404, "BLOB_UNKNOWN", "blob %s not in %s", digest, name)
		}
		os.Remove(reg.repoPath(name, "blobs", hexOf(digest)))
		w.WriteHeader(202)
		return nil
	}
	return errf(405, "UNSUPPORTED", "method %s", r.Method)
}

// ---- uploads

func newUUID() string {
	b := make([]byte, 16)
	randRead(b)
	return hex.EncodeToString(b)
}

func (reg *Registry) uploadPath(id string) string { return filepath.Join(reg.Root, "uploads", id) }

func (reg *Registry) uploadAccepted(w http.ResponseWriter, name, id string, size int64) {
	w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Range", fmt.Sprintf("0-%d", max(size-1, 0)))
	w.Header().Set("Content-Length", "0")
}

func (reg *Registry) upload(w http.ResponseWriter, r *http.Request, perm *Perm, name, id string) *apiErr {
	if r.Method == http.MethodPost && id == "" {
		q := r.URL.Query()
		if mount, from := q.Get("mount"), q.Get("from"); mount != "" && from != "" && digestRe.MatchString(mount) {
			if ValidName(from) && perm.Pull(from) && reg.linked(from, mount) {
				if err := touch(reg.repoPath(name, "blobs", hexOf(mount)), ""); err != nil {
					return errf(500, "UNKNOWN", "%v", err)
				}
				w.Header().Set("Location", "/v2/"+name+"/blobs/"+mount)
				w.Header().Set("Docker-Content-Digest", mount)
				w.WriteHeader(201)
				return nil
			}
		}
		id = newUUID()
		if err := os.WriteFile(reg.uploadPath(id), nil, 0o644); err != nil {
			return errf(500, "UNKNOWN", "%v", err)
		}
		os.WriteFile(reg.uploadPath(id)+".repo", []byte(name), 0o644)
		if d := q.Get("digest"); d != "" {
			return reg.finishUpload(w, r, name, id, d)
		}
		reg.uploadAccepted(w, name, id, 0)
		w.WriteHeader(202)
		return nil
	}
	if !uuidRe.MatchString(id) {
		return errf(404, "BLOB_UPLOAD_UNKNOWN", "upload %q", id)
	}
	if repo, err := os.ReadFile(reg.uploadPath(id) + ".repo"); err != nil || string(repo) != name {
		return errf(404, "BLOB_UPLOAD_UNKNOWN", "upload %q", id)
	}
	st, err := os.Stat(reg.uploadPath(id))
	if err != nil {
		return errf(404, "BLOB_UPLOAD_UNKNOWN", "upload %q", id)
	}
	switch r.Method {
	case http.MethodGet:
		reg.uploadAccepted(w, name, id, st.Size())
		w.WriteHeader(204)
		return nil
	case http.MethodDelete:
		os.Remove(reg.uploadPath(id))
		os.Remove(reg.uploadPath(id) + ".repo")
		w.WriteHeader(204)
		return nil
	case http.MethodPatch:
		if cr := r.Header.Get("Content-Range"); cr != "" {
			start, _, _ := strings.Cut(strings.TrimPrefix(cr, "bytes="), "-")
			if n, err := strconv.ParseInt(start, 10, 64); err != nil || n != st.Size() {
				reg.uploadAccepted(w, name, id, st.Size())
				return errf(416, "BLOB_UPLOAD_INVALID", "range %s does not start at %d", cr, st.Size())
			}
		}
		size, err := appendBody(reg.uploadPath(id), r.Body)
		if err != nil {
			return errf(500, "UNKNOWN", "%v", err)
		}
		reg.uploadAccepted(w, name, id, size)
		w.WriteHeader(202)
		return nil
	case http.MethodPut:
		return reg.finishUpload(w, r, name, id, r.URL.Query().Get("digest"))
	}
	return errf(405, "UNSUPPORTED", "method %s", r.Method)
}

func appendBody(path string, body io.Reader) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := io.Copy(f, body); err != nil {
		return 0, err
	}
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), f.Sync()
}

func (reg *Registry) finishUpload(w http.ResponseWriter, r *http.Request, name, id, digest string) *apiErr {
	path := reg.uploadPath(id)
	defer os.Remove(path + ".repo")
	if !digestRe.MatchString(digest) {
		os.Remove(path)
		return errf(400, "DIGEST_INVALID", "invalid digest %q", digest)
	}
	if _, err := appendBody(path, r.Body); err != nil {
		return errf(500, "UNKNOWN", "%v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return errf(500, "UNKNOWN", "%v", err)
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return errf(500, "UNKNOWN", "%v", err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		os.Remove(path)
		return errf(400, "DIGEST_INVALID", "digest mismatch: got %s, want %s", got, digest)
	}
	dst := reg.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return errf(500, "UNKNOWN", "%v", err)
	}
	if err := os.Rename(path, dst); err != nil {
		return errf(500, "UNKNOWN", "%v", err)
	}
	if err := touch(reg.repoPath(name, "blobs", hexOf(digest)), ""); err != nil {
		return errf(500, "UNKNOWN", "%v", err)
	}
	w.Header().Set("Location", "/v2/"+name+"/blobs/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(201)
	return nil
}

// ---- manifests

// manifest is the subset of image manifests and indexes that we look at.
type manifest struct {
	MediaType string       `json:"mediaType"`
	Config    *descriptor  `json:"config"`
	Layers    []descriptor `json:"layers"`
	Manifests []descriptor `json:"manifests"`
	Subject   *descriptor  `json:"subject"`
	// ArtifactType is returned by the referrers API.
	ArtifactType string            `json:"artifactType"`
	Annotations  map[string]string `json:"annotations"`
}

type descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Platform     *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform,omitempty"`
}

const ociManifest = "application/vnd.oci.image.manifest.v1+json"

func parseManifest(b []byte) (manifest, error) {
	var m manifest
	err := json.Unmarshal(b, &m, json.RejectUnknownMembers(false))
	return m, err
}

// resolve turns a tag or digest into a digest that belongs to the repo.
func (reg *Registry) resolve(name, ref string) (digest, mediaType string, err *apiErr) {
	digest = ref
	if !digestRe.MatchString(ref) {
		if !tagRe.MatchString(ref) {
			return "", "", errf(400, "MANIFEST_INVALID", "invalid reference %q", ref)
		}
		b, e := os.ReadFile(reg.repoPath(name, "tags", ref))
		if e != nil {
			return "", "", errf(404, "MANIFEST_UNKNOWN", "%s:%s not found", name, ref)
		}
		digest = strings.TrimSpace(string(b))
	}
	mt, e := os.ReadFile(reg.repoPath(name, "manifests", hexOf(digest)))
	if e != nil || !exists(reg.blobPath(digest)) {
		return "", "", errf(404, "MANIFEST_UNKNOWN", "%s@%s not found", name, digest)
	}
	return digest, string(mt), nil
}

func (reg *Registry) manifest(w http.ResponseWriter, r *http.Request, name, ref string) *apiErr {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		digest, mt, err := reg.resolve(name, ref)
		if err != nil {
			return err
		}
		b, e := os.ReadFile(reg.blobPath(digest))
		if e != nil {
			return errf(404, "MANIFEST_UNKNOWN", "%s", digest)
		}
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Etag", `"`+digest+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		if r.Method == http.MethodGet {
			w.Write(b)
		}
		return nil
	case http.MethodPut:
		return reg.putManifest(w, r, name, ref)
	case http.MethodDelete:
		reg.mu.Lock()
		defer reg.mu.Unlock()
		if !digestRe.MatchString(ref) {
			if _, _, err := reg.resolve(name, ref); err != nil {
				return err
			}
			os.Remove(reg.repoPath(name, "tags", ref))
			w.WriteHeader(202)
			return nil
		}
		if _, _, err := reg.resolve(name, ref); err != nil {
			return err
		}
		os.Remove(reg.repoPath(name, "manifests", hexOf(ref)))
		for tag, d := range reg.tagMap(name) {
			if d == ref {
				os.Remove(reg.repoPath(name, "tags", tag))
			}
		}
		w.WriteHeader(202)
		return nil
	}
	return errf(405, "UNSUPPORTED", "method %s", r.Method)
}

func (reg *Registry) putManifest(w http.ResponseWriter, r *http.Request, name, ref string) *apiErr {
	body, e := io.ReadAll(io.LimitReader(r.Body, maxManifest+1))
	if e != nil {
		return errf(400, "MANIFEST_INVALID", "%v", e)
	}
	if len(body) > maxManifest {
		return errf(413, "SIZE_INVALID", "manifest over %d bytes", maxManifest)
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	isDigest := digestRe.MatchString(ref)
	if isDigest && ref != digest {
		return errf(400, "DIGEST_INVALID", "body is %s, url says %s", digest, ref)
	}
	if !isDigest && !tagRe.MatchString(ref) {
		return errf(400, "TAG_INVALID", "invalid tag %q", ref)
	}
	m, err := parseManifest(body)
	if err != nil {
		return errf(400, "MANIFEST_INVALID", "%v", err)
	}
	mt := m.MediaType
	if mt == "" {
		mt, _, _ = strings.Cut(r.Header.Get("Content-Type"), ";")
	}
	if mt == "" {
		mt = ociManifest
	}
	for _, d := range m.refs() {
		if !digestRe.MatchString(d.Digest) {
			return errf(400, "MANIFEST_INVALID", "invalid digest %q", d.Digest)
		}
		if isIndexChild(m, d) {
			if !exists(reg.repoPath(name, "manifests", hexOf(d.Digest))) {
				return errf(400, "MANIFEST_BLOB_UNKNOWN", "manifest %s not in %s", d.Digest, name)
			}
		} else if !reg.linked(name, d.Digest) {
			return errf(400, "MANIFEST_BLOB_UNKNOWN", "blob %s not in %s", d.Digest, name)
		}
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	dst := reg.blobPath(digest)
	if !exists(dst) {
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := writeAtomic(dst, body); err != nil {
			return errf(500, "UNKNOWN", "%v", err)
		}
	}
	if err := touch(reg.repoPath(name, "manifests", hexOf(digest)), mt); err != nil {
		return errf(500, "UNKNOWN", "%v", err)
	}
	if !isDigest {
		if err := touch(reg.repoPath(name, "tags", ref), digest); err != nil {
			return errf(500, "UNKNOWN", "%v", err)
		}
	}
	if m.Subject != nil {
		w.Header().Set("OCI-Subject", m.Subject.Digest)
	}
	w.Header().Set("Location", "/v2/"+name+"/manifests/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(201)
	if !isDigest && reg.OnPush != nil {
		go reg.OnPush(name, ref, digest)
	}
	return nil
}

// refs lists everything a manifest points to (not the subject: it may be pushed later).
func (m manifest) refs() []descriptor {
	var out []descriptor
	if m.Config != nil {
		out = append(out, *m.Config)
	}
	out = append(out, m.Layers...)
	return append(out, m.Manifests...)
}

func isIndexChild(m manifest, d descriptor) bool {
	return slices.ContainsFunc(m.Manifests, func(x descriptor) bool { return x.Digest == d.Digest })
}

// ---- tags, catalog, referrers

func (reg *Registry) tagMap(name string) map[string]string {
	out := map[string]string{}
	entries, _ := os.ReadDir(reg.repoPath(name, "tags"))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			continue
		}
		b, err := os.ReadFile(reg.repoPath(name, "tags", e.Name()))
		if err == nil {
			out[e.Name()] = strings.TrimSpace(string(b))
		}
	}
	return out
}

func paginate(w http.ResponseWriter, r *http.Request, items []string) []string {
	slices.Sort(items)
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		i, _ := slices.BinarySearch(items, last)
		for i < len(items) && items[i] <= last {
			i++
		}
		items = items[i:]
	}
	if n, err := strconv.Atoi(q.Get("n")); err == nil && n >= 0 && n < len(items) {
		items = items[:n]
		if n > 0 {
			u := *r.URL
			v := u.Query()
			v.Set("last", items[n-1])
			u.RawQuery = v.Encode()
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, u.RequestURI()))
		}
	}
	return items
}

func (reg *Registry) tags(w http.ResponseWriter, r *http.Request, name string) *apiErr {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return errf(405, "UNSUPPORTED", "method %s", r.Method)
	}
	if !exists(reg.repoPath(name)) {
		return errf(404, "NAME_UNKNOWN", "repository %s", name)
	}
	tags := []string{}
	for t := range reg.tagMap(name) {
		tags = append(tags, t)
	}
	tags = paginate(w, r, tags)
	w.Header().Set("Content-Type", "application/json")
	json.MarshalWrite(w, map[string]any{"name": name, "tags": tags})
	return nil
}

func (reg *Registry) catalog(w http.ResponseWriter, r *http.Request, perm *Perm) {
	repos := []string{}
	for _, name := range reg.Repos() {
		if perm.Pull(name) {
			repos = append(repos, name)
		}
	}
	repos = paginate(w, r, repos)
	w.Header().Set("Content-Type", "application/json")
	json.MarshalWrite(w, map[string]any{"repositories": repos})
}

func (reg *Registry) referrers(w http.ResponseWriter, r *http.Request, name, digest string) *apiErr {
	if !digestRe.MatchString(digest) {
		return errf(400, "DIGEST_INVALID", "invalid digest %q", digest)
	}
	filter := r.URL.Query().Get("artifactType")
	out := []descriptor{}
	entries, _ := os.ReadDir(reg.repoPath(name, "manifests"))
	for _, e := range entries {
		d := "sha256:" + e.Name()
		if !digestRe.MatchString(d) {
			continue
		}
		b, err := os.ReadFile(reg.blobPath(d))
		if err != nil {
			continue
		}
		m, err := parseManifest(b)
		if err != nil || m.Subject == nil || m.Subject.Digest != digest {
			continue
		}
		at := m.ArtifactType
		if at == "" && m.Config != nil {
			at = m.Config.MediaType
		}
		if filter != "" && at != filter {
			continue
		}
		mt, _ := os.ReadFile(reg.repoPath(name, "manifests", e.Name()))
		out = append(out, descriptor{MediaType: string(mt), Digest: d, Size: int64(len(b)), ArtifactType: at, Annotations: m.Annotations})
	}
	w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	if filter != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	json.MarshalWrite(w, map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": out})
	return nil
}

// ---- helpers used by the daemon and UI

// ValidName reports whether name is a valid repository name that does not clash with the storage layout.
func ValidName(name string) bool {
	if !nameRe.MatchString(name) || len(name) > 255 {
		return false
	}
	for c := range strings.SplitSeq(name, "/") {
		if c == "blobs" || c == "manifests" || c == "tags" {
			return false
		}
	}
	return true
}

// Repos lists every repository name.
func (reg *Registry) Repos() []string {
	var out []string
	base := filepath.Join(reg.Root, "repos")
	filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		switch d.Name() {
		case "blobs", "manifests", "tags":
			if rel, err := filepath.Rel(base, filepath.Dir(p)); err == nil && !slices.Contains(out, filepath.ToSlash(rel)) {
				out = append(out, filepath.ToSlash(rel))
			}
			return filepath.SkipDir
		}
		return nil
	})
	slices.Sort(out)
	return out
}

type Tag struct {
	Name     string    `json:"name"`
	Digest   string    `json:"digest"`
	Size     int64     `json:"size"`
	PushedAt time.Time `json:"pushed_at"`
}

// Tags lists tags of a repo with their size (sum of config and layers, or of children for indexes).
func (reg *Registry) Tags(name string) []Tag {
	out := []Tag{}
	for tag, digest := range reg.tagMap(name) {
		t := Tag{Name: tag, Digest: digest, Size: reg.imageSize(digest, 0)}
		if st, err := os.Stat(reg.repoPath(name, "tags", tag)); err == nil {
			t.PushedAt = st.ModTime()
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b Tag) int { return b.PushedAt.Compare(a.PushedAt) })
	return out
}

func (reg *Registry) imageSize(digest string, depth int) int64 {
	b, err := os.ReadFile(reg.blobPath(digest))
	if err != nil || depth > 2 {
		return 0
	}
	m, err := parseManifest(b)
	if err != nil {
		return 0
	}
	size := int64(len(b))
	if m.Config != nil {
		size += m.Config.Size
	}
	for _, l := range m.Layers {
		size += l.Size
	}
	for _, c := range m.Manifests {
		size += reg.imageSize(c.Digest, depth+1)
	}
	return size
}

// Resolve returns the digest of repo:tag, or "" if it does not exist.
func (reg *Registry) Resolve(name, tag string) string {
	if !nameRe.MatchString(name) {
		return ""
	}
	d, _, err := reg.resolve(name, tag)
	if err != nil {
		return ""
	}
	return d
}

// DeleteTag removes a tag; GC removes the data later.
func (reg *Registry) DeleteTag(name, tag string) error {
	if !nameRe.MatchString(name) || !tagRe.MatchString(tag) {
		return errors.New("invalid name or tag")
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return os.Remove(reg.repoPath(name, "tags", tag))
}
