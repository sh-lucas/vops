package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/snapshot"
	"github.com/sh-lucas/vops/internal/store"
)

// Backups are snapshots as files: a .tar.gz with vops-backup.json first, then one top-level dir per volume.
// Files keep their numeric owners, modes and xattrs: the volumes are read and written by GNU tar inside
// `podman unshare` (where the subuids that own them are plain uids), and Go only rewraps the stream, so
// nothing is buffered and every entry is checked before it reaches the disk.

const ManifestName = "vops-backup.json"

type BackupManifest struct {
	Format    int            `json:"format"` // 1
	Project   string         `json:"project"`
	Snapshot  int64          `json:"snapshot"`
	Reason    string         `json:"reason"`
	Note      string         `json:"note"`
	Commit    string         `json:"commit"`
	CreatedAt int64          `json:"created_at"`
	Vops      string         `json:"vops"`
	Volumes   []BackupVolume `json:"volumes"`
}

type BackupVolume struct {
	Kind string `json:"kind"` // volume | bind
	Name string `json:"name"` // podman volume name, or bind path relative to the repo
	Dir  string `json:"dir"`  // its top-level dir in the archive
}

// ExportSnapshot writes snapshot id as a .tar.gz to w.
func (e *Engine) ExportSnapshot(ctx context.Context, w io.Writer, id int64, version string) error {
	s, err := e.DB.Snapshot(id)
	if err != nil {
		return err
	}
	return exportSnapshot(ctx, w, s, version)
}

// BackupCheck reports whether snapshot id can be exported (before a response commits to streaming it).
func (e *Engine) BackupCheck(id int64) (store.Snapshot, error) {
	s, err := e.DB.Snapshot(id)
	if err != nil {
		return s, err
	}
	for _, v := range s.Volumes {
		if !snapshot.IsSubvolume(v.Path) {
			return s, fmt.Errorf("snapshot #%d: %s is missing on disk", id, v.Name)
		}
	}
	return s, nil
}

