package backup

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Directory names a backup walk never descends, the codebase checkout pruned at the root only
var excludedDirs = setOf(
	"icebath-agent", "__pycache__", ".git", "node_modules", "backups", quickSnapshotsDir, "checkpoints",
	"browser-profiles", "browser-profile", ".venv", "venv", "site-packages",
	".cache", ".tox", ".nox", ".pytest_cache", ".mypy_cache", ".ruff_cache",
)

// Regenerable runtime trees at a profile home root, the package manager cache handled per subdirectory
var excludedRootDirs = func() map[string]bool {
	out := map[string]bool{}
	for n := range home.LocalRuntimeRootDirs {
		out[n] = true
	}
	for n := range profiles.PMRuntimeRootDirs {
		if n != "cache" {
			out[n] = true
		}
	}
	return out
}()

// Browser profiles and secret value caches at a home root, never written into a full backup
var excludedBackupRootDirs = func() map[string]bool {
	out := setOf("browser_profiles")
	for _, store := range profiles.SecretCacheStores() {
		if !strings.Contains(store, "/") {
			out[store] = true
		}
	}
	return out
}()

// Durable cache subdirectories a backup keeps while the rest of the cache is regenerable
var keptCacheSubdirs = setOf("images", "audio", "videos", "documents", "screenshots", "citations", tools.GeneratedSubdir)

var (
	sqliteSidecarSuffixes = []string{".db-wal", ".db-shm", ".db-journal"}
	excludedSuffixes      = append([]string{".pyc", ".pyo"}, sqliteSidecarSuffixes...)
	excludedNames         = setOf(".backup.lock", "gateway.pid", "cron.pid")
	excludedPrefixes      = []string{emergencyPrefix, "state.db" + state.RetiredGenerationDirSuffix}
)

// Set of names for membership tests
func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Reports whether a name starts with any prefix
func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Reports whether a name ends with any suffix
func hasAnySuffix(name string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// Path parts of a relative path as pathlib splits them
func relParts(rel string) []string {
	_, parts := pypath.PathParts(rel)
	return parts
}

// Home root entry a relative path lives under, the entry inside a named profile for profile paths
func profileRootEntry(parts []string) (string, bool) {
	if len(parts) >= 3 && parts[0] == "profiles" {
		return parts[2], true
	}
	if len(parts) > 0 {
		return parts[0], true
	}
	return "", false
}

// Reports whether a relative path lies inside a regenerable tree at a profile home root
func inExcludedRootDir(rel string) bool {
	parts := relParts(rel)
	if entry, ok := profileRootEntry(parts); ok && (excludedRootDirs[entry] || excludedBackupRootDirs[entry]) {
		return true
	}
	if len(parts) >= 3 && parts[0] == "profiles" {
		parts = parts[2:]
	}
	return len(parts) >= 2 && parts[0] == "cache" && !keptCacheSubdirs[parts[1]]
}

// Reports whether a relative path is skipped by a full backup
func shouldExclude(rel string) bool {
	if inExcludedRootDir(rel) {
		return true
	}
	parts := relParts(rel)
	for _, p := range parts {
		if excludedDirs[p] && (p != "icebath-agent" || p == parts[0]) {
			return true
		}
	}
	name := pypath.PathName(rel)
	return excludedNames[name] || hasAnyPrefix(name, excludedPrefixes) || hasAnySuffix(name, excludedSuffixes)
}

// Reports whether a path is a symlink, socket, device or other non regular entry, false when lstat fails
func isNonRegularPath(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && !info.Mode().IsRegular()
}

// Reports whether a directory entry is a symlink or a Windows reparse point, false when lstat fails
func isLinkPath(p string) bool {
	info, err := os.Lstat(p)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info)
}

// Absolute and home relative path of one file a full backup holds
type backupFile struct{ abs, rel string }

// Every file a full backup holds in walk order, recording pruned directories when asked
func iterBackupFiles(root, outPath string, skippedDirs map[string]bool) []backupFile {
	var out []backupFile
	outResolved, outErr := pypath.Resolve(outPath)
	pyfs.Walk(root, false, func(dirpath string, dirnames, filenames []string) []string {
		relDir, _ := pypath.PathRelativeTo(dirpath, root)
		relDir = pypath.PathStr(relDir)
		isRoot := relDir == "."
		join := func(name string) string {
			if isRoot {
				return name
			}
			return filepath.Join(relDir, name)
		}
		var kept []string
		for _, d := range dirnames {
			if (!excludedDirs[d] || (d == "icebath-agent" && !isRoot)) && !inExcludedRootDir(join(d)) {
				kept = append(kept, d)
			} else if skippedDirs != nil {
				skippedDirs[join(d)] = true
			}
		}
		descend := []string{}
		for _, d := range kept {
			if !isLinkPath(pypath.PathJoin(dirpath, d)) {
				descend = append(descend, d)
			}
		}
		for _, fname := range filenames {
			rel := join(fname)
			fpath := filepath.Join(root, rel)
			if shouldExclude(rel) || isNonRegularPath(fpath) {
				continue
			}
			if outErr == nil {
				if resolved, err := pypath.Resolve(fpath); err == nil && resolved == outResolved {
					continue
				}
			}
			out = append(out, backupFile{abs: fpath, rel: rel})
		}
		return descend
	})
	return out
}
