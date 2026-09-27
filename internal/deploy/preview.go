package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/compose"
	"github.com/sh-lucas/vops/internal/config"
	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
	"github.com/sh-lucas/vops/internal/snapshot"
	"github.com/sh-lucas/vops/internal/store"
)

// Previews: a preview is the synthetic project "<project>@<name>", run by the same engine as any project
// (labels, hashes, plan/apply). What differs is where it comes from:
//
//	code:   a git worktree of the project's applied commit (or a ref) in PreviewDir/<slug>
//	data:   a copy-on-write copy of the project's data (live, or a snapshot), made once at creation
//	env:    preview.env from the project dir + the project's preview secrets, never production's env
//	images: the project's, with overrides (--image, registry pushes of preview tags)
//
// Guardrails live in compose (no shared network, no skipped services, no host ports, no extra domains).
// Previews are not in the plan: git doesn't describe them, the previews table does.

const (
	DefaultPreviewMax = 5
	DefaultPreviewTTL = 3 * 24 * time.Hour
	PreviewEnvFile    = "preview.env" // in the project dir: non-secret overrides for previews
)

// PreviewPath is the project path of a preview.
func PreviewPath(project, name string) string { return project + "@" + name }

// PreviewEnvScope is where a project's preview secrets live in the env table (shared by all its previews).
func PreviewEnvScope(project string) string { return project + "@*" }

// PreviewOpts is what `vops preview up` asks for.
type PreviewOpts struct {
	Project string            `json:"project"`
	Name    string            `json:"name"`
	Ref     string            `json:"ref"`    // git ref on the host; empty keeps the preview's commit (a new one gets the project's applied commit)
	Images  map[string]string `json:"images"` // service -> image, merged into the preview's overrides
	From    int64             `json:"from"`   // new previews: copy the data of this snapshot instead of the live data
}

// PreviewState is a preview as `preview ls` and the dashboard show it.
type PreviewState struct {
	store.Preview `json:",inline"`
	Path          string         `json:"path"`
	ExpiresAt     int64          `json:"expires_at"`
	Error         string         `json:"error,omitempty"`
	Services      []ServiceState `json:"services"`
}

func (e *Engine) previewMax() int {
	if e.PreviewMax > 0 {
		return e.PreviewMax
	}
	return DefaultPreviewMax
}

func (e *Engine) previewTTL() time.Duration {
	if e.PreviewTTL > 0 {
		return e.PreviewTTL
	}
	return DefaultPreviewTTL
}

func (e *Engine) domain() string {
	if e.Config != nil {
		return e.Config().Domain
	}
	c, _ := config.ReadRepo(e.Repo)
	return c.Domain
}

func (e *Engine) previewRoot(path string) string {
	return filepath.Join(e.PreviewDir, compose.Slug(path))
}

