package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Path a registry gave, with slashes, without empty and dot parts; an error for an empty, absolute or traversing path, one with a colon,
// or a nested one where a single name is wanted
func clean(value, field string, nested bool) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", fmt.Errorf("The %s is empty, so it is not safe to use.", field)
	}
	normalized := strings.ReplaceAll(raw, `\`, "/")
	var parts []string
	for _, part := range strings.Split(normalized, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if strings.HasPrefix(normalized, "/") || len(parts) == 0 || slices.Contains(parts, "..") ||
		slices.ContainsFunc(parts, func(p string) bool { return strings.Contains(p, ":") }) || !nested && len(parts) != 1 {
		return "", fmt.Errorf("The %s %s is not safe, because it leaves its folder or is not a plain name.", field, value)
	}
	return strings.Join(parts, "/"), nil
}

// Name a skill installs under, one safe folder name
func skillName(name string) (string, error) {
	return clean(name, "skill name", false)
}

// Path of a file of a bundle within the skill's folder
func bundlePath(rel string) (string, error) {
	return clean(rel, "bundle file path", true)
}

// Install path a lock records: relative, without traversal, ending in the skill's name
func lockPath(installPath, name string) (string, error) {
	safe, err := skillName(name)
	if err != nil {
		return "", err
	}
	normalized, err := clean(installPath, "install path", true)
	if err != nil {
		return "", err
	}
	if parts := strings.Split(normalized, "/"); parts[len(parts)-1] != safe {
		return "", fmt.Errorf("The install path %s is not safe, because it leaves the skills folder or passes through a link.", installPath)
	}
	return normalized, nil
}

// Folder of an install path strictly inside the skills folder, refused when a part of it is a link or it is the skills folder itself
func (r *run) installDir(installPath, name string) (string, error) {
	normalized, err := lockPath(installPath, name)
	if err != nil {
		return "", err
	}
	target := r.scope.Dir()
	root := resolved(target)
	for _, part := range strings.Split(normalized, "/") {
		target = filepath.Join(target, part)
		if redirect(target) {
			return "", fmt.Errorf("The install path %s is not safe, because it leaves the skills folder or passes through a link.", installPath)
		}
	}
	dir := resolved(target)
	if dir == root || !inside(dir, root) {
		return "", fmt.Errorf("The install path %s is not safe, because it leaves the skills folder or passes through a link.", installPath)
	}
	return dir, nil
}

// Absolute path with its links followed, those of the part that exists when the rest does not yet
func resolved(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	parent := filepath.Dir(p)
	if parent == p {
		return filepath.Clean(p)
	}
	return filepath.Join(resolved(parent), filepath.Base(p))
}

// Whether child is parent or lies under it
func inside(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// Whether the path is a link, or a junction on Windows, that leads a walk or a removal elsewhere
func redirect(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && (info.Mode()&os.ModeSymlink != 0 || runtime.GOOS == "windows" && info.Mode()&os.ModeIrregular != 0)
}

// Every file under a folder by its path relative to it, with slashes, sorted
func walkFiles(root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, err := filepath.Rel(root, p); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(out)
	return out
}

// Whether a file of a skill never ships in a bundle: a dot file, Python bytecode or its cache
func skipped(rel string) bool {
	base := rel[strings.LastIndex(rel, "/")+1:]
	return strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".pyc") || slices.Contains(strings.Split(rel, "/"), "__pycache__")
}
