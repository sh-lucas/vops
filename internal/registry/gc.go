package registry

import (
	"crypto/rand"
	"errors"
	"io/fs"
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

// GC deletes untagged manifests (unless an index or a kept subject references them, or their digest is in keep:
// what pins, previews and recent deploys run), blobs nothing references, and uploads older than a day.
// Anything younger than grace is kept, so a push in progress is safe.
func (reg *Registry) GC(grace time.Duration, keepDigests map[string]bool) (GCResult, error) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	var res GCResult
	cutoff := time.Now().Add(-grace)
	young := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && st.ModTime().After(cutoff)
	}

	// a read that fails (EIO, EMFILE) stops GC before the blob sweep: deleting what it couldn't read loses images
	keepBlobs := map[string]bool{}
	repos, err := reg.repos()
	if err != nil {
		return res, err
	}
	for _, name := range repos {
		// mark: tagged manifests, recent manifests, their children, and referrers of anything kept
		all := map[string]manifest{}
		entries, err := os.ReadDir(reg.repoPath(name, "manifests"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
		for _, e := range entries {
			d := "sha256:" + e.Name()
			if !digestRe.MatchString(d) {
				continue
			}
			b, err := os.ReadFile(reg.blobPath(d))
			if errors.Is(err, fs.ErrNotExist) {
				os.Remove(reg.repoPath(name, "manifests", e.Name()))
				continue
			}
			if err != nil {
				return res, err
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
		tags, err := reg.readTags(name)
		if err != nil {
			return res, err
		}
		for tag, d := range tags {
			if _, ok := all[d]; !ok {
				os.Remove(reg.repoPath(name, "tags", tag))
				continue
			}
			visit(d)
		}
		for d := range all {
			if keepDigests[d] || young(reg.repoPath(name, "manifests", hexOf(d))) {
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
		links, err := os.ReadDir(reg.repoPath(name, "blobs"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
		used := map[string]bool{}
		for d := range keep {
			used[hexOf(d)] = true
			for _, x := range all[d].refs() {
				used[hexOf(x.Digest)] = true
			}
		}
		fresh := false // a push in progress: its repo stays even with no tag yet
		for _, l := range links {
			p := reg.repoPath(name, "blobs", l.Name())
			if used[l.Name()] {
				continue
			}
			if young(p) {
				keepBlobs[l.Name()] = true
				fresh = true
				continue
			}
			os.Remove(p)
		}
		// temp files a crash left behind
		for _, dir := range []string{"tags", "manifests"} {
			entries, _ := os.ReadDir(reg.repoPath(name, dir))
			for _, e := range entries {
				if p := reg.repoPath(name, dir, e.Name()); strings.HasPrefix(e.Name(), ".") && !young(p) {
					os.Remove(p)
				}
			}
		}
		if len(keep) == 0 && !fresh && !young(reg.repoPath(name)) {
			left, err := reg.readTags(name)
			if err != nil {
				return res, err
			}
			if len(left) == 0 {
				reg.removeRepo(name)
			}
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
			reg.uploading.Delete(strings.TrimSuffix(e.Name(), ".repo"))
		}
	}
	return res, nil
}

// removeRepo deletes the repo's own dirs, then the repo dir and its parents up to repos/ while they are empty:
// a nested repo (shop/web under shop) stays.
func (reg *Registry) removeRepo(name string) {
	p := reg.repoPath(name)
	for _, d := range []string{"blobs", "manifests", "tags"} {
		os.RemoveAll(filepath.Join(p, d))
	}
	if os.Remove(p) != nil {
		return
	}
	base := filepath.Join(reg.Root, "repos")
	for dir := filepath.Dir(p); strings.HasPrefix(dir, base+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
}
