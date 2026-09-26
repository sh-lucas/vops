package registry

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func randRead(b []byte) { rand.Read(b) }

type GCResult struct {
	Manifests int   `json:"manifests"`
	Blobs     int   `json:"blobs"`
	Freed     int64 `json:"freed"`
}

// GC deletes untagged manifests (unless an index or a kept subject references them), blobs nothing
// references, and uploads older than a day. Anything younger than grace is kept, so a push in progress is safe.
func (reg *Registry) GC(grace time.Duration) (GCResult, error) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	var res GCResult
	cutoff := time.Now().Add(-grace)
	young := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && st.ModTime().After(cutoff)
	}

	keepBlobs := map[string]bool{}
	for _, name := range reg.Repos() {
		// mark: tagged manifests, recent manifests, their children, and referrers of anything kept
		all := map[string]manifest{}
		entries, _ := os.ReadDir(reg.repoPath(name, "manifests"))
		for _, e := range entries {
			d := "sha256:" + e.Name()
			if !digestRe.MatchString(d) {
				continue
			}
			b, err := os.ReadFile(reg.blobPath(d))
			if err != nil {
				os.Remove(reg.repoPath(name, "manifests", e.Name()))
				continue
			}
			m, _ := parseManifest(b)
			all[d] = m
		}
		keep := map[string]bool{}
		var visit func(d string)
		visit = func(d string) {
			m, ok := all[d]
			if !ok || keep[d] {
				return
			}
			keep[d] = true
			for _, c := range m.Manifests {
				visit(c.Digest)
			}
		}
		for tag, d := range reg.tagMap(name) {
			if _, ok := all[d]; !ok {
				os.Remove(reg.repoPath(name, "tags", tag))
				continue
			}
			visit(d)
		}
		for d := range all {
			if young(reg.repoPath(name, "manifests", hexOf(d))) {
				visit(d)
			}
		}
		for changed := true; changed; {
			changed = false
			for d, m := range all {
				if !keep[d] && m.Subject != nil && keep[m.Subject.Digest] {
					visit(d)
					changed = true
				}
			}
		}
		for d, m := range all {
			if !keep[d] {
				os.Remove(reg.repoPath(name, "manifests", hexOf(d)))
				res.Manifests++
				continue
			}
			keepBlobs[hexOf(d)] = true
			for _, x := range m.refs() {
				keepBlobs[hexOf(x.Digest)] = true
			}
		}
		// unlink blobs from the repo that none of its manifests use (unless recently uploaded)
		links, _ := os.ReadDir(reg.repoPath(name, "blobs"))
		used := map[string]bool{}
		for d := range keep {
			used[hexOf(d)] = true
			for _, x := range all[d].refs() {
				used[hexOf(x.Digest)] = true
			}
		}
		for _, l := range links {
			p := reg.repoPath(name, "blobs", l.Name())
			if used[l.Name()] {
				continue
			}
			if young(p) {
				keepBlobs[l.Name()] = true
				continue
			}
			os.Remove(p)
		}
		if len(keep) == 0 && len(reg.tagMap(name)) == 0 && !young(reg.repoPath(name)) {
			reg.removeRepo(name)
		}
	}

	// sweep blobs
	filepath.WalkDir(filepath.Join(reg.Root, "blobs", "sha256"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || keepBlobs[d.Name()] || young(p) {
			return nil
		}
		if st, err := d.Info(); err == nil {
			res.Freed += st.Size()
		}
		if os.Remove(p) == nil {
			res.Blobs++
		}
		return nil
	})

	// stale uploads
	entries, _ := os.ReadDir(filepath.Join(reg.Root, "uploads"))
	for _, e := range entries {
		p := filepath.Join(reg.Root, "uploads", e.Name())
		if st, err := e.Info(); err == nil && time.Since(st.ModTime()) > 24*time.Hour {
			os.Remove(p)
		}
	}
	return res, nil
}

// removeRepo deletes the repo dir and empty parents up to repos/.
func (reg *Registry) removeRepo(name string) {
	p := reg.repoPath(name)
	os.RemoveAll(p)
	base := filepath.Join(reg.Root, "repos")
	for dir := filepath.Dir(p); strings.HasPrefix(dir, base+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
}
