package pyfs

import (
	"os"
	"path/filepath"
	"strings"
)

// os.path.join of a directory and one entry name, the separator added only when missing
func walkJoin(top, name string) string {
	if top == "" || strings.HasSuffix(top, string(filepath.Separator)) || strings.HasSuffix(top, "/") {
		return top + name
	}
	return top + string(filepath.Separator) + name
}

// os.walk top down in scandir order, visit returning the subdirectory names to descend and unreadable directories skipped
func Walk(top string, followlinks bool, visit func(dirpath string, dirnames, filenames []string) []string) {
	f, err := os.Open(top)
	if err != nil {
		return
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil && len(entries) == 0 {
		return
	}
	var dirs, files []string
	for _, e := range entries {
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(walkJoin(top, e.Name()))
			isDir = err == nil && info.IsDir()
		}
		if isDir {
			dirs = append(dirs, e.Name())
		} else {
			files = append(files, e.Name())
		}
	}
	for _, d := range visit(top, dirs, files) {
		next := walkJoin(top, d)
		if !followlinks {
			if info, err := os.Lstat(next); err == nil && info.Mode()&os.ModeSymlink != 0 {
				continue
			}
		}
		Walk(next, followlinks, visit)
	}
}
