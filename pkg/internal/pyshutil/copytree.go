package pyshutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
)

// Copies the directory tree at src into dst like shutil.copytree, links kept when symlinks, an existing dst merged when dirsExistOK
func CopyTree(src, dst string, symlinks, dirsExistOK bool) error {
	return CopyTreeIgnore(src, dst, symlinks, dirsExistOK, nil)
}

// Ignore callback of CopyTreeIgnore skipping the names matching any glob pattern like shutil.ignore_patterns
func IgnorePatterns(patterns ...string) func(dir string, names []string) []string {
	return func(_ string, names []string) []string {
		var out []string
		for _, pattern := range patterns {
			out = append(out, fnmatch.Filter(names, pattern)...)
		}
		return out
	}
}

// Copies like CopyTree, skipping in each directory the names ignore returns for it
func CopyTreeIgnore(src, dst string, symlinks, dirsExistOK bool, ignore func(dir string, names []string) []string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil && !dirsExistOK {
		return fmt.Errorf("[Errno 17] File exists: '%s'", dst)
	}
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return err
	}
	ignored := map[string]bool{}
	if ignore != nil {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		for _, name := range ignore(src, names) {
			ignored[name] = true
		}
	}
	var failures []error
	for _, entry := range entries {
		if ignored[entry.Name()] {
			continue
		}
		from, to := filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())
		info, err := os.Lstat(from)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0 && symlinks:
			target, err := os.Readlink(from)
			if err == nil {
				err = os.Symlink(target, to)
			}
			if err == nil {
				err = CopyLinkStat(from, to)
			}
			if err != nil {
				failures = append(failures, err)
			}
		case info.Mode()&os.ModeSymlink != 0:
			resolved, err := os.Stat(from)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if resolved.IsDir() {
				err = CopyTreeIgnore(from, to, symlinks, dirsExistOK, ignore)
			} else {
				err = Copy2(from, to)
			}
			if err != nil {
				failures = append(failures, err)
			}
		case info.IsDir():
			if err := CopyTreeIgnore(from, to, symlinks, dirsExistOK, ignore); err != nil {
				failures = append(failures, err)
			}
		default:
			if err := Copy2(from, to); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := copyStat(src, dst); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
