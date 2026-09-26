package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// textFile reports whether the dashboard may show a tracked file: common text formats, no env files.
func textFile(name string) bool {
	base := strings.ToLower(path.Base(name))
	if strings.HasPrefix(base, ".env") || strings.HasSuffix(base, ".env") {
		return false
	}
	switch base {
	case "dockerfile", "containerfile", "makefile", "justfile", "readme", "license", ".gitignore", ".containerignore", ".dockerignore":
		return true
	}
	switch path.Ext(base) {
	case ".yml", ".yaml", ".md", ".txt", ".json", ".toml", ".ini", ".conf", ".cfg", ".sh", ".sql", ".caddyfile", ".html", ".css", ".js", ".ts", ".go", ".py", ".rb", ".xml", ".properties", ".example", ".tmpl", ".service":
		return true
	}
	return false
}

type treeFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Text bool   `json:"text"`
}

func (d *Daemon) trackedFiles(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", d.Repo, "ls-files", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []string
	for f := range strings.SplitSeq(strings.TrimRight(string(out), "\x00"), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// tree lists git-tracked files; the ui builds folders from the paths.
func (d *Daemon) tree(ctx context.Context) ([]treeFile, error) {
	files, err := d.trackedFiles(ctx)
	if err != nil {
		return nil, err
	}
	out := []treeFile{}
	for _, f := range files {
		t := treeFile{Path: f, Text: textFile(f)}
		if st, err := os.Stat(filepath.Join(d.Repo, filepath.FromSlash(f))); err == nil {
			t.Size = st.Size()
		}
		out = append(out, t)
	}
	return out, nil
}

func (d *Daemon) file(ctx context.Context, p string) ([]byte, error) {
	files, err := d.trackedFiles(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(files, p) || !textFile(p) {
		return nil, fmt.Errorf("%q is not a tracked text file", p)
	}
	full := filepath.Join(d.Repo, filepath.FromSlash(p))
	st, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 512<<10 {
		return nil, fmt.Errorf("%q is not a regular file under 512KB", p)
	}
	return os.ReadFile(full)
}