// PreviewUp creates a preview or updates it (new ref, new images, or just redeploy and keep it alive).
func (e *Engine) PreviewUp(ctx context.Context, w io.Writer, o PreviewOpts) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	w = &syncWriter{w: w}
	path := PreviewPath(o.Project, o.Name)
	if err := compose.ValidProjectPath(path); err != nil {
		return err
	}
	if e.PreviewDir == "" {
		return errors.New("previews need a preview dir")
	}
	if strings.HasPrefix(o.Ref, "-") {
		return fmt.Errorf("invalid ref %q", o.Ref)
	}
	for svc, img := range o.Images {
		if img == "" || strings.HasPrefix(img, "-") || strings.ContainsAny(img, " \t\n") {
			return fmt.Errorf("invalid image %q for %s", img, svc)
		}
	}
	pv, found, err := e.DB.Preview(o.Project, o.Name)
	if err != nil {
		return err
	}
	if !found {
		projects, err := DesiredProjects(ctx, e.Repo)
		if err != nil {
			return err
		}
		if _, ok := projects[o.Project]; !ok {
			return fmt.Errorf("no project %s in git", o.Project)
		}
		for p := range projects {
			if compose.Slug(p) == compose.Slug(path) || compose.DNSName(p) == compose.DNSName(path) {
				return fmt.Errorf("preview %s would take the names of project %s: pick another name", path, p)
			}
		}
		all, err := e.DB.Previews("")
		if err != nil {
			return err
		}
		if len(all) >= e.previewMax() {
			return fmt.Errorf("%d previews already run on this host (preview_max in vops.yml): remove one with vops preview rm", len(all))
		}
		pv = store.Preview{Project: o.Project, Name: o.Name, Images: map[string]string{}}
	} else if o.From != 0 {
		return fmt.Errorf("preview %s exists and keeps its data: `vops preview rm %s %s` first to start from snapshot #%d", path, o.Project, o.Name, o.From)
	}
	switch {
	case o.Ref != "":
		commit, err := git(ctx, e.Repo, "rev-parse", "--verify", "-q", o.Ref+"^{commit}")
		if err != nil || commit == "" {
			return fmt.Errorf("ref %s is not on the host (git push vops %s)", o.Ref, o.Ref)
		}
		pv.Ref, pv.Commit = o.Ref, commit
	case !found:
		flags, _ := e.DB.Projects()
		if pv.Commit = flags[o.Project].Commit; pv.Commit == "" {
			if pv.Commit, err = git(ctx, e.Repo, "rev-parse", "HEAD"); err != nil {
				return err
			}
		}
	}
	maps.Copy(pv.Images, o.Images)

	root := e.previewRoot(path)
	if err := e.worktree(ctx, root, pv.Commit); err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	fail := func(err error) error {
		if !found { // nothing half-made stays behind
			e.removePreview(ctx, io.Discard, pv)
		}
		return err
	}
	var warns []string
	pp, err := e.planPreview(ctx, pv, e.domain(), &warns)
	if err != nil {
		return fail(err)
	}
	for _, warn := range warns {
		fmt.Fprintf(w, "  ! %s\n", warn)
	}
	if !found {
		if pv.Data, err = e.previewData(ctx, w, pv, pp.project, root, o.From); err != nil {
			return fail(err)
		}
		pv.SnapshotID = o.From
	}
	if err := e.DB.PutPreview(pv); err != nil {
		return fail(err)
	}
	e.DB.Event(o.Project, "preview", "preview %s %s at %.12s%s", o.Name, map[bool]string{true: "updated", false: "created"}[found], pv.Commit, describeImages(pv.Images))
	err = e.applyProject(ctx, w, &Plan{Commit: pv.Commit}, pp, ApplyOpts{})
	e.RefreshRoutes(ctx)
	if err != nil {
		e.DB.Event(o.Project, "error", "preview %s: %v", o.Name, err)
		return err
	}
	var domains []string
	for _, n := range sortedKeys(pp.specs) {
		if ds := pp.specs[n].spec.Domains; len(ds) > 0 {
			domains = append(domains, ds[len(ds)-1])
		}
	}
	line := fmt.Sprintf("%s: up at %.12s", path, pv.Commit)
	if len(domains) > 0 {
		line += ", serving " + strings.Join(domains, ", ")
	}
	fmt.Fprintln(w, line)
	return nil
}

func describeImages(images map[string]string) string {
	var out []string
	for _, s := range sortedKeys(images) {
		out = append(out, s+"="+images[s])
	}
	if len(out) == 0 {
		return ""
	}
	return " with " + strings.Join(out, ", ")
}

// worktree checks out commit (detached) at dir: a git worktree of the host repo, so build contexts,
// env files and bind mounts of the preview are its own.
func (e *Engine) worktree(ctx context.Context, dir, commit string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if head, _ := git(ctx, dir, "rev-parse", "HEAD"); head == commit {
			return nil
		}
		_, err := git(ctx, dir, "checkout", "-q", "--detach", commit)
		return err
	}
	if err := os.MkdirAll(e.PreviewDir, 0o700); err != nil {
		return err
	}
	git(ctx, e.Repo, "worktree", "prune") // entries of worktrees deleted by hand
	_, err := git(ctx, e.Repo, "worktree", "add", "-q", "--detach", dir, commit)
	return err
}

