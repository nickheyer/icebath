package backup

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
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
	excludedNames         = setOf(".backup.lock", ".config.yaml.lock", ".env.lock", "gateway.pid", "cron.pid")
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
	_, parts := pathutil.Parts(rel)
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
	name := pathutil.Name(rel)
	return excludedNames[name] || hasAnyPrefix(name, excludedPrefixes) || hasAnySuffix(name, excludedSuffixes)
}

// Reports whether a path is a symlink, socket, device or other non regular entry, false when lstat fails
func isNonRegularPath(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && !info.Mode().IsRegular()
}

// Absolute and home relative path of one file a full backup holds
type backupFile struct{ abs, rel string }

// Every file a full backup holds in lexical walk order, recording pruned directories when asked; symlinked directories are neither descended nor listed
func iterBackupFiles(root, outPath string, skippedDirs map[string]bool) []backupFile {
	var out []backupFile
	outResolved, outErr := pathutil.Resolve(outPath)
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		name := d.Name()
		dir := d.IsDir()
		if !dir && d.Type()&fs.ModeSymlink != 0 {
			st, serr := os.Stat(p)
			dir = serr == nil && st.IsDir()
		}
		if dir {
			if (!excludedDirs[name] || (name == "icebath-agent" && filepath.Dir(rel) != ".")) && !inExcludedRootDir(rel) {
				return nil
			}
			if skippedDirs != nil {
				skippedDirs[rel] = true
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if shouldExclude(rel) || isNonRegularPath(p) {
			return nil
		}
		if outErr == nil {
			if resolved, err := pathutil.Resolve(p); err == nil && resolved == outResolved {
				return nil
			}
		}
		out = append(out, backupFile{abs: p, rel: rel})
		return nil
	})
	return out
}
