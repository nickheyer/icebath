package lsp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const (
	workspaceCacheCap = 512
	maxWalk           = 64
)

// One cached worktree resolution
type worktreeHit struct {
	root  string
	found bool
}

var (
	workspaceMu    sync.Mutex
	workspaceCache = map[string]worktreeHit{}
)

// NormalizePath expands a leading tilde and makes the path absolute and clean without resolving symlinks
func NormalizePath(path string) string {
	p, err := normalizePath(path)
	if err != nil {
		return pypath.Normpath(pypath.Expanduser(path))
	}
	return p
}

// Absolute clean form of a path, the getcwd failure when a relative path has no cwd to anchor on
func normalizePath(path string) (string, error) {
	return pypath.Abspath(pypath.Expanduser(path))
}

// Normalized start directory, the parent when the start is a regular file
func startDir(start string) (string, bool) {
	p, err := normalizePath(start)
	if err != nil {
		return "", false
	}
	if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
		p = filepath.Dir(p)
	}
	return p, true
}

// The start directory and its ancestors up to the filesystem root, bounded by the walk cap
func walkUp(start string) []string {
	var out []string
	cur := start
	for range maxWalk {
		out = append(out, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return out
}

// Whether a path exists, false on any stat error
func pathExists(p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// FindGitWorktree returns the nearest ancestor holding a .git file or directory, empty when none
func FindGitWorktree(start string) string {
	startPath, ok := startDir(start)
	if !ok {
		return ""
	}
	workspaceMu.Lock()
	hit, cached := workspaceCache[startPath]
	workspaceMu.Unlock()
	if cached {
		return hit.root
	}
	resolved := ""
	for _, cur := range walkUp(startPath) {
		exists, err := pathExists(filepath.Join(cur, ".git"))
		if err != nil {
			break
		}
		if exists {
			resolved = cur
			break
		}
	}
	workspaceMu.Lock()
	workspaceCache[startPath] = worktreeHit{root: resolved, found: resolved != ""}
	if len(workspaceCache) > workspaceCacheCap {
		workspaceCache = map[string]worktreeHit{}
	}
	workspaceMu.Unlock()
	return resolved
}

// IsInsideWorkspace reports whether a path is the workspace root or lies under it, symlinks unresolved
func IsInsideWorkspace(path, workspaceRoot string) bool {
	p, err := normalizePath(path)
	if err != nil {
		return false
	}
	root, err := normalizePath(workspaceRoot)
	if err != nil {
		return false
	}
	if p == root {
		return true
	}
	common, ok := commonPath(p, root)
	return ok && common == root
}

// Python os.path.commonpath of two absolute paths, components taken from the first, false across drives
func commonPath(a, b string) (string, bool) {
	sep := string(filepath.Separator)
	volA, volB := filepath.VolumeName(a), filepath.VolumeName(b)
	fold := func(s string) string { return s }
	if runtime.GOOS == "windows" {
		fold = strings.ToLower
		if fold(volA) != fold(volB) {
			return "", false
		}
	}
	split := func(s string) []string {
		var out []string
		for _, c := range strings.Split(s, sep) {
			if c != "" && c != "." {
				out = append(out, c)
			}
		}
		return out
	}
	pa, pb := split(a[len(volA):]), split(b[len(volB):])
	n := 0
	for n < len(pa) && n < len(pb) && fold(pa[n]) == fold(pb[n]) {
		n++
	}
	return volA + sep + strings.Join(pa[:n], sep), true
}

// Whether any of the names exists in a directory
func anyPresent(dir string, names []string) bool {
	for _, name := range names {
		if exists, err := pathExists(filepath.Join(dir, name)); err == nil && exists {
			return true
		}
	}
	return false
}

// NearestRoot walks up for the first directory holding a marker, empty past the ceiling or after an exclude hit
func NearestRoot(start string, markers, excludes []string, ceiling string) string {
	startPath, ok := startDir(start)
	if !ok {
		return ""
	}
	ceilingPath := ""
	if ceiling != "" {
		ceilingPath = NormalizePath(ceiling)
	}
	for _, cur := range walkUp(startPath) {
		if anyPresent(cur, excludes) {
			return ""
		}
		if !anyPresent(cur, []string{"__init__.py"}) && anyPresent(cur, markers) {
			return cur
		}
		if ceilingPath != "" && cur == ceilingPath {
			return ""
		}
	}
	return ""
}

// ResolveWorkspaceForFile picks the cwd worktree when the file lies in it, else the file's own worktree
func ResolveWorkspaceForFile(filePath, cwd string) (string, bool) {
	anchor := cwd
	if anchor == "" {
		if wd, err := os.Getwd(); err == nil {
			anchor = wd
		}
	}
	if anchor != "" {
		if root := FindGitWorktree(anchor); root != "" && IsInsideWorkspace(filePath, root) {
			return root, true
		}
	}
	if root := FindGitWorktree(filePath); root != "" {
		return root, true
	}
	return "", false
}

// OperatorWorkspaceRoots maps each anchor to its git worktree, skipping failed anchors and repos at or above home
func OperatorWorkspaceRoots(anchors []func() (string, error)) map[string]bool {
	home := NormalizePath("~")
	roots := map[string]bool{}
	for _, anchor := range anchors {
		dir, err := anchor()
		if err != nil {
			continue
		}
		if root := FindGitWorktree(dir); root != "" && !IsInsideWorkspace(home, root) {
			roots[root] = true
		}
	}
	return roots
}

// IsTrustedWorkspace reports whether a root lies in a trusted directory or belongs to an operator worktree
func IsTrustedWorkspace(root string, trustedRoots []string, operatorRoots map[string]bool) bool {
	for _, t := range trustedRoots {
		if IsInsideWorkspace(root, t) {
			return true
		}
	}
	return operatorRoots[FindGitWorktree(root)]
}

// ClearWorkspaceCache forgets every cached worktree resolution
func ClearWorkspaceCache() {
	workspaceMu.Lock()
	workspaceCache = map[string]worktreeHit{}
	workspaceMu.Unlock()
}