// planPreview plans a preview like planProject plans a project, from its worktree, env and images.
// The plan is returned even on error: it still lists the preview's containers.
func (e *Engine) planPreview(ctx context.Context, pv store.Preview, domain string, warns *[]string) (*ProjectPlan, error) {
	path := PreviewPath(pv.Project, pv.Name)
	root := e.previewRoot(path)
	pp := &ProjectPlan{Path: path, Actions: []Action{}, specs: map[string]*desired{}, actual: map[string][]podman.Container{}}
	cs, err := podman.PS(ctx, LProject+"="+path)
	if err != nil {
		return pp, err
	}
	for _, c := range cs {
		pp.actual[c.Labels[LService]] = append(pp.actual[c.Labels[LService]], c)
	}
	projects, err := DesiredProjects(ctx, root)
	if err != nil {
		return pp, err
	}
	files := projects[pv.Project]
	if len(files) == 0 {
		return pp, fmt.Errorf("%s has no compose file at %.12s", pv.Project, pv.Commit)
	}
	env, err := e.previewEnv(filepath.Join(root, filepath.FromSlash(pv.Project)), pv.Project)
	if err != nil {
		return pp, err
	}
	return pp, e.planProject(ctx, pp, source{root: root, files: files, env: env, images: pv.Images}, domain, warns)
}

// previewEnv is a preview's env: preview.env (committed, not secret) overridden by the project's preview
// secrets. Production's env is never used: a preview runs on a copy of production's data, not with its keys.
func (e *Engine) previewEnv(projectDir, project string) (map[string]string, error) {
	env := map[string]string{}
	file := filepath.Join(projectDir, PreviewEnvFile)
	if _, err := os.Stat(file); err == nil {
		kv, err := compose.ReadEnvFile(file)
		if err != nil {
			return nil, err
		}
		maps.Copy(env, kv)
	}
	secrets, err := e.DB.Env(PreviewEnvScope(project))
	if err != nil {
		return nil, err
	}
	maps.Copy(env, secrets)
	return env, nil
}

// previewData gives a new preview a copy of its project's data: every volume and bind of the project
// (the live data, or snapshot from) becomes a writable btrfs snapshot under the preview's names. O(1),
// copy-on-write. Live data is copied with its containers paused (milliseconds), like a pre-deploy snapshot.
func (e *Engine) previewData(ctx context.Context, w io.Writer, pv store.Preview, proj *compose.Project, root string, from int64) (string, error) {
	path := proj.Path
	if !e.snapshotsOn() || !snapshot.Supported(e.SnapshotDir) {
		if from != 0 {
			return "", errors.New("--from needs snapshots (btrfs, snapshots: on)")
		}
		fmt.Fprintf(w, "%s: starting with empty data (copying data needs btrfs snapshots)\n", path)
		return "empty (no btrfs snapshots)", nil
	}
	base := *proj
	base.Path = pv.Project
	volumes := map[string]string{} // production volume -> preview volume
	for key, v := range proj.Volumes {
		if !v.External {
			volumes[base.VolumeName(key)] = proj.VolumeName(key)
		}
	}
	var vols []store.SnapshotVolume
	var users []string
	desc := "copy of production data at " + time.Now().UTC().Format("2006-01-02 15:04 UTC")
	if from != 0 {
		s, err := e.DB.Snapshot(from)
		if err != nil {
			return "", err
		}
		if s.Project != pv.Project {
			return "", fmt.Errorf("snapshot #%d belongs to %s, not %s", from, s.Project, pv.Project)
		}
		for _, v := range s.Volumes {
			v.Source = v.Path
			vols = append(vols, v)
		}
		desc = fmt.Sprintf("snapshot #%d (%s, %s)", s.ID, s.Reason, time.Unix(s.CreatedAt, 0).UTC().Format("2006-01-02 15:04 UTC"))
	} else {
		var err error
		if vols, _, users, err = e.projectData(ctx, pv.Project); err != nil {
			return "", err
		}
	}
	defer pause(ctx, users)()
	projectDir := filepath.Join(root, filepath.FromSlash(pv.Project))
	var copied []string
	for _, v := range vols {
		target := ""
		switch v.Kind {
		case "volume":
			name, ok := volumes[v.Name]
			if !ok {
				fmt.Fprintf(w, "%s: %s is not in the preview's compose, not copied\n", path, v.Name)
				continue
			}
			if podman.VolumePath(ctx, name) == "" {
				if _, err := podman.Run(ctx, "volume", "create", "--label", LProject+"="+path, name); err != nil {
					return "", err
				}
			}
			target = podman.VolumePath(ctx, name)
		case "bind":
			if target = filepath.Join(root, filepath.FromSlash(v.Name)); !within(target, projectDir) {
				continue
			}
		}
		if err := snapshot.Restore(ctx, v.Source, target); err != nil {
			return "", fmt.Errorf("copy %s: %w", v.Name, err)
		}
		copied = append(copied, v.Name)
	}
	if len(copied) == 0 {
		fmt.Fprintf(w, "%s: %s has no data to copy (no btrfs volumes or binds)\n", path, pv.Project)
		return "empty (nothing to copy)", nil
	}
	fmt.Fprintf(w, "%s: data is a %s: %s\n", path, desc, strings.Join(copied, ", "))
	return desc, nil
}

