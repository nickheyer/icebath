package state

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// Suffixes SQLite names the WAL and shared memory sidecars of a database with
const (
	walSuffix = "-wal"
	shmSuffix = "-shm"
)

// Suffix a deleted descriptor target carries in procfs
const deletedSuffix = " (deleted)"

// Absolute path with every existing symlink followed and a missing remainder kept as written
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	rest := ""
	for cur := abs; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Absolute path of p without following links, p itself when the working directory is unknown
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// Case folded comparable form on Windows, the path unchanged elsewhere
func normCase(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(filepath.FromSlash(p))
	}
	return p
}

// Comparable absolute form of a descriptor target with the procfs deleted suffix removed
func canonicalSQLitePath(p string) string {
	return normCase(absPath(strings.TrimSuffix(p, deletedSuffix)))
}

// Main database file and the two sidecars SQLite may keep beside it
func sqliteFamily(base string) [3]string {
	return [3]string{base, base + walSuffix, base + shmSuffix}
}

// File URI of the absolute path, percent encoded so a question mark or hash in a home cannot truncate it
func fileURI(path string) string {
	abs := filepath.ToSlash(absPath(path))
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	return (&url.URL{Scheme: "file", Path: abs}).String()
}

// File URI that opens the resolved path read-only, failing instead of creating a missing file
func ReadOnlyURI(path string) string { return fileURI(resolvePath(path)) + "?mode=ro" }
