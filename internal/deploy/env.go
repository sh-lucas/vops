package deploy

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/sh-lucas/vops/internal/compose"
)

// secretKey: a compose literal under such a key is a secret committed to git.
var secretKey = regexp.MustCompile(`(?i)PASSWORD|SECRET|TOKEN|KEY|PRIVATE`)

// EnvUsage is what the env view shows: per service, every variable its containers get and where it comes
// from, and the vops env keys nothing uses. Values never leave the engine.
type EnvUsage struct {
	Services map[string][]compose.EnvEntry `json:"services"` // source: compose | env_file | vops | missing
	Unused   []string                      `json:"unused"`
}

// EnvUsage reads a project's compose like the plan does, with its env (or, preview=true, the previews' env:
// preview.env + preview secrets), and reports sources only.
func (e *Engine) EnvUsage(ctx context.Context, project string, preview bool) (EnvUsage, error) {
	u := EnvUsage{Services: map[string][]compose.EnvEntry{}, Unused: []string{}}
	projects, err := DesiredProjects(ctx, e.Repo)
	if err != nil {
		return u, err
	}
	files, ok := projects[project]
	if !ok {
		return u, fmt.Errorf("no project %s in git", project)
	}
	dir := filepath.Join(e.Repo, filepath.FromSlash(project))
	path, scope := project, project
	origin := map[string]compose.EnvEntry{} // where each env key comes from, for bare keys and ${VAR}s
	var env map[string]string
	if preview {
		path, scope = PreviewPath(project, "preview"), PreviewEnvScope(project)
		if file := filepath.Join(dir, PreviewEnvFile); fileExists(file) {
			kv, err := compose.ReadEnvFile(file)
			if err != nil {
				return u, err
			}
			for k := range kv {
				origin[k] = compose.EnvEntry{Source: "env_file", File: PreviewEnvFile}
			}
		}
		if env, err = e.previewEnv(dir, project); err != nil {
			return u, err
		}
	} else if env, err = e.DB.Env(project); err != nil {
		return u, err
	}
	secrets, err := e.DB.EnvKeys(scope)
	if err != nil {
		return u, err
	}
	for _, k := range secrets {
		origin[k.Key] = compose.EnvEntry{Source: "vops"}
	}
	for k, v := range builtinEnv(path, e.domain()) {
		if _, set := env[k]; !set {
			env[k] = v
			origin[k] = compose.EnvEntry{Source: "vops", File: "builtin"}
		}
	}
	proj, err := compose.Load(dir, path, files, maps.Clone(env))
	if err != nil {
		return u, err
	}
	usage, err := proj.EnvUsage(env)
	if err != nil {
		return u, err
	}
	used := map[string]bool{"COMPOSE_PROFILES": true} // read by vops itself
	maps.Copy(used, proj.Refs)
	for svc, entries := range usage {
		for i, en := range entries {
			switch {
			case en.Source == "project":
				o := origin[en.Key]
				entries[i].Source, entries[i].File = o.Source, o.File
				used[en.Key] = true
			case en.Source == "compose" && slices.ContainsFunc(en.Vars, func(v string) bool { return origin[v].Source == "vops" && origin[v].File == "" }):
				entries[i].Source = "vops"
			case en.Source == "compose" && len(en.Vars) == 0:
				entries[i].Secret = secretKey.MatchString(en.Key)
			}
		}
		u.Services[svc] = entries
	}
	for _, k := range secrets {
		if !used[k.Key] {
			u.Unused = append(u.Unused, k.Key)
		}
	}
	return u, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