// pause pauses running containers and returns the function that unpauses them.
func pause(ctx context.Context, ids []string) func() {
	var paused []string
	for _, id := range ids {
		if _, err := podman.Run(ctx, "pause", id); err == nil {
			paused = append(paused, id)
		}
	}
	return func() {
		for _, id := range paused {
			podman.Run(ctx, "unpause", id)
		}
	}
}

// PreviewRm removes a preview and everything it made.
func (e *Engine) PreviewRm(ctx context.Context, w io.Writer, project, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	path := PreviewPath(project, name)
	if err := compose.ValidProjectPath(path); err != nil {
		return err
	}
	pv, found, err := e.DB.Preview(project, name)
	if err != nil {
		return err
	}
	if !found {
		// leftovers of a preview the db doesn't know (a crash while creating it) are removed too
		cs, _ := podman.PS(ctx, LProject+"="+path)
		if _, err := os.Stat(e.previewRoot(path)); len(cs) == 0 && err != nil {
			return fmt.Errorf("no preview %s of %s (vops preview ls)", name, project)
		}
		pv = store.Preview{Project: project, Name: name}
	}
	if err := e.removePreview(ctx, w, pv); err != nil {
		return err
	}
	e.DB.Event(project, "preview", "preview %s removed", name)
	return nil
}

