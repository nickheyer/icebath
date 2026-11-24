package dlutil

import (
	"os"
	"path/filepath"
)

// Soname or path the platform loader resolves for a short library name such as "opus", empty when none is installed
func FindLibrary(name string) string {
	if name == "" {
		return ""
	}
	return findLibrary(name)
}

// Whether a path names a regular file or a link to one
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Non-empty entries of the directory list an environment variable holds
func envDirs(key string) []string {
	var dirs []string
	for _, dir := range filepath.SplitList(os.Getenv(key)) {
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