func exportSnapshot(ctx context.Context, w io.Writer, s store.Snapshot, version string) error {
	m := BackupManifest{Format: 1, Project: s.Project, Snapshot: s.ID, Reason: s.Reason, Note: s.Note, Commit: s.Commit, CreatedAt: s.CreatedAt, Vops: version}
	for i, v := range s.Volumes {
		m.Volumes = append(m.Volumes, BackupVolume{v.Kind, v.Name, volumeDir(i, v.Name)})
	}
	b, _ := json.Marshal(m, json.Deterministic(true))
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(b)), ModTime: time.Now(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	for i, v := range s.Volumes {
		if err := exportVolume(ctx, tw, s.Volumes[i].Path, m.Volumes[i].Dir); err != nil {
			return fmt.Errorf("%s: %w", v.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func volumeDir(i int, name string) string {
	return strconv.Itoa(i) + "-" + unsafeName.ReplaceAllString(name, "_")
}

// exportVolume copies GNU tar's archive of src into tw under dir/.
func exportVolume(ctx context.Context, tw *tar.Writer, src, dir string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := snapshot.Command(ctx, "tar", "-cf", "-", "--numeric-owner", "--xattrs", "--xattrs-include=*", "-C", src, ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	err = copyEntries(tar.NewReader(out), tw, func(name string) string { return path.Join(dir, name) })
	if err != nil {
		cancel()
		cmd.Wait()
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("tar: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// copyEntries copies every entry of r to w, renaming them (and hard link targets) with rename.
func copyEntries(r *tar.Reader, w *tar.Writer, rename func(string) string) error {
	for {
		h, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		isDir := h.Typeflag == tar.TypeDir
		h.Name = rename(path.Clean(strings.TrimPrefix(h.Name, "./")))
		if isDir && !strings.HasSuffix(h.Name, "/") {
			h.Name += "/"
		}
		if h.Typeflag == tar.TypeLink {
			h.Linkname = rename(path.Clean(strings.TrimPrefix(h.Linkname, "./")))
		}
		h.Format = tar.FormatPAX
		if err := w.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(w, r); err != nil {
			return err
		}
	}
}

var volumeDirRe = regexp.MustCompile(`^[0-9]+-[A-Za-z0-9._-]+$`)

// readBackup reads and checks a backup as it streams. check validates the manifest (before any data);
// open is called once per manifest volume, in archive order (volumes without entries last), and gets that
// volume's entries as a tar stream with names relative to it; its wait reports how writing them went.
// Refused: a missing or late manifest, absolute paths, "..", entries of unknown dirs or not grouped by volume,
// devices and fifos, hard links leaving their volume, and anything under a symlink of the archive.
func readBackup(r io.Reader, check func(BackupManifest) error, open func(BackupVolume) (io.WriteCloser, func() error, error)) (BackupManifest, error) {
	var m BackupManifest
	gz, err := gzip.NewReader(r)
	if err != nil {
		return m, fmt.Errorf("not a .tar.gz: %w", err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != ManifestName || h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
		return m, errors.New("not a vops backup: " + ManifestName + " must be its first entry")
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", ManifestName, err)
	}
	if m.Format != 1 {
		return m, fmt.Errorf("%s: format %d, this vops reads format 1", ManifestName, m.Format)
	}
	if len(m.Volumes) == 0 {
		return m, fmt.Errorf("%s: no volumes", ManifestName)
	}
	byDir := map[string]BackupVolume{}
	names := map[string]bool{}
	for _, v := range m.Volumes {
		if !volumeDirRe.MatchString(v.Dir) || byDir[v.Dir].Dir != "" || names[v.Name] || v.Name == "" || v.Kind != "volume" && v.Kind != "bind" {
			return m, fmt.Errorf("%s: bad volume %+v", ManifestName, v)
		}
		byDir[v.Dir], names[v.Name] = v, true
	}
	if err := check(m); err != nil {
		return m, err
	}

	var (
		cur   string
		tw    *tar.Writer
		pipe  io.WriteCloser
		wait  func() error
		done  = map[string]bool{}
		links = map[string]bool{} // symlinks of the current volume
	)
	finish := func() error {
		if pipe == nil {
			return nil
		}
		err := tw.Close()
		if cerr := pipe.Close(); err == nil {
			err = cerr
		}
		if werr := wait(); err == nil {
			err = werr
		}
		pipe, done[cur] = nil, true
		return err
	}
	fail := func(err error) (BackupManifest, error) {
		if pipe != nil {
			pipe.Close()
			wait()
		}
		return m, err
	}
	start := func(v BackupVolume) error {
		var err error
		if pipe, wait, err = open(v); err != nil {
			pipe = nil
			return err
		}
		cur, tw, links = v.Dir, tar.NewWriter(pipe), map[string]bool{}
		return nil
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if strings.HasPrefix(name, "/") || slices.Contains(strings.Split(name, "/"), "..") {
			return fail(fmt.Errorf("%q: absolute paths and .. are refused", h.Name))
		}
		dir, rel, _ := strings.Cut(path.Clean(name), "/")
		v, ok := byDir[dir]
		if !ok {
			return fail(fmt.Errorf("%q: not in a volume of %s", h.Name, ManifestName))
		}
		if rel == "" {
			rel = "."
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		case tar.TypeLink:
			ldir, lrel, _ := strings.Cut(path.Clean(h.Linkname), "/")
			if ldir != dir || lrel == "" || strings.HasPrefix(h.Linkname, "/") || slices.Contains(strings.Split(h.Linkname, "/"), "..") || underLink(links, lrel) {
				return fail(fmt.Errorf("%q: hard link to %q leaves its volume", h.Name, h.Linkname))
			}
			h.Linkname = lrel
		default:
			return fail(fmt.Errorf("%q: %s are refused (only files, dirs and links)", h.Name, typeName(h.Typeflag)))
		}
		if dir != cur {
			if done[dir] {
				return fail(fmt.Errorf("%q: the entries of %s are not together", h.Name, dir))
			}
			if err := finish(); err != nil {
				return fail(err)
			}
			if err := start(v); err != nil {
				return fail(err)
			}
		}
		if underLink(links, rel) || links[rel] {
			return fail(fmt.Errorf("%q: goes through or replaces a symlink of the archive", h.Name))
		}
		if h.Typeflag == tar.TypeSymlink {
			links[rel] = true
		}
		h.Name = rel
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		h.Format = tar.FormatPAX
		if err := tw.WriteHeader(h); err != nil {
			return fail(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return fail(err)
		}
	}
	if err := finish(); err != nil {
		return fail(err)
	}
	for _, v := range m.Volumes {
		if !done[v.Dir] {
			if err := start(v); err != nil {
				return fail(err)
			}
			if err := finish(); err != nil {
				return fail(err)
			}
		}
	}
	return m, nil
}

// underLink reports whether rel is inside a symlink recorded in links.
func underLink(links map[string]bool, rel string) bool {
	for p := path.Dir(rel); p != "." && p != "/"; p = path.Dir(p) {
		if links[p] {
			return true
		}
	}
	return false
}

func typeName(t byte) string {
	switch t {
	case tar.TypeChar, tar.TypeBlock:
		return "device files"
	case tar.TypeFifo:
		return "fifos"
	}
	return "entries of type " + strconv.QuoteRune(rune(t))
}

// ImportSnapshot reads a backup into a new read-only snapshot of project (reason upload). Its volumes must
// be part of the project's data as snapshots see it (a subset is fine: restoring only puts those back).
// A failed import leaves nothing behind.
func (e *Engine) ImportSnapshot(ctx context.Context, w io.Writer, project string, r io.Reader) (store.Snapshot, error) {
	if !e.snapshotsOn() || !snapshot.Supported(e.SnapshotDir) {
		return store.Snapshot{}, errors.New("backups are snapshots: they need btrfs (and snapshots on in vops.yml)")
	}
	vols, _, _, err := e.projectData(ctx, project)
	if err != nil {
		return store.Snapshot{}, err
	}
	have := map[string]store.SnapshotVolume{}
	for _, v := range vols {
		have[v.Name] = v
	}
	dir := filepath.Join(e.SnapshotDir, compose.Slug(project), time.Now().UTC().Format("20060102-150405")+"-"+randHex(2)+"-upload")
	s := store.Snapshot{Project: project, Reason: "upload"}
	check := func(m BackupManifest) error {
		for _, v := range m.Volumes {
			if hv, ok := have[v.Name]; !ok || hv.Kind != v.Kind {
				return fmt.Errorf("the backup's %s %s is not data of %s (it has: %s; a backup restores into the project it came from)", v.Kind, v.Name, project, names(vols))
			}
		}
		return nil
	}
	open := func(v BackupVolume) (io.WriteCloser, func() error, error) {
		p := filepath.Join(dir, volumeDir(len(s.Volumes), v.Name))
		if err := snapshot.Create(ctx, p); err != nil {
			return nil, nil, err
		}
		s.Volumes = append(s.Volumes, store.SnapshotVolume{Kind: v.Kind, Name: v.Name, Source: have[v.Name].Source, Path: p})
		cmd := snapshot.Command(ctx, "tar", "-xf", "-", "--numeric-owner", "--xattrs", "--xattrs-include=*", "-C", p)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return in, func() error {
			if err := cmd.Wait(); err != nil {
				return fmt.Errorf("extracting %s: %v: %s", v.Name, err, strings.TrimSpace(stderr.String()))
			}
			return nil
		}, nil
	}
	m, err := readBackup(r, check, open)
	if err == nil {
		for _, v := range s.Volumes {
			if err = snapshot.ReadOnly(ctx, v.Path); err != nil {
				break
			}
		}
	}
	if err == nil {
		s.Commit = m.Commit
		s.Note = fmt.Sprintf("backup of %s #%d (%s) from %s", m.Project, m.Snapshot, m.Reason, time.Unix(m.CreatedAt, 0).UTC().Format("2006-01-02 15:04 UTC"))
		s.ID, err = e.DB.AddSnapshot(s)
	}
	if err != nil {
		snapshot.Delete(context.WithoutCancel(ctx), dir)
		return store.Snapshot{}, err
	}
	fmt.Fprintf(w, "%s: snapshot #%d (upload) of %s\n", project, s.ID, names(s.Volumes))
	e.DB.Event(project, "snapshot", "#%d upload: %s", s.ID, s.Note)
	return s, nil
}