// removePreview removes a preview's containers, networks, volumes, images, worktree (with its data) and
// row. On error the row stays, so a retry (or the ttl) finishes the job. Caller holds e.mu.
func (e *Engine) removePreview(ctx context.Context, w io.Writer, pv store.Preview) error {
	path := PreviewPath(pv.Project, pv.Name)
	cs, err := podman.PS(ctx, LProject+"="+path)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range cs {
		e.Routes.Set(proxy.Key{Project: path, Service: c.Labels[LService]}, nil, nil)
	}
	if err := e.remove(ctx, cs); err != nil {
		errs = append(errs, err)
	}
	label := "label=" + LProject + "=" + path
	nets, _ := podman.Run(ctx, "network", "ls", "--format", "{{.Name}}", "--filter", label)
	for _, n := range strings.Fields(nets) {
		if _, err := podman.Run(ctx, "network", "rm", n); err != nil {
			errs = append(errs, err)
		}
	}
	vols, _ := podman.Run(ctx, "volume", "ls", "--format", "{{.Name}}", "--filter", label)
	for _, v := range strings.Fields(vols) {
		if _, err := podman.Run(ctx, "volume", "rm", "-f", v); err != nil {
			// data owned by the container's users: delete it from their namespace, then the volume
			snapshot.Delete(ctx, podman.VolumePath(ctx, v))
			if _, err := podman.Run(ctx, "volume", "rm", "-f", v); err != nil {
				errs = append(errs, err)
			}
		}
	}
	// images built for the preview and local tags of its overrides (in use elsewhere = kept by podman)
	built, _ := podman.Run(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=localhost/vops/"+compose.Slug(path)+"-*")
	images := strings.Fields(built)
	for _, img := range pv.Images {
		images = append(images, compose.NormalizeImage(img))
	}
	for _, img := range images {
		podman.Run(ctx, "rmi", img)
	}
	if err := snapshot.Delete(ctx, e.previewRoot(path)); err != nil {
		errs = append(errs, err)
	}
	git(ctx, e.Repo, "worktree", "prune")
	if len(errs) > 0 {
		return fmt.Errorf("removing %s: %w", path, errors.Join(errs...))
	}
	if err := e.DB.DeletePreview(pv.Project, pv.Name); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: removed (%d containers, networks, volumes, worktree)\n", path, len(cs))
	return nil
}

// ExpirePreviews removes previews not updated for longer than the ttl (housekeeping).
func (e *Engine) ExpirePreviews(ctx context.Context, w io.Writer) {
	all, err := e.DB.Previews("")
	if err != nil {
		fmt.Fprintln(w, err)
		return
	}
	ttl := e.previewTTL()
	for _, pv := range all {
		if time.Since(time.Unix(pv.UpdatedAt, 0)) < ttl {
			continue
		}
		e.Lock(func() {
			// it may have been updated (or removed) meanwhile
			if cur, found, _ := e.DB.Preview(pv.Project, pv.Name); !found || time.Since(time.Unix(cur.UpdatedAt, 0)) < ttl {
				return
			}
			if err := e.removePreview(ctx, w, pv); err != nil {
				fmt.Fprintln(w, err)
				e.DB.Event(pv.Project, "error", "expired preview %s: %v", pv.Name, err)
				return
			}
			e.DB.Event(pv.Project, "preview", "preview %s removed: not updated for %s (preview_ttl)", pv.Name, ttl)
		})
	}
}

// Previews lists previews ("" = of all projects) with their services.
func (e *Engine) Previews(ctx context.Context, project string) ([]PreviewState, error) {
	all, err := e.DB.Previews(project)
	if err != nil {
		return nil, err
	}
	domain := e.domain()
	out := []PreviewState{}
	for _, pv := range all {
		st := PreviewState{Preview: pv, Path: PreviewPath(pv.Project, pv.Name), ExpiresAt: pv.UpdatedAt + int64(e.previewTTL().Seconds())}
		var warns []string
		pp, err := e.planPreview(ctx, pv, domain, &warns)
		if err != nil {
			st.Error = err.Error()
		}
		st.Services = pp.Services()
		out = append(out, st)
	}
	return out, nil
}

// PreviewTarget is a preview that a registry push creates or updates.
type PreviewTarget struct {
	Project, Name string
	Images        map[string]string // service -> the pushed image
}

// PreviewTargets: pushing repo:tag, where tag matches a project's x-vops.previews pattern ("preview-*"),
// creates or updates the preview named by the * part, with that tag on every service running repo.
func (p *Plan) PreviewTargets(repo, tag string) []PreviewTarget {
	var out []PreviewTarget
	for _, pp := range p.Projects {
		if pp.Disabled || pp.Gone || pp.Error != "" {
			continue
		}
		name, ok := compose.PreviewName(pp.previews, tag)
		if !ok {
			continue
		}
		images := map[string]string{}
		for svc, d := range pp.specs {
			if d.repo == repo {
				images[svc] = withTag(d.spec.Image, tag)
			}
		}
		if len(images) > 0 {
			out = append(out, PreviewTarget{Project: pp.Path, Name: name, Images: images})
		}
	}
	return out
}

// withTag replaces the tag (or digest) of an image ref.
func withTag(image, tag string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	} else if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	return image + ":" + tag
}
