package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/snapshot"
	"github.com/sh-lucas/vops/internal/store"
)

// Data safety: every named volume and every bind mount inside the project dir is created as a btrfs
// subvolume, so it can be snapshotted in O(1). Before any deploy that changes a project, its data is
// snapshotted ("pre-deploy"); `vops rollback` puts a snapshot back (rollback.go).
//
// The same subvolumes, copied writable into another project's volumes, are the data of previews (preview.go).

// DefaultSnapshotKeep is how many automatic snapshots per project are kept (manual ones are kept until deleted).
const DefaultSnapshotKeep = 5

var errNoData = errors.New("nothing to snapshot")

func (e *Engine) snapshotsOn() bool { return !e.SnapshotsOff && e.SnapshotDir != "" }

func (e *Engine) keep() int {
	if e.SnapshotKeep > 0 {
		return e.SnapshotKeep
	}
	return DefaultSnapshotKeep
}

// ensureData creates the volumes and bind dirs of a service, as subvolumes when possible.
func (e *Engine) ensureData(ctx context.Context, log func(string, ...any), sp *compose.Spec) error {
	for _, v := range sp.Volumes {
		if _, err := podman.Run(ctx, "volume", "exists", v); err != nil {
			if _, err := podman.Run(ctx, "volume", "create", "--label", LProject+"="+sp.Project, v); err != nil {
				return err
			}
		}
		if e.snapshotsOn() {
			if _, err := snapshot.EnsureSubvolume(ctx, podman.VolumePath(ctx, v)); err != nil {
				log("volume %s: can't make it snapshottable: %v", v, err)
			}
		}
	}
	for _, b := range sp.Binds {
		if st, err := os.Stat(b); err == nil && !st.IsDir() {
			continue // a bind-mounted file (config, socket): nothing to create
		}
		if e.snapshotsOn() && within(b, sp.Dir) {
			if _, err := snapshot.EnsureSubvolume(ctx, b); err != nil {
				log("%s: can't make it snapshottable: %v", b, err)
			}
		}
		if err := os.MkdirAll(b, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// DataState is what the dashboard and `vops snapshot ls` show about a project's data.
type DataState struct {
	Supported   bool     `json:"supported"`
	Reason      string   `json:"reason,omitempty"`      // why snapshots are unavailable
	Protected   []string `json:"protected"`             // data covered by snapshots
	Unprotected []string `json:"unprotected,omitempty"` // data that can't be snapshotted (not a subvolume)
}

// projectData finds a project's writable data: volumes and binds inside the project dir, from the
// running definition (container mounts) and the desired one (compose). Only subvolumes can be snapshotted.
func (e *Engine) projectData(ctx context.Context, project string) (vols []store.SnapshotVolume, unprotected []string, users []string, err error) {
	seen := map[string]bool{}
	projectDir := filepath.Join(e.Repo, filepath.FromSlash(project))
	add := func(kind, name, source string) {
		if source == "" || seen[source] {
			return
		}
		seen[source] = true
		if !snapshot.IsSubvolume(source) {
			unprotected = append(unprotected, name)
			return
		}
		vols = append(vols, store.SnapshotVolume{Kind: kind, Name: name, Source: source})
	}
	bindName := func(src string) string {
		rel, _ := filepath.Rel(e.Repo, src)
		return filepath.ToSlash(rel)
	}
	cs, err := podman.PS(ctx, LProject+"="+project)
	if err != nil {
		return nil, nil, nil, err
	}
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	mounts, err := podman.Mounts(ctx, ids...)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, c := range cs {
		uses := false
		for _, m := range mounts[c.ID] {
			switch {
			case !m.RW:
			case m.Type == "volume" && m.Name != "":
				add("volume", m.Name, m.Source)
				uses = true
			case m.Type == "bind" && within(m.Source, projectDir):
				add("bind", bindName(m.Source), m.Source)
				uses = true
			}
		}
		if uses && c.State == "running" {
			users = append(users, c.ID)
		}
	}
	// desired data that no container mounts right now (disabled project, stopped service)
	if plan, err := e.Plan(ctx); err == nil {
		for _, pp := range plan.Projects {
			if pp.Path != project {
				continue
			}
			for _, d := range pp.specs {
				for _, v := range d.spec.Volumes {
					add("volume", v, podman.VolumePath(ctx, v))
				}
				for _, b := range d.spec.Binds {
					if within(b, projectDir) {
						if _, err := os.Stat(b); err == nil {
							add("bind", bindName(b), b)
						}
					}
				}
			}
		}
	}
	slices.SortFunc(vols, func(a, b store.SnapshotVolume) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(unprotected)
	return vols, unprotected, users, nil
}

// DataState reports whether a project's data can be snapshotted.
func (e *Engine) DataState(ctx context.Context, project string) DataState {
	st := DataState{Supported: e.snapshotsOn() && snapshot.Supported(e.SnapshotDir), Protected: []string{}}
	switch {
	case e.SnapshotsOff:
		st.Reason = "snapshots are off (snapshots: off in vops.yml)"
	case !st.Supported:
		st.Reason = "snapshots need btrfs (and btrfs-progs) under " + e.SnapshotDir
	}
	vols, unprotected, _, err := e.projectData(ctx, project)
	if err != nil {
		st.Reason = err.Error()
	}
	for _, v := range vols {
		st.Protected = append(st.Protected, v.Name)
	}
	st.Unprotected = unprotected
	return st
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// snap takes read-only snapshots of vols as one snapshot. Running containers using them are paused
// meanwhile (milliseconds), so all volumes are captured at the same instant. Caller holds e.mu.
func (e *Engine) snap(ctx context.Context, w io.Writer, s store.Snapshot, users []string) (store.Snapshot, error) {
	if len(s.Volumes) == 0 {
		return s, errNoData
	}
	if !snapshot.Supported(e.SnapshotDir) {
		return s, fmt.Errorf("snapshots need btrfs under %s", e.SnapshotDir)
	}
	unpause, err := pause(ctx, w, users)
	if err != nil {
		return s, err
	}
	defer unpause()
	dir := filepath.Join(e.SnapshotDir, compose.Slug(s.Project), time.Now().UTC().Format("20060102-150405")+"-"+randHex(2))
	for i := range s.Volumes {
		v := &s.Volumes[i]
		v.Path = filepath.Join(dir, strconv.Itoa(i)+"-"+unsafeName.ReplaceAllString(v.Name, "_"))
		if err := takeSnapshot(ctx, v.Source, v.Path); err != nil {
			snapshot.Delete(ctx, dir)
			return s, err
		}
	}
	id, err := e.DB.AddSnapshot(s)
	if err != nil {
		snapshot.Delete(ctx, dir)
		return s, err
	}
	s.ID = id
	fmt.Fprintf(w, "%s: snapshot #%d (%s) of %s\n", s.Project, id, s.Reason, names(s.Volumes))
	e.DB.Event(s.Project, "snapshot", "#%d %s: %s", id, s.Reason, names(s.Volumes))
	e.prune(ctx, w, s.Project)
	return s, nil
}

func names(vs []store.SnapshotVolume) string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return strings.Join(out, ", ")
}

// prune drops automatic snapshots beyond the newest keep.
func (e *Engine) prune(ctx context.Context, w io.Writer, project string) {
	old, err := e.DB.SnapshotsToPrune(project, e.keep())
	if err != nil {
		fmt.Fprintf(w, "%s: prune snapshots: %v\n", project, err)
		return
	}
	for _, s := range old {
		if err := e.deleteSnapshot(ctx, s); err != nil {
			fmt.Fprintf(w, "%s: prune snapshot #%d: %v\n", project, s.ID, err)
		}
	}
}

func (e *Engine) deleteSnapshot(ctx context.Context, s store.Snapshot) error {
	for _, v := range s.Volumes {
		if err := snapshot.Delete(ctx, v.Path); err != nil {
			return err
		}
		os.Remove(filepath.Dir(v.Path)) // the snapshot's dir, once empty
	}
	return e.DB.DeleteSnapshot(s.ID)
}

// preDeploy snapshots a project's data before a deploy changes it. No data is fine; a failed
// snapshot aborts the deploy (set snapshots: off in vops.yml to deploy without them).
// It returns the snapshot id, 0 when none was taken.
func (e *Engine) preDeploy(ctx context.Context, w io.Writer, pp *ProjectPlan, commit string) (int64, error) {
	if !e.snapshotsOn() || !snapshot.Supported(e.SnapshotDir) {
		return 0, nil
	}
	vols, _, users, err := e.projectData(ctx, pp.Path)
	if err != nil {
		return 0, err
	}
	current, _ := e.DB.Projects()
	s, err := e.snap(ctx, w, store.Snapshot{Project: pp.Path, Reason: "pre-deploy", Note: "before " + short(commit), Commit: current[pp.Path].Commit, Volumes: vols}, users)
	if err != nil && !errors.Is(err, errNoData) {
		return 0, fmt.Errorf("pre-deploy snapshot failed, nothing was deployed (snapshots: off in vops.yml skips them): %w", err)
	}
	return s.ID, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Snapshot takes a manual snapshot of a project's data.
func (e *Engine) Snapshot(ctx context.Context, w io.Writer, project, note string) (store.Snapshot, error) {
	e.lock()
	defer e.unlock()
	if !e.snapshotsOn() {
		return store.Snapshot{}, errors.New("snapshots are off (snapshots: off in vops.yml)")
	}
	vols, unprotected, users, err := e.projectData(ctx, project)
	if err != nil {
		return store.Snapshot{}, err
	}
	if len(unprotected) > 0 {
		fmt.Fprintf(w, "%s: not covered (not btrfs subvolumes): %s\n", project, strings.Join(unprotected, ", "))
	}
	current, _ := e.DB.Projects()
	s, err := e.snap(ctx, w, store.Snapshot{Project: project, Reason: "manual", Note: note, Commit: current[project].Commit, Volumes: vols}, users)
	if errors.Is(err, errNoData) {
		return s, fmt.Errorf("%s has no data to snapshot (no volumes, no bind mounts inside its dir)", project)
	}
	return s, err
}

// DeleteSnapshot deletes a snapshot and its data.
func (e *Engine) DeleteSnapshot(ctx context.Context, id int64) error {
	e.lock()
	defer e.unlock()
	s, err := e.DB.Snapshot(id)
	if err != nil {
		return err
	}
	return e.deleteSnapshot(ctx, s)
}
