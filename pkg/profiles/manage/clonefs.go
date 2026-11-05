package manage

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Root files a clone edits in place, materialized so a symlinked source is never written through
var cloneMaterialize = []string{".env", "config.yaml", "auth.json", "SOUL.md"}

// Entries never copied out of a profile, caches, sockets, temp files and anything not a file, dir or link
func nonExportableEntries(dir string, names []string) map[string]bool {
	ignored := map[string]bool{}
	for _, entry := range names {
		if entry == "__pycache__" || strings.HasSuffix(entry, ".sock") || strings.HasSuffix(entry, ".tmp") ||
			strings.HasSuffix(entry, ".pyc") || strings.HasSuffix(entry, ".pyo") {
			ignored[entry] = true
			continue
		}
		st, err := os.Lstat(filepath.Join(dir, entry))
		if err != nil {
			ignored[entry] = true
			continue
		}
		if m := st.Mode(); !(m.IsRegular() || m.IsDir() || m&fs.ModeSymlink != 0) {
			ignored[entry] = true
		}
	}
	return ignored
}

// Absolute path with symlinks resolved as far as they exist, like pathlib resolve without strict
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir, base := filepath.Split(abs)
	if dir == abs || base == "" {
		return abs
	}
	return filepath.Join(resolvePath(filepath.Clean(dir)), base)
}

// Copy filter for a full clone, history always skipped and infrastructure skipped when cloning the default home
func cloneAllCopytreeIgnore(sourceDir string) pyfs.IgnoreFunc {
	sourceResolved := resolvePath(sourceDir)
	rootExclude := map[string]bool{}
	for k := range profiles.CloneAllHistoryExcludeRoot {
		rootExclude[k] = true
	}
	if sourceResolved == resolvePath(profiles.DefaultHome()) {
		for k := range profiles.CloneAllDefaultExcludeRoot {
			rootExclude[k] = true
		}
	}
	return func(dir string, names []string) map[string]bool {
		ignored := nonExportableEntries(dir, names)
		if resolvePath(dir) == sourceResolved {
			for _, n := range names {
				if rootExclude[n] {
					ignored[n] = true
				}
			}
		}
		return ignored
	}
}

// Writes text to path unless something already exists there, never failing
func seedFileIfMissing(path, text string, mode os.FileMode) {
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		return
	}
	if mode != 0 {
		_ = os.Chmod(path, mode)
	}
}

// Copies one profile relative file when the source has it, tightening a copied .env to owner only
func cloneFile(sourceDir, profileDir, relpath string) error {
	src := filepath.Join(sourceDir, relpath)
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	dst := filepath.Join(profileDir, relpath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	if err := pyfs.Copy2(src, dst); err != nil {
		return err
	}
	if relpath == ".env" {
		_ = os.Chmod(dst, 0o600)
	}
	return nil
}

// Replaces symlinked root files the clone edits with private copies, dropping dangling links
func materializeSymlinkedFiles(profileDir string) ([]string, error) {
	var done []string
	for _, name := range cloneMaterialize {
		path := filepath.Join(profileDir, name)
		if st, err := os.Lstat(path); err != nil || st.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		target := resolvePath(path)
		if err := os.Remove(path); err != nil {
			return done, err
		}
		if st, err := os.Stat(target); err == nil && st.Mode().IsRegular() {
			if err := pyfs.Copy2(target, path); err != nil {
				return done, err
			}
		}
		done = append(done, name)
	}
	return done, nil
}

// Tree copy keeping links that recreates NTFS junctions as junctions instead of descending into them
func copytreeKeepJunctions(src, dst string, ignore pyfs.IgnoreFunc, dirsExistOK bool) error {
	junctions := map[string]string{}
	var order []string
	wrapped := func(dir string, names []string) map[string]bool {
		ignored := ignore(dir, names)
		for _, name := range names {
			if target, ok := junctionTarget(filepath.Join(dir, name)); ok {
				junctions[filepath.Join(dir, name)] = target
				order = append(order, filepath.Join(dir, name))
				ignored[name] = true
			}
		}
		return ignored
	}
	if err := pyfs.CopyTree(src, dst, true, dirsExistOK, wrapped); err != nil {
		return err
	}
	for _, link := range order {
		rel, err := filepath.Rel(src, link)
		if err != nil {
			return err
		}
		if err := createJunction(junctions[link], filepath.Join(dst, rel)); err != nil {
			slog.Warn("clone: skipped junction " + link + " -> " + junctions[link] + " (" + pyfs.OSErrorText(err) + ")")
		}
	}
	return nil
}

// Drops multiplex_profiles from a cloned config since multiplexing belongs to the default profile
func stripMultiplexFlag(ctx context.Context, configPath string) error {
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil || !exists {
		return err
	}
	cfg, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		return nil
	}
	gateway, isMap := cfg["gateway"].(map[string]any)
	if !isMap {
		gateway = map[string]any{}
	}
	_, top := cfg["multiplex_profiles"]
	_, nested := gateway["multiplex_profiles"]
	if !top && !nested {
		return nil
	}
	delete(cfg, "multiplex_profiles")
	delete(gateway, "multiplex_profiles")
	if _, ok := cfg["gateway"]; ok && len(gateway) == 0 {
		delete(cfg, "gateway")
	}
	_ = config.AtomicConfigReplace(ctx, filepath.Dir(configPath), configPath, cfg, "")
	return nil
}

// Copy filter for a cloned plugins tree that also skips in flight installer staging dirs at the root
func clonePluginsIgnore(pluginsRoot string) pyfs.IgnoreFunc {
	return func(dir string, names []string) map[string]bool {
		ignored := nonExportableEntries(dir, names)
		if dir == pluginsRoot {
			for _, n := range names {
				if strings.HasPrefix(n, ".install-") || strings.HasPrefix(n, ".update-") {
					if st, err := os.Stat(filepath.Join(dir, n)); err == nil && st.IsDir() {
						ignored[n] = true
					}
				}
			}
		}
		return ignored
	}
}

// Copies the source user installed plugins so a catalog memory provider still resolves in the clone
func clonePlugins(sourceDir, profileDir string) error {
	src := filepath.Join(sourceDir, "plugins")
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return nil
	}
	return copytreeKeepJunctions(src, filepath.Join(profileDir, "plugins"), clonePluginsIgnore(src), true)
}

// Sorted names of the plugins a clone carries, hidden entries skipped, empty when unreadable
func ClonedPluginNames(profileDir string) []string {
	root := filepath.Join(profileDir, "plugins")
	entries, err := os.ReadDir(root)
	if err != nil {
		return []string{}
	}
	out := []string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(root, e.Name())
		st, statErr := os.Stat(p)
		lst, lstatErr := os.Lstat(p)
		if (statErr == nil && st.IsDir()) || (lstatErr == nil && lst.Mode()&fs.ModeSymlink != 0) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
