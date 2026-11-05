package pm

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var cacheDistInfo = regexp.MustCompile(`^([A-Za-z0-9_.]+?)-\p{Nd}[^-]*\.dist-info`)

// Normalized dist names a project's uv.lock can resolve
func LockPackageNames(sourceRepo string) (map[string]bool, error) {
	text, err := readUTF8(filepath.Join(sourceRepo, "uv.lock"))
	if err != nil {
		return nil, err
	}
	data, err := tomlLoads(text)
	if err != nil {
		return nil, err
	}
	listed, err := tomlIndex(data, "package")
	if err != nil {
		return nil, err
	}
	rows, err := tomlIter(listed)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, row := range rows {
		name, err := tomlIndex(row, "name")
		if err != nil {
			return nil, err
		}
		s, ok := name.(string)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("expected string or bytes-like object, got '%s'", tomlTypeName(name))}
		}
		names[pep503Name(s)] = true
	}
	return names, nil
}

// Entries a glob pattern matches under root, sorted
func globUnder(root, pattern string) []string {
	matches, _ := filepath.Glob(filepath.Join(globEscape(root), pattern))
	sort.Strings(matches)
	return matches
}

// Walks a tree the way os walk does, following only the top, calling visit for each non directory entry
func walkFiles(top string, visit func(path string) error) error {
	entries, err := os.ReadDir(top)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		path := filepath.Join(top, entry.Name())
		isDir := entry.IsDir()
		if entry.Type()&fs.ModeSymlink != 0 {
			st, err := os.Stat(path)
			isDir = err == nil && st.IsDir()
		}
		if isDir {
			if entry.Type()&fs.ModeSymlink == 0 && !IsJunction(path) {
				dirs = append(dirs, path)
			}
			continue
		}
		if err := visit(path); err != nil {
			return err
		}
	}
	for _, dir := range dirs {
		if err := walkFiles(dir, visit); err != nil {
			return err
		}
	}
	return nil
}

// Deletes wheel and sdist links whose archive bucket is gone and returns how many
func dropDanglingLinks(cache string) (int, error) {
	dropped := 0
	families := append(globUnder(cache, "wheels-v*"), globUnder(cache, "sdists-v*")...)
	for _, family := range families {
		err := walkFiles(family, func(path string) error {
			st, err := os.Lstat(path)
			if err != nil || st.Mode()&fs.ModeSymlink == 0 {
				return nil
			}
			if _, err := os.Stat(path); err == nil {
				return nil
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			dropped++
			return nil
		})
		if err != nil {
			return dropped, err
		}
	}
	return dropped, nil
}

// Deletes cache entries for dists the lock cannot resolve and returns the pruned entry count
func PruneUVCacheToLock(cache, sourceRepo string) (int, error) {
	keep, err := LockPackageNames(sourceRepo)
	if err != nil {
		return 0, err
	}
	distName := func(bucket string) (string, bool) {
		for _, marker := range globUnder(bucket, "*.dist-info") {
			if m := cacheDistInfo.FindStringSubmatch(filepath.Base(marker)); m != nil {
				return pep503Name(m[1]), true
			}
		}
		return "", false
	}
	pruned := 0
	archive := filepath.Join(cache, "archive-v0")
	if isDirectory(archive) {
		buckets, err := os.ReadDir(archive)
		if err != nil {
			return 0, err
		}
		for _, entry := range buckets {
			bucket := filepath.Join(archive, entry.Name())
			if !isDirectory(bucket) {
				continue
			}
			if name, ok := distName(bucket); ok && !keep[name] {
				removeTreeQuiet(bucket)
				pruned++
			}
		}
	}
	families := append(globUnder(cache, filepath.Join("wheels-v*", "pypi")), globUnder(cache, filepath.Join("sdists-v*", "pypi"))...)
	for _, family := range families {
		if !isDirectory(family) {
			continue
		}
		entries, err := os.ReadDir(family)
		if err != nil {
			return pruned, err
		}
		for _, entry := range entries {
			path := filepath.Join(family, entry.Name())
			if isDirectory(path) && !keep[pep503Name(entry.Name())] {
				removeTreeQuiet(path)
				pruned++
			}
		}
	}
	dropped, err := dropDanglingLinks(cache)
	return pruned + dropped, err
}
