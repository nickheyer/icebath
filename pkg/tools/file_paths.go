package tools

import (
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Working directory values that mean not configured
var terminalCwdSentinels = setOf("", ".", "./", "auto", "cwd")

var msysDriveRe = regexp.MustCompile(`^/(?:(?:cygdrive|mnt)/)?([a-zA-Z])(/.*)?$`)

// Translates a Git Bash drive path to its native Windows form, a no op elsewhere
func msysToWindowsPath(p string) string {
	if runtime.GOOS != "windows" || p == "" {
		return p
	}
	m := msysDriveRe.FindStringSubmatch(p)
	if m == nil {
		return p
	}
	tail := strings.ReplaceAll(m[2], "/", `\`)
	if tail == "" {
		tail = `\`
	}
	return strings.ToUpper(m[1]) + ":" + tail
}

// Home directory of the OS user, empty when unknown
func userHomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// Expands a leading tilde or tilde user prefix the way a shell would
func expandTilde(path string) string {
	if path == "" || !strings.Contains(path, "~") {
		return path
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home := userHomeDir()
		if home == "" {
			return path
		}
		if path == "~" {
			return home
		}
		return filepath.Join(home, path[2:])
	}
	if strings.HasPrefix(path, "~") {
		rest := path[1:]
		name, tail := rest, ""
		if i := strings.IndexAny(rest, `/\`); i >= 0 {
			name, tail = rest[:i], rest[i:]
		}
		if u, err := user.Lookup(name); err == nil && u.HomeDir != "" {
			return u.HomeDir + tail
		}
	}
	return path
}

// Absolute path with symlinks resolved along the longest existing prefix
func resolveNonStrict(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, base := filepath.Split(abs)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" || dir == abs {
		return abs
	}
	return filepath.Join(resolveNonStrict(dir), base)
}

// Realpath of a path that may not exist, used by every write guard comparison
func realPath(p string) string {
	return resolveNonStrict(expandTilde(p))
}

// Absolute workspace root the session anchors relative paths to, else the process cwd
func authoritativeWorkspaceRoot(env *Env) string {
	raw := ""
	if env != nil {
		raw = strings.TrimSpace(env.Cwd)
	}
	if terminalCwdSentinels[strings.ToLower(raw)] {
		return ""
	}
	expanded := expandTilde(raw)
	if !filepath.IsAbs(expanded) {
		return ""
	}
	return expanded
}

// Absolute base directory for resolving relative paths
func resolveBaseDir(env *Env) string {
	root := authoritativeWorkspaceRoot(env)
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			wd = string(filepath.Separator)
		}
		root = wd
	}
	return resolveNonStrict(msysToWindowsPath(expandTilde(root)))
}

// Resolves a model supplied path against the session base directory
func resolvePathForTask(env *Env, path string) string {
	text := expandTilde(msysToWindowsPath(path))
	if !filepath.IsAbs(text) {
		text = filepath.Join(resolveBaseDir(env), text)
	}
	return resolveNonStrict(text)
}

// Splits a directory entry keeping the last component after dropping trailing separators and dots
func splitEntry(path string) (string, string) {
	vol := filepath.VolumeName(path)
	tail := path[len(vol):]
	trimmed := strings.TrimRight(tail, `/\`)
	if trimmed == "" && tail != "" {
		trimmed = tail[:1]
	}
	full := vol + trimmed
	parent, leaf := filepath.Split(full)
	if parent != "" && parent != full {
		parent = strings.TrimRight(parent, `/\`)
		if parent == "" || parent == vol {
			parent = vol + string(filepath.Separator)
		}
	}
	if leaf == "." && parent != "" {
		return splitEntry(parent)
	}
	return parent, leaf
}

// Resolves the parent but keeps a symlink in the last component for delete and rename
func resolveEntryForTask(env *Env, path string) string {
	parent, name := splitEntry(path)
	if name == "" || name == "." || name == ".." || (parent == "" && strings.HasPrefix(name, "~")) {
		return resolvePathForTask(env, path)
	}
	if parent == "" {
		parent = "."
	}
	return filepath.Join(resolvePathForTask(env, parent), name)
}

// Reports whether a resolved path equals or sits below a resolved root
func isUnder(path, root string) bool {
	if path == root {
		return true
	}
	root = strings.TrimRight(root, string(filepath.Separator))
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// Warning when a relative path resolved outside the workspace root, else empty
func pathResolutionWarning(env *Env, path, resolved string) string {
	if filepath.IsAbs(expandTilde(path)) {
		return ""
	}
	root := authoritativeWorkspaceRoot(env)
	if root == "" {
		return ""
	}
	root = resolveNonStrict(root)
	if isUnder(resolved, root) {
		return ""
	}
	return "Relative path '" + path + "' resolved to '" + resolved + "', which is OUTSIDE the active workspace ('" + root +
		"'). The edit will land in a different directory than the terminal's cwd. If this is not intended (e.g. a " +
		"git-worktree session writing into the main checkout), pass an absolute path under the workspace instead."
}

// Reports whether a path contains a literal parent directory component
func hasTraversalComponent(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}
